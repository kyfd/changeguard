package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestIdempotencyFencesOldExecutorAfterAbortAndReclaim(t *testing.T) {
	data := NewMemory()
	base := model.IdempotencyRecord{OrganizationID: "org_demo", ActorID: "usr_developer", Operation: "APPROVE", Resource: "chg_fence", Key: "fence-key-01", RequestDigest: "digest"}
	claimA, created, err := data.BeginIdempotency(base)
	if err != nil || !created {
		t.Fatalf("claim A created=%t err=%v", created, err)
	}
	if claimA.ClaimToken == "" || claimA.Generation != 1 {
		t.Fatalf("invalid first claim: %+v", claimA)
	}
	if err := data.AbortIdempotency(claimA); err != nil {
		t.Fatal(err)
	}
	claimB, created, err := data.BeginIdempotency(base)
	if err != nil || !created {
		t.Fatalf("claim B created=%t err=%v", created, err)
	}
	if claimB.ClaimToken == claimA.ClaimToken {
		t.Fatal("recreated claim reused token")
	}
	if _, err := data.CompleteIdempotency(claimA, map[string]string{"executor": "A"}, "change:chg_fence", 200); !errors.Is(err, ErrIdempotencyClaimLost) {
		t.Fatalf("old executor complete=%v", err)
	}
	if err := data.AbortIdempotency(claimA); !errors.Is(err, ErrIdempotencyClaimLost) {
		t.Fatalf("old executor abort=%v", err)
	}
	if _, err := data.CompleteIdempotency(claimB, map[string]string{"executor": "B"}, "change:chg_fence", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CompleteIdempotency(claimB, map[string]string{"executor": "different"}, "change:chg_fence", 200); !errors.Is(err, ErrIdempotencyResultConflict) {
		t.Fatalf("changed successful result=%v", err)
	}
}

func TestConcurrentTakeoverAllowsOnlyNewGeneration(t *testing.T) {
	data := NewMemory()
	base := model.IdempotencyRecord{OrganizationID: "org_demo", ActorID: "usr_developer", Operation: "QUEUE_EXPERIMENT", Resource: "chg_takeover", Key: "takeover-key", RequestDigest: "digest"}
	old, _, err := data.BeginIdempotency(base)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var wg sync.WaitGroup
	winners := make(chan model.IdempotencyRecord, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := data.TakeoverIdempotency(publicIdempotencyRecord(old))
			if err == nil {
				winners <- claim
			} else if !errors.Is(err, ErrIdempotencyClaimLost) {
				t.Errorf("takeover: %v", err)
			}
		}()
	}
	wg.Wait()
	close(winners)
	claims := make([]model.IdempotencyRecord, 0, 1)
	for claim := range winners {
		claims = append(claims, claim)
	}
	if len(claims) != 1 || claims[0].Generation != 2 {
		t.Fatalf("takeover winners=%+v", claims)
	}
	if _, err := data.CompleteIdempotency(old, map[string]string{"old": "true"}, "change:chg_takeover", 200); !errors.Is(err, ErrIdempotencyClaimLost) {
		t.Fatalf("old generation complete=%v", err)
	}
	if _, err := data.CompleteIdempotency(claims[0], map[string]string{"new": "true"}, "change:chg_takeover", 200); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyMutationsRollBackWhenPersistenceFails(t *testing.T) {
	initial := seedState()
	payload, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("persistence failed")
	data := &Store{data: initial, backend: &failingSaveBackend{payload: payload, version: 4, err: backendErr}, version: 4, persisted: payload}
	record := model.IdempotencyRecord{OrganizationID: "org_demo", ActorID: "usr_developer", Operation: "APPROVE", Resource: "chg", Key: "failure-0001", RequestDigest: "digest"}
	if _, _, err := data.BeginIdempotency(record); !errors.Is(err, backendErr) {
		t.Fatalf("begin error=%v", err)
	}
	if len(data.data.IdempotencyRecords) != 0 {
		t.Fatal("failed begin leaked pending record")
	}

	data.backend = nil
	claim, _, err := data.BeginIdempotency(record)
	if err != nil {
		t.Fatal(err)
	}
	before := data.data.IdempotencyRecords[0]
	data.backend = &failingSaveBackend{payload: payload, version: 4, err: backendErr}
	if _, err := data.CompleteIdempotency(claim, map[string]string{"ok": "yes"}, "change:chg", 200); !errors.Is(err, backendErr) {
		t.Fatalf("complete error=%v", err)
	}
	after := data.data.IdempotencyRecords[0]
	if after.Status != before.Status || after.CompletedAt != nil || len(after.Result) != 0 {
		t.Fatalf("failed complete leaked mutation: %+v", after)
	}
	if err := data.AbortIdempotency(claim); !errors.Is(err, backendErr) {
		t.Fatalf("abort error=%v", err)
	}
	if len(data.data.IdempotencyRecords) != 1 || data.data.IdempotencyRecords[0].Status != IdempotencyPending {
		t.Fatal("failed abort removed pending record")
	}
}

func TestIdempotencyClaimIsAtomicAndPersistent(t *testing.T) {
	data := NewMemory()
	record := model.IdempotencyRecord{OrganizationID: "org_demo", ActorID: "usr_developer", Operation: "QUEUE_EXPERIMENT", Resource: "chg_1", Key: "request-0001", RequestDigest: "digest-a"}
	const workers = 12
	var wg sync.WaitGroup
	claimedRecords := make(chan model.IdempotencyRecord, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, created, err := data.BeginIdempotency(record)
			if err != nil && !errors.Is(err, ErrIdempotencyPending) {
				t.Errorf("claim: %v", err)
			}
			if created {
				claimedRecords <- claim
			}
		}()
	}
	wg.Wait()
	close(claimedRecords)
	claims := make([]model.IdempotencyRecord, 0, 1)
	for claim := range claimedRecords {
		claims = append(claims, claim)
	}
	if len(claims) != 1 {
		t.Fatalf("atomic claim count=%d, want 1", len(claims))
	}
	completed, err := data.CompleteIdempotency(claims[0], map[string]string{"id": "chg_1"}, "change:chg_1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != IdempotencySucceeded || completed.ResponseRef != "change:chg_1" {
		t.Fatalf("unexpected completed record: %+v", completed)
	}
	if _, _, err := data.BeginIdempotency(model.IdempotencyRecord{OrganizationID: record.OrganizationID, ActorID: record.ActorID, Operation: record.Operation, Resource: record.Resource, Key: record.Key, RequestDigest: "digest-b"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different digest must conflict, got %v", err)
	}
}

func TestFileStateLoadsLegacyDataAndPersistsIdempotencyRecords(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "")
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"organizations":[{"id":"org_legacy","name":"Legacy"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := New(path)
	if err != nil {
		t.Fatalf("legacy state must load: %v", err)
	}
	record := model.IdempotencyRecord{OrganizationID: "org_legacy", ActorID: "actor", Operation: "APPROVE", Resource: "chg", Key: "legacy-0001", RequestDigest: "digest"}
	if _, _, err := data.BeginIdempotency(record); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatal(err)
	}
	if len(state["idempotency_records"]) == 0 {
		t.Fatal("idempotency records were not persisted in file/JSONB state payload")
	}
}
