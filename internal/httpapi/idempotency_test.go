package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kyfd/changeguard/internal/agent"
	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/experiment"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

type idempotencyRunner struct{}

func (idempotencyRunner) Run(_ context.Context, change model.ChangeRequest) model.ExperimentReport {
	return model.ExperimentReport{Status: "PASSED", Mode: "POSTGRES", RollbackVerified: true, ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: change.RuleSetVersion}
}

type idempotencyAnalyzer struct{}

func (idempotencyAnalyzer) Analyze(_ context.Context, change model.ChangeRequest) model.AgentAnalysis {
	return model.AgentAnalysis{Provider: "test", Risk: change.Risk}
}

var _ experiment.Runner = idempotencyRunner{}
var _ agent.Analyzer = idempotencyAnalyzer{}

func newIdempotencyHTTPServer(t *testing.T) (*Server, *service.Service, *store.Store) {
	t.Helper()
	data := store.NewMemory()
	svc := service.New(data, idempotencyRunner{}, idempotencyAnalyzer{})
	logger := log.New(io.Discard, "", 0)
	manager := auth.New(auth.Config{Mode: "disabled"}, data, logger)
	return New(svc, manager, logger), svc, data
}

func TestHTTPIdempotencyKeyValidationAndMissingKeyCompatibility(t *testing.T) {
	server, svc, _ := newIdempotencyHTTPServer(t)
	change := readyConfigChange(t, svc, "http validation")

	invalid := httptest.NewRequest(http.MethodPost, "/api/changes/"+change.ID+"/approve", strings.NewReader(`{"Comment":"ok"}`))
	invalid.Header.Set("X-Actor-ID", "usr_reviewer")
	invalid.Header.Set("Idempotency-Key", "bad key")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, invalid)
	if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_IDEMPOTENCY_KEY" {
		t.Fatalf("invalid key status=%d body=%s", response.Code, response.Body.String())
	}

	missing := httptest.NewRequest(http.MethodPost, "/api/changes/"+change.ID+"/approve", strings.NewReader(`{"Comment":"ok"}`))
	missing.Header.Set("X-Actor-ID", "usr_reviewer")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, missing)
	if response.Code != http.StatusOK || response.Header().Get("Idempotency-Status") != "not-requested" {
		t.Fatalf("missing key must remain compatible and explicit: status=%d header=%q body=%s", response.Code, response.Header().Get("Idempotency-Status"), response.Body.String())
	}
}

func TestHTTPIdempotencyReplayAndDigestConflict(t *testing.T) {
	server, svc, data := newIdempotencyHTTPServer(t)
	change := readyConfigChange(t, svc, "http replay")
	request := func(comment string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/changes/"+change.ID+"/approve", strings.NewReader(`{"Comment":`+strconvQuote(comment)+`}`))
		r.Header.Set("X-Actor-ID", "usr_reviewer")
		r.Header.Set("Idempotency-Key", "approve-key-0001")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	first := request("same")
	second := request("same")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || second.Header().Get("Idempotency-Replayed") != "true" || first.Body.String() != second.Body.String() {
		t.Fatalf("replay mismatch first=%d/%s second=%d/%s header=%q", first.Code, first.Body.String(), second.Code, second.Body.String(), second.Header().Get("Idempotency-Replayed"))
	}
	conflict := request("different")
	if conflict.Code != http.StatusConflict || responseCode(t, conflict) != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Fatalf("digest conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	count := 0
	for _, event := range data.Audits(0) {
		if event.ChangeID == change.ID && event.Action == "APPROVE" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("replay wrote %d approval audits, want 1", count)
	}
}

func TestHTTPPassportIdempotentRetryDoesNotReplayToken(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("h", 32))
	server, svc, data := newIdempotencyHTTPServer(t)
	change := readyConfigChange(t, svc, "http passport")
	change, err := svc.Approve(change.ID, "usr_reviewer", "approved")
	if err != nil {
		t.Fatal(err)
	}
	issue := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/changes/"+change.ID+"/passports", strings.NewReader(`{"ttl_seconds":600}`))
		r.Header.Set("X-Actor-ID", "usr_reviewer")
		r.Header.Set("Idempotency-Key", "passport-key-0001")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	first := issue()
	var credential model.PassportCredential
	if first.Code != http.StatusCreated || json.Unmarshal(first.Body.Bytes(), &credential) != nil || credential.Token == "" {
		t.Fatalf("first issue status=%d body=%s", first.Code, first.Body.String())
	}
	second := issue()
	if second.Code != http.StatusOK || second.Header().Get("Idempotency-Replayed") != "true" || strings.Contains(second.Body.String(), credential.Token) {
		t.Fatalf("safe replay status=%d header=%q body=%s", second.Code, second.Header().Get("Idempotency-Replayed"), second.Body.String())
	}
	var safe service.PassportIssueResult
	if err := json.Unmarshal(second.Body.Bytes(), &safe); err != nil || safe.Code != service.PassportAlreadyIssuedCode || safe.Passport == nil || safe.Passport.ID != credential.Passport.ID || safe.Credential != nil {
		t.Fatalf("unexpected safe retry result: %+v err=%v", safe, err)
	}
	if len(data.PassportsByChange(change.OrganizationID, change.ID)) != 1 {
		t.Fatal("passport retry issued a second credential")
	}
}

func configCreateBody(title, agentTaskID string) string {
	payload, _ := json.Marshal(model.CreateChangeInput{
		Title: title, ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		AgentTaskID:  agentTaskID,
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "restore prior configuration",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"}},
	})
	return string(payload)
}

func TestHTTPChangeCreateIdempotentReplayConflictAndSource(t *testing.T) {
	server, _, data := newIdempotencyHTTPServer(t)
	create := func(title, agentTaskID, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/changes", strings.NewReader(configCreateBody(title, agentTaskID)))
		r.Header.Set("X-Actor-ID", "usr_developer")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}

	first := create("repeat create", "", "create-key-0001")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	var created model.ChangeRequest
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Source != "console" || created.AgentTaskID != "" {
		t.Fatalf("unexpected source/association: %+v", created)
	}

	// 重复点击 / 超时重试：返回同一个变更单，并标记为重放。
	second := create("repeat create", "", "create-key-0001")
	var replayedChange model.ChangeRequest
	_ = json.Unmarshal(second.Body.Bytes(), &replayedChange)
	if second.Code != http.StatusCreated || second.Header().Get("Idempotency-Replayed") != "true" || replayedChange.ID != created.ID {
		t.Fatalf("replay status=%d header=%q body=%s", second.Code, second.Header().Get("Idempotency-Replayed"), second.Body.String())
	}

	// 同一幂等键用于不同请求体 → 冲突，不新建。
	conflict := create("different body", "", "create-key-0001")
	if conflict.Code != http.StatusConflict || responseCode(t, conflict) != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Fatalf("digest conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	// Agent 任务关联必须先由服务端核对：没有配置变更准备服务时拒绝，且不落库。
	unverified := create("agent authored", "task_abc_123", "create-key-0002")
	if unverified.Code != http.StatusConflict {
		t.Fatalf("unverifiable agent task must be refused: status=%d body=%s", unverified.Code, unverified.Body.String())
	}
	if got := countAgentTaskChanges(data, "task_abc_123"); got != 0 {
		t.Fatalf("refused association created %d changes, want 0", got)
	}

	// 无幂等键保持兼容，并显式标注未请求幂等。
	compatible := create("no key", "", "")
	if compatible.Code != http.StatusCreated || compatible.Header().Get("Idempotency-Status") != "not-requested" {
		t.Fatalf("missing key compatibility status=%d header=%q", compatible.Code, compatible.Header().Get("Idempotency-Status"))
	}
}

func TestChangeCreateIdempotencySurvivesProcessRestart(t *testing.T) {
	_, svc, data := newIdempotencyHTTPServer(t)
	input := model.CreateChangeInput{
		Title: "restart safe", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		AgentTaskID:  "task_restart_1",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "restore prior configuration",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"}},
	}
	// agent_task_id 由服务端核对后传入（这里模拟核对通过），Client 声明的字段必须与之一致。
	first, replayed, err := svc.CreateIdempotent(input, "usr_developer", "create-key-restart-1", "digest-restart", "task_restart_1")
	if err != nil || replayed {
		t.Fatalf("first create replayed=%v err=%v", replayed, err)
	}
	// 进程重启：用同一份持久化存储构造新的服务实例，重试同一幂等键。
	restarted := service.New(data, idempotencyRunner{}, idempotencyAnalyzer{})
	second, replayed, err := restarted.CreateIdempotent(input, "usr_developer", "create-key-restart-1", "digest-restart", "task_restart_1")
	if err != nil || !replayed || second.ID != first.ID {
		t.Fatalf("restart retry replayed=%v err=%v first=%s second=%s", replayed, err, first.ID, second.ID)
	}
	if got := countAgentTaskChanges(data, "task_restart_1"); got != 1 {
		t.Fatalf("restart produced %d changes, want 1", got)
	}
}

func countAgentTaskChanges(data *store.Store, taskID string) int {
	count := 0
	for _, change := range data.Changes() {
		if change.AgentTaskID == taskID {
			count++
		}
	}
	return count
}

func readyConfigChange(t *testing.T, svc *service.Service, title string) model.ChangeRequest {
	t.Helper()
	change, err := svc.Create(model.CreateChangeInput{
		Title: title, ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "restore prior configuration", ReleasePlan: model.ReleasePlan{Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil || change.Status != model.StatusWaitingApproval {
		t.Fatalf("prepare change=%+v err=%v", change, err)
	}
	return change
}

func responseCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Code
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
