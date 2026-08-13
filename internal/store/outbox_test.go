package store

import (
	"encoding/json"
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
	if err := data.FailOutbox(first.ID, "worker-a", first.LeaseGeneration, errors.New("temporary")); err != nil {
		t.Fatal(err)
	}
	data.mu.Lock()
	data.data.Outbox[0].NextAttemptAt = time.Now().Add(-time.Second)
	data.mu.Unlock()
	second, err := data.ClaimOutbox("worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.FailOutbox(second.ID, "worker-b", second.LeaseGeneration, errors.New("permanent")); err != nil {
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
	if err := data.RenewOutbox(event.ID, "worker-b", event.LeaseGeneration, time.Minute); !errors.Is(err, ErrConcurrentWrite) {
		t.Fatalf("expected ownership error, got %v", err)
	}
	if err := data.RenewOutbox(event.ID, "worker-a", event.LeaseGeneration, time.Minute); err != nil {
		t.Fatal(err)
	}
	renewed := data.OutboxByOrganization(change.OrganizationID, false, 10)[0]
	if renewed.LockedUntil == nil || !renewed.LockedUntil.After(before) {
		t.Fatalf("expected lease extension, before=%v after=%v", before, renewed.LockedUntil)
	}
}

func TestExpiredExperimentLeaseIsFencedAfterNewClaim(t *testing.T) {
	data := NewMemory()
	change := data.Changes()[0]
	_, err := data.UpdateChangeWithOutbox(change.ID, func(item *model.ChangeRequest) error {
		item.Status = model.StatusExperimentQueued
		return nil
	}, model.OutboxEvent{OrganizationID: change.OrganizationID, AggregateType: "change", AggregateID: change.ID, EventType: "experiment.requested", InputSHA256: change.ArtifactSHA256})
	if err != nil {
		t.Fatal(err)
	}
	oldLease, err := data.ClaimOutbox("worker-old", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	newLease, err := data.ClaimOutbox("worker-new", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if newLease.AttemptID == "" || newLease.AttemptID != oldLease.AttemptID {
		t.Fatalf("attempt id must remain stable across takeover: old=%q new=%q", oldLease.AttemptID, newLease.AttemptID)
	}
	if newLease.LeaseGeneration != oldLease.LeaseGeneration+1 {
		t.Fatalf("lease generation must increase monotonically: old=%d new=%d", oldLease.LeaseGeneration, newLease.LeaseGeneration)
	}
	for name, action := range map[string]func() error{
		"renew": func() error {
			return data.RenewOutbox(oldLease.ID, "worker-old", oldLease.LeaseGeneration, time.Minute)
		},
		"complete": func() error { return data.CompleteOutbox(oldLease.ID, "worker-old", oldLease.LeaseGeneration) },
		"fail": func() error {
			return data.FailOutbox(oldLease.ID, "worker-old", oldLease.LeaseGeneration, errors.New("stale"))
		},
		"finalize": func() error {
			_, finalizeErr := data.FinalizeExperimentOutbox(oldLease.ID, "worker-old", oldLease.LeaseGeneration, oldLease.AttemptID, change.ArtifactSHA256, "result", func(*model.ChangeRequest) error { return nil })
			return finalizeErr
		},
	} {
		if err := action(); !errors.Is(err, ErrConcurrentWrite) {
			t.Errorf("stale %s should be fenced, got %v", name, err)
		}
	}
}

func TestExperimentFinalizeIsIdempotentAndInputBound(t *testing.T) {
	data := NewMemory()
	change := data.Changes()[0]
	_, err := data.UpdateChangeWithOutbox(change.ID, func(item *model.ChangeRequest) error {
		item.Status = model.StatusExperimentQueued
		return nil
	}, model.OutboxEvent{OrganizationID: change.OrganizationID, AggregateType: "change", AggregateID: change.ID, EventType: "experiment.requested", InputSHA256: change.ArtifactSHA256})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := data.ClaimOutbox("worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.CheckpointExperimentOutbox(lease.ID, "worker", lease.LeaseGeneration, model.OutboxStageApply, "wrong-input"); !errors.Is(err, ErrConcurrentWrite) {
		t.Fatalf("wrong input digest must be rejected, got %v", err)
	}
	updates := 0
	finalize := func(digest string) error {
		_, finalizeErr := data.FinalizeExperimentOutbox(lease.ID, "worker", lease.LeaseGeneration, lease.AttemptID, change.ArtifactSHA256, digest, func(item *model.ChangeRequest) error {
			updates++
			item.Status = model.StatusWaitingApproval
			return nil
		})
		return finalizeErr
	}
	if err := finalize("result-digest"); err != nil {
		t.Fatal(err)
	}
	if err := finalize("result-digest"); err != nil {
		t.Fatalf("duplicate finalize should be idempotent: %v", err)
	}
	if updates != 1 {
		t.Fatalf("duplicate finalize applied business mutation %d times", updates)
	}
	if err := finalize("different-result"); !errors.Is(err, ErrConcurrentWrite) {
		t.Fatalf("different result for same attempt must be rejected, got %v", err)
	}
	event := data.OutboxByOrganization(change.OrganizationID, true, 10)[0]
	if event.Stage != model.OutboxStageFinalize || event.ResultDigest != "result-digest" || event.StageStartedAt == nil || event.StageUpdatedAt == nil {
		t.Fatalf("finalize checkpoint was not persisted: %+v", event)
	}
}

func TestFailedOutboxPersistenceRestoresClaimMutation(t *testing.T) {
	initial := seedState()
	change := initial.Changes[0]
	now := time.Now()
	initial.Outbox = append(initial.Outbox, model.OutboxEvent{ID: "out_failure", OrganizationID: change.OrganizationID, AggregateType: "change", AggregateID: change.ID, EventType: "experiment.requested", Status: model.OutboxPending, MaxAttempts: 5, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now, InputSHA256: change.ArtifactSHA256})
	payload, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("save failed")
	data := &Store{data: initial, backend: &failingSaveBackend{payload: payload, version: 3, err: backendErr}, version: 3, persisted: payload}
	if _, err := data.ClaimOutbox("worker", time.Minute); !errors.Is(err, backendErr) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	var got model.OutboxEvent
	for _, event := range data.OutboxByOrganization(change.OrganizationID, true, 0) {
		if event.ID == "out_failure" {
			got = event
		}
	}
	if got.Status != model.OutboxPending || got.Attempts != 0 || got.LeaseGeneration != 0 || got.LockedBy != "" || got.AttemptID != "" {
		t.Fatalf("failed claim leaked in-memory mutation: %+v", got)
	}
}
