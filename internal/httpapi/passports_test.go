package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
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
