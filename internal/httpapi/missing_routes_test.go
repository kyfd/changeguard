package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestChangeOutcomesAndPolicyExportUseRecordedData(t *testing.T) {
	server, _, data := newIdempotencyHTTPServer(t)
	change := readyConfigChange(t, server.service, "recorded outcomes")
	empty := httptest.NewRequest(http.MethodGet, "/api/changes/"+change.ID+"/outcomes", nil)
	empty.Header.Set("X-Actor-ID", "usr_owner")
	emptyResponse := httptest.NewRecorder()
	server.ServeHTTP(emptyResponse, empty)
	if emptyResponse.Code != http.StatusOK || !strings.Contains(emptyResponse.Body.String(), `"status":"NOT_RUN"`) {
		t.Fatalf("empty outcomes status=%d body=%s", emptyResponse.Code, emptyResponse.Body.String())
	}

	now := time.Now().UTC()
	if _, _, err := data.RecordOutcomeSignal(model.OutcomeSignal{
		OrganizationID: change.OrganizationID, ID: "outcome_test", ExternalID: "ops-1",
		Source: "operations", Kind: model.OutcomeSignalKind("SLI"), Status: "WARN",
		ChangeID: change.ID, Detail: "真实发布后延迟升高", OccurredAt: now, ReceivedAt: now,
	}, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_outcome_test", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	recorded := httptest.NewRequest(http.MethodGet, "/api/changes/"+change.ID+"/outcomes", nil)
	recorded.Header.Set("X-Actor-ID", "usr_owner")
	recordedResponse := httptest.NewRecorder()
	server.ServeHTTP(recordedResponse, recorded)
	if recordedResponse.Code != http.StatusOK || !strings.Contains(recordedResponse.Body.String(), `"status":"WARN"`) || !strings.Contains(recordedResponse.Body.String(), "真实发布后延迟升高") {
		t.Fatalf("recorded outcomes status=%d body=%s", recordedResponse.Code, recordedResponse.Body.String())
	}

	export := httptest.NewRequest(http.MethodGet, "/api/policies/export", nil)
	export.Header.Set("X-Actor-ID", "usr_owner")
	exportResponse := httptest.NewRecorder()
	server.ServeHTTP(exportResponse, export)
	if exportResponse.Code != http.StatusOK || !strings.Contains(exportResponse.Header().Get("Content-Disposition"), "dbguard-risk-policies.json") {
		t.Fatalf("export status=%d headers=%v", exportResponse.Code, exportResponse.Header())
	}
	var payload struct {
		Policies []model.RiskPolicy `json:"policies"`
	}
	if err := json.Unmarshal(exportResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Policies) == 0 {
		t.Fatal("expected organization policies")
	}
	for _, policy := range payload.Policies {
		if policy.OrganizationID != change.OrganizationID {
			t.Fatalf("exported another organization: %+v", policy)
		}
	}
}

func TestOutcomesRejectForeignOrganization(t *testing.T) {
	server, _, data := newIdempotencyHTTPServer(t)
	seedChange(t, data, "chg_foreign_outcomes", "app_order", "org_other")
	req := httptest.NewRequest(http.MethodGet, "/api/changes/chg_foreign_outcomes/outcomes", nil)
	req.Header.Set("X-Actor-ID", "usr_owner")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
		t.Fatalf("foreign outcomes status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUsageUnavailableAndCapabilitiesFailClosed(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request := httptest.NewRequest(method, "/api/enterprise/llm/usage", nil)
		request.Header.Set("X-Actor-ID", "usr_owner")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		want := http.StatusServiceUnavailable
		if method == http.MethodPost {
			want = http.StatusMethodNotAllowed
		}
		if response.Code != want {
			t.Fatalf("usage %s status=%d body=%s", method, response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/config/status", nil)
	request.Header.Set("X-Actor-ID", "usr_owner")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var config struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || config.Capabilities["enterprise_outbound_api"] || config.Capabilities["agent_runtime_api"] {
		t.Fatalf("unsupported capability enabled: status=%d capabilities=%v", response.Code, config.Capabilities)
	}
}
