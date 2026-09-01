package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestSanitizeLLMErrorClassifiesTypedTimeoutWithoutLeakingURL(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: "https://model.example.com/chat?api_key=must-not-leak",
		Err: context.DeadlineExceeded,
	}
	if got := sanitizeLLMError(err); got != "模型调用超时" {
		t.Fatalf("timeout classification = %q", got)
	}
}

func TestSanitizeLLMErrorRedactsAuthenticationFailure(t *testing.T) {
	got := sanitizeLLMError(errors.New("HTTP 401 invalid API key sk-must-not-leak"))
	if got != "API Key 无效或未授权（HTTP 401），请在企业「接入 AI」中更新密钥" {
		t.Fatalf("authentication classification = %q", got)
	}
}

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
	if result.AdvisoryRisk != model.RiskMedium || result.Risk != result.AdvisoryRisk {
		t.Fatalf("fallback must expose advisory risk and legacy alias, got %#v", result)
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
	if result.Provider != "openai-compatible" || result.AdvisoryRisk != model.RiskHigh || result.Risk != model.RiskHigh || result.Steps != 1 || result.ToolCalls != 3 {
		t.Fatalf("unexpected analysis: %#v", result)
	}
}

func TestAgentLoopRejectsInvalidToolArgumentsWithoutExecuting(t *testing.T) {
	var rounds, executions int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rounds++
		if rounds == 1 {
			message := requiredToolMessage()
			calls := message["tool_calls"].([]map[string]any)
			message["tool_calls"] = append(calls, map[string]any{
				"id": "call_probe", "type": "function",
				"function": map[string]any{"name": "probe", "arguments": `{"value":`},
			})
			writeModelMessage(w, message)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"risk\":\"LOW\",\"summary\":\"证据已读取\",\"reasons\":[\"规则通过\"],\"suggestions\":[\"继续评审\"],\"evidenceIds\":[\"ev_rule\"]}"}}]}`))
	}))
	defer server.Close()

	registry := DefaultToolRegistry()
	registry.Register(Tool{
		Name: "probe", Parameters: emptyObjectSchema(),
		Execute: func(context.Context, model.ChangeRequest, map[string]any, DataSource) (any, error) {
			executions++
			return map[string]any{"ok": true}, nil
		},
	})
	runtime := &Runtime{baseURL: server.URL, apiKey: "test", model: "test", maxTokens: 128, client: server.Client(), mode: "loop", maxRounds: 3, registry: registry}
	result, err := runtime.analyzeWithTools(context.Background(), model.ChangeRequest{
		ID: "chg_invalid_args", Findings: []model.Finding{{ID: "ev_rule", Severity: model.RiskLow}},
	}, LLMConfig{BaseURL: server.URL, APIKey: "test", Model: "test", MaxTokens: 128})
	if err != nil {
		t.Fatalf("analysis failed: %v", err)
	}
	if executions != 0 {
		t.Fatalf("invalid arguments must not execute tool, executions=%d", executions)
	}
	var invalidCall *model.AgentToolCallRecord
	for i := range result.ToolCallLog {
		if result.ToolCallLog[i].Name == "probe" {
			invalidCall = &result.ToolCallLog[i]
			break
		}
	}
	if invalidCall == nil || !strings.Contains(invalidCall.Error, "工具参数无效") {
		t.Fatalf("invalid arguments must be auditable, log=%+v", result.ToolCallLog)
	}
}

func TestAgentLoopValidatesToolParameterSchemaBeforeExecution(t *testing.T) {
	tests := []struct {
		name       string
		arguments  string
		parameters map[string]any
		wantCalls  int
		wantLimit  int64
	}{
		{
			name: "valid integer limit", arguments: `{"limit":10}`, wantCalls: 1, wantLimit: 10,
			parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "additionalProperties": false},
		},
		{
			name: "string limit", arguments: `{"limit":"10"}`,
			parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "additionalProperties": false},
		},
		{
			name: "fractional limit", arguments: `{"limit":10.5}`,
			parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "additionalProperties": false},
		},
		{
			name: "negative limit", arguments: `{"limit":-1}`,
			parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "additionalProperties": false},
		},
		{
			name: "limit above maximum", arguments: `{"limit":21}`,
			parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "additionalProperties": false},
		},
		{name: "unknown field in empty schema", arguments: `{"unexpected":1}`, parameters: emptyObjectSchema()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rounds, executions int
			var observedLimit int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rounds++
				if rounds == 1 {
					message := requiredToolMessage()
					calls := message["tool_calls"].([]map[string]any)
					message["tool_calls"] = append(calls, map[string]any{
						"id": "call_probe", "type": "function",
						"function": map[string]any{"name": "probe", "arguments": tt.arguments},
					})
					writeModelMessage(w, message)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"risk\":\"LOW\",\"summary\":\"证据已读取\",\"reasons\":[\"规则通过\"],\"suggestions\":[\"继续评审\"],\"evidenceIds\":[\"ev_rule\"]}"}}]}`))
			}))
			defer server.Close()

			registry := DefaultToolRegistry()
			registry.Register(Tool{
				Name: "probe", Parameters: tt.parameters,
				Execute: func(_ context.Context, _ model.ChangeRequest, args map[string]any, _ DataSource) (any, error) {
					executions++
					if raw, ok := args["limit"].(json.Number); ok {
						observedLimit, _ = raw.Int64()
					}
					return map[string]any{"ok": true}, nil
				},
			})
			runtime := &Runtime{baseURL: server.URL, apiKey: "test", model: "test", maxTokens: 128, client: server.Client(), mode: "loop", maxRounds: 3, registry: registry}
			result, err := runtime.analyzeWithTools(context.Background(), model.ChangeRequest{
				ID: "chg_schema_args", Findings: []model.Finding{{ID: "ev_rule", Severity: model.RiskLow}},
			}, LLMConfig{BaseURL: server.URL, APIKey: "test", Model: "test", MaxTokens: 128})
			if err != nil {
				t.Fatalf("analysis failed: %v", err)
			}
			if executions != tt.wantCalls {
				t.Fatalf("tool executions=%d, want %d", executions, tt.wantCalls)
			}
			if tt.wantCalls == 1 && observedLimit != tt.wantLimit {
				t.Fatalf("observed limit=%d, want %d", observedLimit, tt.wantLimit)
			}
			var probeLog *model.AgentToolCallRecord
			for i := range result.ToolCallLog {
				if result.ToolCallLog[i].Name == "probe" {
					probeLog = &result.ToolCallLog[i]
					break
				}
			}
			if probeLog == nil {
				t.Fatalf("missing probe audit log: %+v", result.ToolCallLog)
			}
			if tt.wantCalls == 0 && !strings.Contains(probeLog.Error, "schema") {
				t.Fatalf("invalid schema arguments must be audited: %+v", probeLog)
			}
			if tt.wantCalls == 1 && probeLog.Error != "" {
				t.Fatalf("valid arguments must not record error: %+v", probeLog)
			}
		})
	}
}

type limitRecordingDataSource struct {
	recentLimit int
}

func (*limitRecordingDataSource) Policies(string) []model.RiskPolicy { return nil }
func (d *limitRecordingDataSource) RecentChanges(_, _ string, limit int) []model.ChangeRequest {
	d.recentLimit = limit
	return nil
}
func (*limitRecordingDataSource) Application(string, string) (model.Application, bool) {
	return model.Application{}, false
}

func TestSearchHistoricalAcceptsJSONNumberLimit(t *testing.T) {
	args, err := decodeToolArguments(`{"limit":10}`)
	if err != nil {
		t.Fatal(err)
	}
	ds := &limitRecordingDataSource{}
	if _, err := DefaultToolRegistry().Call(context.Background(), "search_historical_changes", model.ChangeRequest{}, args, ds); err != nil {
		t.Fatalf("valid limit should execute: %v", err)
	}
	if ds.recentLimit != 11 {
		t.Fatalf("data source limit=%d, want requested limit+1 (11)", ds.recentLimit)
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
