package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

const agentChangeSQL = "SET lock_timeout = '3s';\nCREATE INDEX CONCURRENTLY idx_orders_user ON orders (user_id);"
const agentChangeRollback = "DROP INDEX CONCURRENTLY idx_orders_user;"

func agentChangeInput(taskID string) model.CreateChangeInput {
	return model.CreateChangeInput{
		Title: "agent linked", ApplicationID: "app_order", ChangeType: "数据库变更", Environment: "生产环境",
		AgentTaskID: taskID, SQL: agentChangeSQL, RollbackSQL: agentChangeRollback,
		RollbackPlan: "restore prior configuration",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"}},
	}
}

func agentTaskPayload(overrides map[string]any) map[string]any {
	task := map[string]any{
		"task_id": "task_prod_1", "source": "production", "status": "DRAFT_READY",
		"slots": map[string]any{"application": "app_order"},
		"draft": map[string]any{"sql": agentChangeSQL, "rollback_sql": agentChangeRollback},
	}
	for key, value := range overrides {
		task[key] = value
	}
	return task
}

func newAgentTaskStub(t *testing.T, status int, task map[string]any) (*httptest.Server, *http.Header) {
	t.Helper()
	captured := http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(task)
	}))
	t.Cleanup(server.Close)
	return server, &captured
}

func configureAgent(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv(agentBaseURLEnv, baseURL)
	t.Setenv(agentUpstreamToken, "shared-secret")
}

func postChange(t *testing.T, server *Server, input model.CreateChangeInput) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/changes", bytes.NewReader(encoded))
	request.Header.Set("X-Actor-ID", "usr_developer")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

// 来源必须由服务端判定：调用方声明 source 会被 DisallowUnknownFields 直接拒绝。
func TestChangeCreateRejectsClientDeclaredSource(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	body := strings.Replace(configCreateBody("declared source", ""), `"application_id"`, `"source":"evaluation","application_id"`, 1)
	request := httptest.NewRequest(http.MethodPost, "/api/changes", strings.NewReader(body))
	request.Header.Set("X-Actor-ID", "usr_developer")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("client-declared source must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

// 来源不被信任（评测 / 演示产物）时不得进入放行流程：提交、审批与签发通行证都被拒绝。
func TestUntrustedSourceCannotReachReleaseTransitions(t *testing.T) {
	// 每个场景独立存储：发布窗口冲突会让多个变更无法共存于同一组织。
	t.Run("submit", func(t *testing.T) {
		_, svc, data := newIdempotencyHTTPServer(t)
		draft, err := svc.Create(model.CreateChangeInput{
			Title: "source guard submit", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
			Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
			RollbackPlan: "restore prior configuration", ReleasePlan: model.ReleasePlan{Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"}},
		}, "usr_developer")
		if err != nil {
			t.Fatal(err)
		}
		forceSource(t, data, draft.ID, "evaluation")
		if _, err := svc.Submit(draft.ID, "usr_developer"); !errors.Is(err, service.ErrForbidden) || !strings.Contains(err.Error(), "不允许进入生产放行流程") {
			t.Fatalf("submit must refuse an untrusted source: %v", err)
		}
	})

	t.Run("approve", func(t *testing.T) {
		_, svc, data := newIdempotencyHTTPServer(t)
		waiting := readyConfigChange(t, svc, "source guard approve")
		forceSource(t, data, waiting.ID, "demo")
		if _, err := svc.Approve(waiting.ID, "usr_reviewer", "ok"); !errors.Is(err, service.ErrForbidden) || !strings.Contains(err.Error(), "不允许进入生产放行流程") {
			t.Fatalf("approve must refuse an untrusted source: %v", err)
		}
	})

	t.Run("issue passport", func(t *testing.T) {
		t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("h", 32))
		_, svc, data := newIdempotencyHTTPServer(t)
		approved := readyConfigChange(t, svc, "source guard passport")
		approved, err := svc.Approve(approved.ID, "usr_reviewer", "approved")
		if err != nil {
			t.Fatal(err)
		}
		forceSource(t, data, approved.ID, "evaluation")
		if _, err := svc.IssuePassport(approved.ID, "usr_reviewer", 600); !errors.Is(err, service.ErrForbidden) || !strings.Contains(err.Error(), "不允许进入生产放行流程") {
			t.Fatalf("issue passport must refuse an untrusted source: %v", err)
		}
	})
}

// 幂等重放必须保留原始来源，不能被后续请求"刷成"别的来源。
func TestIdempotentReplayKeepsServerDerivedSource(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(nil))
	configureAgent(t, stub.URL)
	create := func() model.ChangeRequest {
		encoded, _ := json.Marshal(agentChangeInput("task_src_1"))
		request := httptest.NewRequest(http.MethodPost, "/api/changes", bytes.NewReader(encoded))
		request.Header.Set("X-Actor-ID", "usr_developer")
		request.Header.Set("Idempotency-Key", "create-key-source-1")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
		}
		var change model.ChangeRequest
		if err := json.Unmarshal(response.Body.Bytes(), &change); err != nil {
			t.Fatal(err)
		}
		return change
	}
	first := create()
	second := create()
	if first.Source != "agent_task" || second.Source != "agent_task" || first.ID != second.ID {
		t.Fatalf("replay must keep the original change and source: first=%+v second=%+v", first, second)
	}
}

func TestChangesCanBeQueriedByAgentTask(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(nil))
	configureAgent(t, stub.URL)
	create := func(taskID string) {
		response := postChange(t, server, agentChangeInput(taskID))
		if response.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
		}
	}
	create("task_alpha")
	create("task_beta")

	list := httptest.NewRequest(http.MethodGet, "/api/changes?agent_task_id=task_alpha", nil)
	list.Header.Set("X-Actor-ID", "usr_developer")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, list)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var changes []model.ChangeRequest
	if err := json.Unmarshal(response.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].AgentTaskID != "task_alpha" {
		t.Fatalf("agent task association query returned %+v", changes)
	}
}

func forceSource(t *testing.T, data *store.Store, changeID, source string) {
	t.Helper()
	if _, err := data.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		item.Source = source
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Agent 关联必须**服务端核对**：仅填写格式合法的 agent_task_id 不足以获得可信来源。
func TestAgentTaskAssociationMustBeVerified(t *testing.T) {
	t.Run("unconfigured agent is refused", func(t *testing.T) {
		server, _, data := newIdempotencyHTTPServer(t)
		response := postChange(t, server, agentChangeInput("task_prod_1"))
		if response.Code != http.StatusConflict || countAgentTaskChanges(data, "task_prod_1") != 0 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("verification carries the acting identity", func(t *testing.T) {
		server, _, _ := newIdempotencyHTTPServer(t)
		stub, headers := newAgentTaskStub(t, http.StatusNotFound, nil)
		configureAgent(t, stub.URL)
		if response := postChange(t, server, agentChangeInput("task_missing")); response.Code != http.StatusConflict {
			t.Fatalf("nonexistent task must be refused: status=%d", response.Code)
		}
		if headers.Get("X-Actor-Id") != "usr_developer" || headers.Get("X-Org-Id") != "org_demo" || strings.TrimSpace(headers.Get("X-Agent-Upstream-Token")) == "" {
			t.Fatalf("核对必须带发起人身份与共享密钥：%+v", *headers)
		}
	})

	t.Run("nonexistent task is refused", func(t *testing.T) {
		server, _, data := newIdempotencyHTTPServer(t)
		stub, _ := newAgentTaskStub(t, http.StatusNotFound, nil)
		configureAgent(t, stub.URL)
		response := postChange(t, server, agentChangeInput("task_missing"))
		if response.Code != http.StatusConflict || countAgentTaskChanges(data, "task_missing") != 0 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	for _, scenario := range []struct {
		name      string
		overrides map[string]any
	}{
		{"evaluation source is refused", map[string]any{"source": "evaluation"}},
		{"demo source is refused", map[string]any{"source": "demo"}},
		{"cross application is refused", map[string]any{"slots": map[string]any{"application": "another_app"}}},
		{"material mismatch is refused", map[string]any{"draft": map[string]any{"sql": "SELECT 1;", "rollback_sql": agentChangeRollback}}},
		{"missing draft is refused", map[string]any{"draft": nil}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server, _, data := newIdempotencyHTTPServer(t)
			stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(scenario.overrides))
			configureAgent(t, stub.URL)
			response := postChange(t, server, agentChangeInput("task_prod_1"))
			if response.Code != http.StatusConflict || countAgentTaskChanges(data, "task_prod_1") != 0 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	t.Run("verified production task is accepted", func(t *testing.T) {
		server, _, data := newIdempotencyHTTPServer(t)
		stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(nil))
		configureAgent(t, stub.URL)
		response := postChange(t, server, agentChangeInput("task_prod_1"))
		if response.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var change model.ChangeRequest
		if err := json.Unmarshal(response.Body.Bytes(), &change); err != nil {
			t.Fatal(err)
		}
		if change.Source != "agent_task" || change.AgentTaskID != "task_prod_1" {
			t.Fatalf("unexpected source/association: %+v", change)
		}
		if countAgentTaskChanges(data, "task_prod_1") != 1 {
			t.Fatal("verified association must be recorded exactly once")
		}
	})
}

// 直接调用服务层时，客户端声明的 agent_task_id 不会被信任。
func TestServiceCreateRejectsDeclaredAgentTaskID(t *testing.T) {
	_, svc, _ := newIdempotencyHTTPServer(t)
	if _, err := svc.Create(agentChangeInput("task_prod_1"), "usr_developer"); !errors.Is(err, service.ErrValidation) {
		t.Fatalf("Create must reject a client-declared agent_task_id: %v", err)
	}
}

// 应用授权核对接口：供 Agent 在按应用访问项目知识前确认权限。
func TestAgentToolsApplicationAuthorization(t *testing.T) {
	t.Setenv(agentToolsSecretEnv, "tools-secret")
	server, _, _ := newIdempotencyHTTPServer(t)
	get := func(applicationID, organization string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/agent-tools/applications/"+applicationID, nil)
		request.Header.Set("X-Agent-Upstream-Token", "tools-secret")
		request.Header.Set("X-Actor-Id", "usr_developer")
		request.Header.Set("X-Org-Id", organization)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	if response := get("app_order", "org_demo"); response.Code != http.StatusOK {
		t.Fatalf("authorized application should be readable: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := get("app_missing", "org_demo"); response.Code != http.StatusForbidden {
		t.Fatalf("unknown application must be forbidden: status=%d", response.Code)
	}
	if response := get("app_order", "org_other"); response.Code != http.StatusForbidden {
		t.Fatalf("mismatched organization must be forbidden: status=%d", response.Code)
	}
	// 缺少共享密钥时失败关闭。
	request := httptest.NewRequest(http.MethodGet, "/api/agent-tools/applications/app_order", nil)
	request.Header.Set("X-Actor-Id", "usr_developer")
	request.Header.Set("X-Org-Id", "org_demo")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing shared secret must fail closed: status=%d", response.Code)
	}
}
