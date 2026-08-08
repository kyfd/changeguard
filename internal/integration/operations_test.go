package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestVerifyOperationsRequiresStrictBearerToken(t *testing.T) {
	config := Config{OperationsToken: "operations-secret"}
	for _, value := range []string{"", "Token operations-secret", "Bearer", "Bearer operations-secret extra"} {
		headers := http.Header{"Authorization": []string{value}}
		if err := VerifyOperations(config, headers); err != ErrUnauthorized {
			t.Fatalf("authorization %q should be rejected, got %v", value, err)
		}
	}
	headers := http.Header{"Authorization": []string{"bearer operations-secret"}}
	if err := VerifyOperations(config, headers); err != nil {
		t.Fatalf("valid bearer token was rejected: %v", err)
	}
}

func TestParseOperationsBusinessSLI(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	baselineStart, baselineEnd := now.Add(-4*time.Hour), now.Add(-3*time.Hour)
	observationStart, observationEnd := now.Add(-2*time.Hour), now.Add(-time.Hour)
	baseline, observed, objective, tolerance := 98.1, 99.3, 99.0, 0.1
	payload := OperationsPayload{
		EventID: "prometheus:chg-1:checkout_success:20260808", Source: "prometheus", Kind: model.OutcomeSignalBusinessSLI,
		ChangeID: "chg-1", MetricName: "checkout_success_rate", MetricUnit: "percent", MetricDirection: model.MetricHigherIsBetter,
		BaselineValue: &baseline, ObservedValue: &observed, ObjectiveValue: &objective, Tolerance: &tolerance,
		BaselineWindowStart: &baselineStart, BaselineWindowEnd: &baselineEnd,
		ObservationWindowStart: &observationStart, ObservationWindowEnd: &observationEnd,
		OccurredAt: now.Add(-30 * time.Minute),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	signal, err := ParseOperations(Config{OperationsOrganization: "org_demo"}, body, now)
	if err != nil {
		t.Fatalf("valid business SLI was rejected: %v", err)
	}
	if signal.OrganizationID != "org_demo" || signal.Source != "PROMETHEUS" || signal.Status != "OBSERVED" || signal.MetricName != payload.MetricName || signal.BaselineWindowStart == nil || !signal.BaselineWindowStart.Equal(baselineStart) {
		t.Fatalf("unexpected signal: %+v", signal)
	}
}

func TestParseOperationsRejectsUnknownFieldsAndInvalidWindowOrder(t *testing.T) {
	now := time.Now().UTC()
	unknown := []byte(`{"event_id":"e1","source":"ops","kind":"INCIDENT","status":"OPEN","change_id":"chg-1","incident_id":"INC-1","occurred_at":"` + now.Format(time.RFC3339) + `","secret":"must-not-be-stored"}`)
	if _, err := ParseOperations(Config{OperationsOrganization: "org_demo"}, unknown, now); err != ErrInvalidPayload {
		t.Fatalf("unknown fields must be rejected, got %v", err)
	}
	baselineStart, baselineEnd := now.Add(-4*time.Hour), now.Add(-time.Hour)
	observationStart, observationEnd := now.Add(-2*time.Hour), now.Add(-30*time.Minute)
	baseline, observed := 1.0, 2.0
	payload := OperationsPayload{
		EventID: "e2", Source: "ops", Kind: model.OutcomeSignalBusinessSLI, ChangeID: "chg-1",
		MetricName: "orders", MetricUnit: "count", MetricDirection: model.MetricHigherIsBetter,
		BaselineValue: &baseline, ObservedValue: &observed,
		BaselineWindowStart: &baselineStart, BaselineWindowEnd: &baselineEnd,
		ObservationWindowStart: &observationStart, ObservationWindowEnd: &observationEnd,
		OccurredAt: now,
	}
	body, _ := json.Marshal(payload)
	if _, err := ParseOperations(Config{OperationsOrganization: "org_demo"}, body, now); err != ErrInvalidPayload {
		t.Fatalf("overlapping baseline and observation windows must be rejected, got %v", err)
	}
}

func TestParseOperationsRejectsUnsupportedOrStaleEvents(t *testing.T) {
	now := time.Now().UTC()
	payload := OperationsPayload{EventID: "e1", Source: "ops", Kind: "UNKNOWN", Status: "OPEN", ChangeID: "chg-1", OccurredAt: now}
	body, _ := json.Marshal(payload)
	if _, err := ParseOperations(Config{OperationsOrganization: "org_demo"}, body, now); err != ErrUnsupported {
		t.Fatalf("unsupported kind should be explicit, got %v", err)
	}
	payload.Kind, payload.IncidentID, payload.OccurredAt = model.OutcomeSignalIncident, "INC-1", now.Add(-operationsEventMaxAge-time.Hour)
	body, _ = json.Marshal(payload)
	if _, err := ParseOperations(Config{OperationsOrganization: "org_demo"}, body, now); err != ErrReplay {
		t.Fatalf("stale events must be rejected, got %v", err)
	}
}
