package store

import (
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestRecordOutcomeSignalIsIdempotentAndOrganizationScoped(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	signal := model.OutcomeSignal{
		OrganizationID: "org_demo", ID: "outcome_1", ExternalID: "incident-1-open", Source: "SERVICENOW",
		Kind: model.OutcomeSignalIncident, Status: "OPEN", ChangeID: "chg_20260730_001", IncidentID: "INC-1",
		OccurredAt: now, ReceivedAt: now,
	}
	audit := model.AuditEvent{OrganizationID: signal.OrganizationID, ID: "aud_outcome_1", ChangeID: signal.ChangeID, CreatedAt: now}
	recorded, created, err := data.RecordOutcomeSignal(signal, audit)
	if err != nil || !created || recorded.ID != signal.ID {
		t.Fatalf("first signal must be recorded: recorded=%+v created=%t err=%v", recorded, created, err)
	}
	duplicate := signal
	duplicate.ID = "outcome_2"
	recorded, created, err = data.RecordOutcomeSignal(duplicate, audit)
	if err != nil || created || recorded.ID != signal.ID {
		t.Fatalf("duplicate signal must return the original: recorded=%+v created=%t err=%v", recorded, created, err)
	}
	otherOrganization := signal
	otherOrganization.OrganizationID = "org_other"
	otherOrganization.ID = "outcome_3"
	if _, created, err = data.RecordOutcomeSignal(otherOrganization, audit); err != nil || !created {
		t.Fatalf("the same source event id in another organization must not collide: created=%t err=%v", created, err)
	}
	if got := len(data.OutcomeSignals("org_demo", 10)); got != 1 {
		t.Fatalf("org_demo signal count=%d, want 1", got)
	}
	if got := len(data.OutcomeSignals("org_other", 10)); got != 1 {
		t.Fatalf("org_other signal count=%d, want 1", got)
	}
}
