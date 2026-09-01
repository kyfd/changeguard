package store

import (
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestRecordIntegrationEventIsIdempotent(t *testing.T) {
	data := NewMemory()
	event := model.IntegrationEvent{
		OrganizationID: "org_demo",
		ID:             "int_1", Provider: "GITLAB", ExternalID: "evt_1",
		EventType: "PIPELINE", Status: "SUCCESS", ReceivedAt: time.Now(),
	}
	audit := model.AuditEvent{
		OrganizationID: "org_demo", ID: "aud_1",
		ActorID: "integration_gitlab", ActorName: "GitLab CI",
		Action: "GITLAB_PIPELINE_RECEIVED", CreatedAt: event.ReceivedAt,
	}
	recorded, created, err := data.RecordIntegrationEvent(event, audit)
	if err != nil || !created {
		t.Fatalf("first event must be recorded: created=%t err=%v", created, err)
	}
	if recorded.ID != "int_1" {
		t.Fatalf("recorded event id = %q, want int_1", recorded.ID)
	}
	duplicate := event
	duplicate.ID = "int_2"
	recorded, created, err = data.RecordIntegrationEvent(duplicate, audit)
	if err != nil || created {
		t.Fatalf("duplicate event must be ignored: created=%t err=%v", created, err)
	}
	if recorded.ID != "int_1" {
		t.Fatalf("duplicate returned event id = %q, want original int_1", recorded.ID)
	}
	if got := len(data.IntegrationEvents("org_demo", 10)); got != 1 {
		t.Fatalf("stored event count = %d, want 1", got)
	}
	otherOrganization := event
	otherOrganization.OrganizationID = "org_other"
	otherOrganization.ID = "int_3"
	if _, created, err = data.RecordIntegrationEvent(otherOrganization, audit); err != nil || !created {
		t.Fatalf("event ids must be isolated by organization: created=%t err=%v", created, err)
	}
}
