package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

// 回归：真实模型会把 query_policies 返回的 pol_ 策略 id、search_historical_changes
// 返回的 chg_ 历史单号写进 evidenceIds。它们来自工具，不应让整次分析降级；
// 但工具从未返回过的编号仍必须 fail closed。
func TestAgentLoopToleratesToolContextReferencesButRejectsForged(t *testing.T) {
	tests := []struct {
		name        string
		evidenceIDs string
		wantErr     string
		wantIDs     []string
	}{
		{name: "policy and history ids from tools are dropped", evidenceIDs: `[\"ev_rule\",\"pol_real\",\"chg_hist\"]`, wantIDs: []string{"ev_rule"}},
		{name: "forged id still rejected", evidenceIDs: `[\"ev_rule\",\"pol_forged\"]`, wantErr: "不存在的证据编号"},
		{name: "only context ids leaves no evidence", evidenceIDs: `[\"pol_real\"]`, wantErr: "缺少证据编号"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rounds := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rounds++
				if rounds == 1 {
					message := requiredToolMessage()
					calls := message["tool_calls"].([]map[string]any)
					message["tool_calls"] = append(calls,
						map[string]any{"id": "call_pol", "type": "function", "function": map[string]any{"name": "policies_stub", "arguments": `{}`}},
						map[string]any{"id": "call_hist", "type": "function", "function": map[string]any{"name": "history_stub", "arguments": `{}`}},
					)
					writeModelMessage(w, message)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"risk\":\"HIGH\",\"summary\":\"全表删除\",\"reasons\":[\"命中策略\"],\"suggestions\":[\"补充 WHERE\"],\"evidenceIds\":` + tt.evidenceIDs + `}"}}]}`))
			}))
			defer server.Close()

			registry := DefaultToolRegistry()
			registry.Register(Tool{Name: "policies_stub", Parameters: emptyObjectSchema(),
				Execute: func(context.Context, model.ChangeRequest, map[string]any, DataSource) (any, error) {
					return map[string]any{"hits": []map[string]any{{"id": "pol_real", "code": "DELETE_WITHOUT_WHERE"}}, "evidence_ids": []string{"ev_rule"}}, nil
				}})
			registry.Register(Tool{Name: "history_stub", Parameters: emptyObjectSchema(),
				Execute: func(context.Context, model.ChangeRequest, map[string]any, DataSource) (any, error) {
					return map[string]any{"count": 1, "items": []map[string]any{{"id": "chg_hist", "title": "历史变更"}}}, nil
				}})
			runtime := &Runtime{baseURL: server.URL, apiKey: "test", model: "test", maxTokens: 128, client: server.Client(), mode: "loop", maxRounds: 3, registry: registry}
			result, err := runtime.analyzeWithTools(context.Background(), model.ChangeRequest{
				ID: "chg_ctx", Findings: []model.Finding{{ID: "ev_rule", Severity: model.RiskHigh, Blocking: true}},
			}, LLMConfig{BaseURL: server.URL, APIKey: "test", Model: "test", MaxTokens: 128})

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("analysis failed: %v", err)
			}
			if result.Provider != "openai-compatible-agent" {
				t.Fatalf("provider=%q, want openai-compatible-agent", result.Provider)
			}
			if strings.Join(result.EvidenceIDs, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("evidence ids=%v, want %v", result.EvidenceIDs, tt.wantIDs)
			}
		})
	}
}

// 回归：多轮模型调用各自都在单次超时内，但累计超过总时限时，Analyze 必须按总时限降级，
// 不能把 submit 请求拖过 HTTP WriteTimeout。
func TestAnalyzeRespectsTotalTimeoutAcrossRounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		writeModelMessage(w, requiredToolMessage())
	}))
	defer server.Close()
	runtime := &Runtime{baseURL: server.URL, apiKey: "test", model: "test", maxTokens: 128, client: server.Client(),
		mode: "loop", maxRounds: 10, registry: DefaultToolRegistry(), totalTimeout: 700 * time.Millisecond, dailyLimit: 100, usage: map[string]dailyUsage{}}
	started := time.Now()
	result := runtime.Analyze(context.Background(), model.ChangeRequest{
		ID: "chg_slow", OrganizationID: "org", SubmitterID: "u", Findings: []model.Finding{{ID: "ev_rule", Severity: model.RiskHigh, Blocking: true}},
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("analysis ignored total timeout, took %s", elapsed)
	}
	if result.Provider != "rules-fallback" {
		t.Fatalf("slow multi-round analysis must degrade to fallback, got %q", result.Provider)
	}
}
