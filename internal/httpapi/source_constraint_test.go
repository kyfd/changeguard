package httpapi

import (
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
	create := func() model.ChangeRequest {
		request := httptest.NewRequest(http.MethodPost, "/api/changes", strings.NewReader(configCreateBody("replay source", "task_src_1")))
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
	create := func(title, taskID string) {
		request := httptest.NewRequest(http.MethodPost, "/api/changes", strings.NewReader(configCreateBody(title, taskID)))
		request.Header.Set("X-Actor-ID", "usr_developer")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
		}
	}
	create("task A", "task_alpha")
	create("task B", "task_beta")

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
