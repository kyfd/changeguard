package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestFallbackAnalysisUsesEvidence(t *testing.T) {
	t.Setenv("DBGUARD_LLM_API_KEY", "")
	change := model.ChangeRequest{
		ID: "chg_test", Risk: model.RiskMedium,
		Findings:   []model.Finding{{ID: "ev_rule", Severity: model.RiskMedium, Title: "索引创建未使用 CONCURRENTLY", Suggestion: "改用并发索引"}},
		Experiment: &model.ExperimentReport{Status: "PASSED", RollbackVerified: true, FinishedAt: time.Now(), Evidence: []model.Evidence{{ID: "ev_exp", Title: "回滚验证"}}},
	}
	result := NewFromEnvironment().Analyze(context.Background(), change)
	if result.Provider != "rules-fallback" {
		t.Fatalf("expected fallback provider, got %s", result.Provider)
	}
	if len(result.EvidenceIDs) != 2 {
		t.Fatalf("expected evidence ids, got %#v", result.EvidenceIDs)
	}
}

func TestDailyAnalysisLimit(t *testing.T) {
	runtime := &Runtime{dailyLimit: 1, usage: make(map[string]dailyUsage)}
	if !runtime.reserve("usr_test") {
		t.Fatal("first analysis should be allowed")
	}
	if runtime.reserve("usr_test") {
		t.Fatal("second analysis should be blocked by daily limit")
	}
	if !runtime.reserve("usr_other") {
		t.Fatal("limit should be isolated by submitter")
	}
}

func TestAgentRejectsInvalidJSONConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{}"}}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`))
	}))
	defer server.Close()
	runtime := &Runtime{baseURL: server.URL, apiKey: "test", model: "test", maxTokens: 128, client: server.Client(), mode: "oneshot"}
	_, err := runtime.analyzeWithTools(context.Background(), model.ChangeRequest{
		ID: "chg_agent", Findings: []model.Finding{{ID: "ev_rule"}},
	}, LLMConfig{BaseURL: server.URL, APIKey: "test", Model: "test", MaxTokens: 128})
	if err == nil {
		t.Fatal("empty model conclusion must be rejected")
	}
	if !strings.Contains(err.Error(), "无效风险") && !strings.Contains(err.Error(), "为空") && !strings.Contains(err.Error(), "解析") {
		t.Fatalf("expected parse/validation error, got %v", err)
	}
}

func TestAgentOneShotAcceptsValidConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// 单次补全：不应再下发 tools，利于缓存与省 token
		if strings.Contains(string(body), `"tools"`) {
			http.Error(w, "tools should not be sent in one-shot mode", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"risk\":\"HIGH\",\"summary\":\"存在明文密钥，阻断上线\",\"reasons\":[\"配置密钥\"],\"suggestions\":[\"改用密钥托管\"],\"evidenceIds\":[\"ev_rule\"]}"}}],"usage":{"prompt_tokens":200,"completion_tokens":40,"prompt_cache_hit_tokens":150,"prompt_cache_miss_tokens":50}}`))
	}))
	defer server.Close()
	runtime := &Runtime{baseURL: server.URL, apiKey: "test", model: "deepseek-chat", maxTokens: 128, client: server.Client(), mode: "oneshot", registry: DefaultToolRegistry()}
	result, err := runtime.analyzeWithTools(context.Background(), model.ChangeRequest{
		ID: "chg_agent", Findings: []model.Finding{{ID: "ev_rule", Title: "密钥", Severity: model.RiskHigh}},
	}, LLMConfig{BaseURL: server.URL, APIKey: "test", Model: "deepseek-chat", MaxTokens: 128})
	if err != nil {
		t.Fatalf("valid one-shot analysis failed: %v", err)
	}
	if result.Provider != "openai-compatible" || result.Risk != model.RiskHigh || result.Steps != 1 || result.ToolCalls != 3 {
		t.Fatalf("unexpected analysis: %#v", result)
	}
}

func TestAgentEvidenceReferencesMustExist(t *testing.T) {
	change := model.ChangeRequest{
		Findings:   []model.Finding{{ID: "ev_rule"}},
		Experiment: &model.ExperimentReport{Evidence: []model.Evidence{{ID: "ev_exp"}}},
	}
	ok := model.AgentAnalysis{EvidenceIDs: []string{"ev_rule", "ev_exp"}}
	if err := validateEvidenceReferences(ok, change); err != nil {
		t.Fatalf("valid evidence was rejected: %v", err)
	}
	// 幻觉 ID 会被过滤并回填可用证据，不应整体失败
	recovered := model.AgentAnalysis{EvidenceIDs: []string{"ev_missing", "chg_fake"}}
	if err := normalizeEvidenceReferences(&recovered, change); err != nil {
		t.Fatalf("hallucinated ids should be normalized, got %v", err)
	}
	if len(recovered.EvidenceIDs) == 0 {
		t.Fatal("expected backfilled evidence ids")
	}
	strict := model.AgentAnalysis{EvidenceIDs: []string{"ev_rule", "ev_forged"}}
	if err := validateEvidenceReferencesStrict(&strict, change, nil); err == nil || !strings.Contains(err.Error(), "不存在的证据编号") {
		t.Fatalf("agent loop must fail closed on forged evidence, got %v", err)
	}
	emptyChange := model.ChangeRequest{}
	if err := normalizeEvidenceReferences(&model.AgentAnalysis{EvidenceIDs: []string{"x"}}, emptyChange); err == nil {
		t.Fatal("no available evidence must fail")
	}
}
func TestRuntimeGlobalDailyLimitCapsMultipleUsers(t *testing.T) {
	runtime := &Runtime{dailyLimit: 10, globalLimit: 2, usage: make(map[string]dailyUsage)}
	if !runtime.reserve("usr_one") || !runtime.reserve("usr_two") {
		t.Fatal("first two reservations should be allowed")
	}
	if runtime.reserve("usr_three") {
		t.Fatal("global daily limit must cap aggregate model usage")
	}
}

func TestRuntimeDistributedLimitFailsClosedWithoutRedis(t *testing.T) {
	runtime := &Runtime{
		dailyLimit: 5, globalLimit: 20, limitRequired: true,
		usage: make(map[string]dailyUsage),
	}
	if runtime.reserve("user-a") {
		t.Fatal("distributed quota mode without Redis must not call the paid model")
	}
}

func TestRuntimeOrganizationDailyLimitCapsMultipleUsers(t *testing.T) {
	runtime := &Runtime{
		dailyLimit: 10, orgLimit: 2, globalLimit: 20,
		usage: make(map[string]dailyUsage),
	}
	if !runtime.reserveContext(context.Background(), "org-a", "user-a") {
		t.Fatal("first organization reservation should be allowed")
	}
	if !runtime.reserveContext(context.Background(), "org-a", "user-b") {
		t.Fatal("second organization reservation should be allowed")
	}
	if runtime.reserveContext(context.Background(), "org-a", "user-c") {
		t.Fatal("organization daily limit must cap multiple users")
	}
	if !runtime.reserveContext(context.Background(), "org-b", "user-c") {
		t.Fatal("another organization should retain its own quota")
	}
}
