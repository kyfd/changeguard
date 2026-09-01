package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/observability"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

func TestOperationsWebhookAuthenticatesLinksAndDeduplicates(t *testing.T) {
	data := store.NewMemory()
	server := &Server{
		service: service.New(data, nil, nil),
		integrations: integration.Config{
			OperationsToken:        "operations-secret",
			OperationsOrganization: "org_demo",
		},
	}
	body := `{"event_id":"servicenow:INC-42:open","source":"servicenow","kind":"INCIDENT","status":"OPEN","change_id":"chg_20260730_001","incident_id":"INC-42","severity":"SEV2","detail":"token: should-not-survive","occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`

	request := httptest.NewRequest(http.MethodPost, "/api/integrations/operations/webhook", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.handleOperationsWebhook(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != `Bearer realm="changeguard-operations"` {
		t.Fatalf("missing token status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/integrations/operations/webhook", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer operations-secret")
	response = httptest.NewRecorder()
	server.handleOperationsWebhook(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("first event status=%d body=%s", response.Code, response.Body.String())
	}
	var first struct {
		Accepted  bool   `json:"accepted"`
		Duplicate bool   `json:"duplicate"`
		ChangeID  string `json:"change_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.Duplicate || first.ChangeID != "chg_20260730_001" {
		t.Fatalf("unexpected first response: %+v", first)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/integrations/operations/webhook", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer operations-secret")
	response = httptest.NewRecorder()
	server.handleOperationsWebhook(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("duplicate event status=%d body=%s", response.Code, response.Body.String())
	}
	var duplicate struct {
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &duplicate); err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate response=%+v err=%v", duplicate, err)
	}
	stored := data.OutcomeSignals("org_demo", 0)
	if len(stored) != 1 {
		t.Fatalf("stored outcome signals=%d, want 1", len(stored))
	}
	if strings.Contains(stored[0].Detail, "should-not-survive") || !strings.Contains(stored[0].Detail, "[REDACTED]") {
		t.Fatalf("event detail was not redacted: %q", stored[0].Detail)
	}
	audits := data.Audits(20)
	foundAudit := false
	for _, audit := range audits {
		if audit.Action == "INCIDENT_LINKED" && audit.ChangeID == "chg_20260730_001" {
			foundAudit = true
			if strings.Contains(audit.Detail, "should-not-survive") {
				t.Fatalf("audit leaked redacted detail: %q", audit.Detail)
			}
		}
	}
	if !foundAudit {
		t.Fatal("incident linkage audit was not recorded")
	}
}

func TestOperationsWebhookRejectsUnlinkedChange(t *testing.T) {
	data := store.NewMemory()
	server := &Server{
		service: service.New(data, nil, nil),
		integrations: integration.Config{
			OperationsToken:        "operations-secret",
			OperationsOrganization: "org_demo",
		},
	}
	body := `{"event_id":"rollback:missing:1","source":"argocd","kind":"ROLLBACK","status":"SUCCEEDED","change_id":"chg_missing","operation_id":"rollback-1","occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/api/integrations/operations/webhook", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer operations-secret")
	response := httptest.NewRecorder()
	server.handleOperationsWebhook(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unlinked change status=%d body=%s", response.Code, response.Body.String())
	}
	if got := len(data.OutcomeSignals("org_demo", 0)); got != 0 {
		t.Fatalf("unlinked event must not be persisted, got %d", got)
	}
}

func TestMetricsExportsLowCardinalityOutcomeAggregates(t *testing.T) {
	t.Setenv("DBGUARD_METRICS_TOKEN", "metrics-secret")
	data := store.NewMemory()
	app := service.New(data, nil, nil)
	now := time.Now().UTC()
	deployedAt := now.Add(-3 * time.Hour)
	_, _, err := app.RecordIntegrationEvent(model.IntegrationEvent{
		OrganizationID: "org_demo", Provider: "JENKINS", ExternalID: "metrics-deploy-1", EventType: "BUILD", Status: "SUCCESS",
		ChangeID: "chg_20260729_003", Project: "orders", Pipeline: "42", OccurredAt: deployedAt, ReceivedAt: deployedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range []model.OutcomeSignal{
		{
			OrganizationID: "org_demo", ExternalID: "rollback-42-success", Source: "ARGOCD", Kind: model.OutcomeSignalRollback,
			Status: "SUCCEEDED", ChangeID: "chg_20260729_003", OperationID: "rollback-42", OccurredAt: now.Add(-2 * time.Hour), ReceivedAt: now.Add(-2 * time.Hour),
		},
		{
			OrganizationID: "org_demo", ExternalID: "incident-42-open", Source: "SERVICENOW", Kind: model.OutcomeSignalIncident,
			Status: "OPEN", ChangeID: "chg_20260729_003", IncidentID: "INC-42", OccurredAt: now.Add(-150 * time.Minute), ReceivedAt: now.Add(-150 * time.Minute),
		},
		{
			OrganizationID: "org_demo", ExternalID: "incident-42-resolved", Source: "SERVICENOW", Kind: model.OutcomeSignalIncident,
			Status: "RESOLVED", ChangeID: "chg_20260729_003", IncidentID: "INC-42", OccurredAt: now.Add(-90 * time.Minute), ReceivedAt: now.Add(-90 * time.Minute),
		},
	} {
		if _, _, err := app.RecordOutcomeSignal(signal); err != nil {
			t.Fatal(err)
		}
	}
	baselineStart, baselineEnd := now.Add(-5*time.Hour), now.Add(-4*time.Hour)
	observationStart, observationEnd := now.Add(-2*time.Hour), now.Add(-time.Hour)
	baseline, observed, objective := 98.0, 99.5, 99.0
	if _, _, err := app.RecordOutcomeSignal(model.OutcomeSignal{
		OrganizationID: "org_demo", ExternalID: "sli-42", Source: "PROMETHEUS", Kind: model.OutcomeSignalBusinessSLI,
		Status: "OBSERVED", ChangeID: "chg_20260729_003", MetricName: "orders_per_minute", MetricUnit: "count", MetricDirection: model.MetricHigherIsBetter,
		BaselineValue: &baseline, ObservedValue: &observed, ObjectiveValue: &objective,
		BaselineWindowStart: &baselineStart, BaselineWindowEnd: &baselineEnd,
		ObservationWindowStart: &observationStart, ObservationWindowEnd: &observationEnd,
		OccurredAt: now.Add(-30 * time.Minute), ReceivedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	server := &Server{service: app, metrics: observability.New()}
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer metrics-secret")
	response := httptest.NewRecorder()
	server.handleMetrics(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		`dbguard_governance_rollback_outcomes{outcome="success"} 1`,
		`dbguard_governance_linked_incidents{state="resolved"} 1`,
		`dbguard_governance_incident_resolution_minutes 60.00`,
		`dbguard_governance_business_sli_outcomes{outcome="improved"} 1`,
		`dbguard_governance_business_objective_attainment_percent 100.00`,
		`dbguard_governance_outcome_signal_observable{signal="business_sli"} 1`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("metrics missing %q\n%s", marker, body)
		}
	}
	for _, forbidden := range []string{"chg_20260729_003", "INC-42", "orders_per_minute"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics must not expose high-cardinality identifier %q", forbidden)
		}
	}
}
