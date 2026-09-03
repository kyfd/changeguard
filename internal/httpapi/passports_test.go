package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

func TestHTTPListPassportsIsOneShotAndOmitsSecrets(t *testing.T) {
	server, svc, data := newIdempotencyHTTPServer(t)
	change := readyConfigChange(t, svc, "list passports")
	now := time.Now().UTC()
	passport := model.Passport{
		ID: "pass_list_http", OrganizationID: change.OrganizationID, ChangeID: change.ID,
		ArtifactSHA256: "artifact-list", Environment: change.Environment,
		RuleSetVersion: changegate.RuleSetVersion(data.PoliciesByOrganization(change.OrganizationID)),
		ApproverID:     "usr_reviewer", Status: model.PassportActive,
		TokenSHA256: "secret-token-hash", IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_list_http", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/passports", nil)
	request.Header.Set("X-Actor-ID", "usr_owner")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "secret-token-hash") || strings.Contains(body, "token_sha256") {
		t.Fatalf("list leaked token material: %s", body)
	}
	var items []model.Passport
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatalf("list decode: %v body=%s", err, body)
	}
	found := false
	for _, item := range items {
		if item.ID == passport.ID {
			found = true
			if item.TokenSHA256 != "" || item.ChangeID != change.ID {
				t.Fatalf("unexpected passport: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("missing seeded passport: %+v", items)
	}

	perChange := httptest.NewRequest(http.MethodGet, "/api/changes/"+change.ID+"/passports", nil)
	perChange.Header.Set("X-Actor-ID", "usr_owner")
	perChangeResp := httptest.NewRecorder()
	server.ServeHTTP(perChangeResp, perChange)
	if perChangeResp.Code != http.StatusOK {
		t.Fatalf("per-change status=%d body=%s", perChangeResp.Code, perChangeResp.Body.String())
	}

	post := httptest.NewRequest(http.MethodPost, "/api/passports", strings.NewReader(`{}`))
	post.Header.Set("X-Actor-ID", "usr_owner")
	denied := httptest.NewRecorder()
	server.ServeHTTP(denied, post)
	if denied.Code != http.StatusMethodNotAllowed {
		t.Fatalf("list endpoint must be GET-only, got %d", denied.Code)
	}
}

func TestHTTPGateConsumeSameConsumerReplay(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("g", 32))
	server, svc, data := newIdempotencyHTTPServer(t)
	change := readyConfigChange(t, svc, "gate consume replay")
	change, err := svc.Approve(change.ID, "usr_reviewer", "approved")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := svc.IssuePassport(change.ID, "usr_reviewer", 0)
	if err != nil {
		t.Fatal(err)
	}
	consume := func(consumer string) *httptest.ResponseRecorder {
		body := `{"artifact_sha256":` + strconvQuote(change.ArtifactSHA256) + `,"environment":` + strconvQuote(change.Environment) + `,"consumer":` + strconvQuote(consumer) + `}`
		request := httptest.NewRequest(http.MethodPost, "/api/gate/consume", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+credential.Token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	first := consume("gitlab-pipeline-1024")
	if first.Code != http.StatusOK || first.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("first consume status=%d replay=%q body=%s", first.Code, first.Header().Get("Idempotency-Replayed"), first.Body.String())
	}
	var firstResult model.GateResult
	if err := json.Unmarshal(first.Body.Bytes(), &firstResult); err != nil || !firstResult.Allowed || firstResult.Code != "GATE_ALLOWED" || firstResult.Passport == nil {
		t.Fatalf("first consume body=%s err=%v", first.Body.String(), err)
	}
	second := consume("gitlab-pipeline-1024")
	if second.Code != http.StatusOK || second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("same-consumer replay status=%d header=%q body=%s", second.Code, second.Header().Get("Idempotency-Replayed"), second.Body.String())
	}
	var replay model.GateResult
	if err := json.Unmarshal(second.Body.Bytes(), &replay); err != nil || !replay.Allowed || replay.Code != "GATE_ALLOWED" || replay.Passport == nil {
		t.Fatalf("replay body=%s err=%v", second.Body.String(), err)
	}
	if replay.Passport.ConsumedBy != "gitlab-pipeline-1024" || firstResult.Passport.ConsumedAt == nil || replay.Passport.ConsumedAt == nil || !replay.Passport.ConsumedAt.Equal(*firstResult.Passport.ConsumedAt) {
		t.Fatalf("replay snapshot drifted: first=%+v replay=%+v", firstResult.Passport, replay.Passport)
	}
	conflict := consume("other-pipeline")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different consumer status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	var denied model.GateResult
	if err := json.Unmarshal(conflict.Body.Bytes(), &denied); err != nil || denied.Allowed || denied.Code != "PASSPORT_REPLAY" {
		t.Fatalf("different consumer body=%s err=%v", conflict.Body.String(), err)
	}
	consumeAudits := 0
	for _, event := range data.AuditsByChange(change.OrganizationID, change.ID) {
		if event.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumeAudits++
		}
	}
	if consumeAudits != 1 {
		t.Fatalf("HTTP replay must not write a second consume audit, got %d", consumeAudits)
	}
}
