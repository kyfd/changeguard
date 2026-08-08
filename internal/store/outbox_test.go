package store

import (
	"errors"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestOutboxRetryAndDeadLetter(t *testing.T) {
	data := NewMemory()
	change := data.Changes()[0]
	_, err := data.UpdateChangeWithOutbox(change.ID, func(item *model.ChangeRequest) error {
		item.Status = model.StatusExperimentQueued
		return nil
	}, model.OutboxEvent{OrganizationID: change.OrganizationID, AggregateType: "change", AggregateID: change.ID, EventType: "experiment.requested", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := data.ClaimOutbox("worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.FailOutbox(first.ID, "worker-a", errors.New("temporary")); err != nil {
		t.Fatal(err)
	}
	data.mu.Lock()
	data.data.Outbox[0].NextAttemptAt = time.Now().Add(-time.Second)
	data.mu.Unlock()
	second, err := data.ClaimOutbox("worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.FailOutbox(second.ID, "worker-b", errors.New("permanent")); err != nil {
		t.Fatal(err)
	}
	events := data.OutboxByOrganization(change.OrganizationID, false, 10)
	if len(events) != 1 || events[0].Status != model.OutboxDead {
		t.Fatalf("expected dead letter, got %#v", events)
	}
	if err := data.RetryOutbox(change.OrganizationID, events[0].ID, model.AuditEvent{
		ActorID: "usr_owner", ActorName: "owner", Action: "RETRY_OUTBOX", Detail: "人工重试死信",
	}); err != nil {
		t.Fatal(err)
	}
	audits := data.AuditsByOrganization(change.OrganizationID, 20)
	foundAudit := false
	for _, audit := range audits {
		if audit.Action == "RETRY_OUTBOX" {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatal("manual dead-letter retry must create an audit event")
	}
	if retried := data.OutboxByOrganization(change.OrganizationID, false, 10)[0]; retried.Status != model.OutboxPending || retried.Attempts != 0 {
		t.Fatalf("expected retried event, got %#v", retried)
	}
}

func TestOutboxLeaseCanBeRenewedOnlyByOwner(t *testing.T) {
	data := NewMemory()
	change := data.Changes()[0]
	_, err := data.UpdateChangeWithOutbox(change.ID, func(item *model.ChangeRequest) error {
		item.Status = model.StatusExperimentQueued
		return nil
	}, model.OutboxEvent{OrganizationID: change.OrganizationID, AggregateType: "change", AggregateID: change.ID, EventType: "experiment.requested", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	event, err := data.ClaimOutbox("worker-a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	before := *event.LockedUntil
	if err := data.RenewOutbox(event.ID, "worker-b", time.Minute); !errors.Is(err, ErrConcurrentWrite) {
		t.Fatalf("expected ownership error, got %v", err)
	}
	if err := data.RenewOutbox(event.ID, "worker-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	renewed := data.OutboxByOrganization(change.OrganizationID, false, 10)[0]
	if renewed.LockedUntil == nil || !renewed.LockedUntil.After(before) {
		t.Fatalf("expected lease extension, before=%v after=%v", before, renewed.LockedUntil)
	}
}
