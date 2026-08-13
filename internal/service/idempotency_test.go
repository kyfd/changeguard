package service

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

func TestPendingIdempotencyRecordsReconcileWithoutDuplicateEffects(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	queued, err := svc.Create(model.CreateChangeInput{Title: "recover queue", ApplicationID: "app_order", SQL: "CREATE INDEX CONCURRENTLY idx_recover ON orders(status);", RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_recover;"}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	queued, err = svc.Submit(queued.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	queueRecord := model.IdempotencyRecord{OrganizationID: queued.OrganizationID, ActorID: "usr_developer", Operation: "QUEUE_EXPERIMENT", Resource: queued.ID, Key: "recover-queue-01", RequestDigest: "digest"}
	if _, _, err = data.BeginIdempotency(queueRecord); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.QueueExperiment(queued.ID, "usr_developer"); err != nil {
		t.Fatal(err)
	}
	if _, replayed, recoverErr := svc.QueueExperimentIdempotent(queued.ID, "usr_developer", queueRecord.Key, queueRecord.RequestDigest); recoverErr != nil || !replayed {
		t.Fatalf("queue replayed=%t err=%v", replayed, recoverErr)
	}
	outboxCount := 0
	for _, event := range data.OutboxByOrganization(queued.OrganizationID, true, 0) {
		if event.AggregateID == queued.ID && event.EventType == "experiment.requested" {
			outboxCount++
		}
	}
	if outboxCount != 1 {
		t.Fatalf("queue recovery duplicated outbox: %d", outboxCount)
	}

	approved, err := svc.Create(model.CreateChangeInput{
		Title: "recover approve", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "恢复上一版本配置并重新加载服务",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx", "P99 延迟"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	approved, err = svc.Submit(approved.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	approved, err = data.UpdateChange(approved.ID, func(item *model.ChangeRequest) error {
		item.Status = model.StatusWaitingApproval
		item.Risk = model.RiskLow
		item.Findings = nil
		item.SQL = ""
		item.CheckRun = &model.CheckRun{Status: "PASSED", Blocking: 0, ArtifactSHA256: item.ArtifactSHA256, RuleSetVersion: item.RuleSetVersion}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	approveRecord := model.IdempotencyRecord{OrganizationID: approved.OrganizationID, ActorID: "usr_reviewer", Operation: "APPROVE", Resource: approved.ID, Key: "recover-approve", RequestDigest: "digest"}
	if _, _, err = data.BeginIdempotency(approveRecord); err != nil {
		t.Fatal(err)
	}
	if approved, err = svc.Approve(approved.ID, "usr_reviewer", "approved"); err != nil {
		t.Fatal(err)
	}
	if _, replayed, recoverErr := svc.ApproveIdempotent(approved.ID, "usr_reviewer", approveRecord.Key, approveRecord.RequestDigest, "approved"); recoverErr != nil || !replayed {
		t.Fatalf("approve replayed=%t err=%v", replayed, recoverErr)
	}
	auditCount := 0
	for _, event := range data.Audits(0) {
		if event.ChangeID == approved.ID && event.Action == "APPROVE" {
			auditCount++
		}
	}
	if auditCount != 1 {
		t.Fatalf("approve recovery duplicated audit: %d", auditCount)
	}
}

func TestPendingPassportRecordReconcilesWithoutTokenReplay(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("r", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "recover passport", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "恢复上一版本配置并重新加载服务",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx", "P99 延迟"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Approve(change.ID, "usr_reviewer", "approved")
	if err != nil {
		t.Fatal(err)
	}
	record := model.IdempotencyRecord{OrganizationID: change.OrganizationID, ActorID: "usr_reviewer", Operation: "ISSUE_PASSPORT", Resource: change.ID, Key: "recover-passport", RequestDigest: "digest"}
	if _, _, err = data.BeginIdempotency(record); err != nil {
		t.Fatal(err)
	}
	credential, err := svc.IssuePassport(change.ID, "usr_reviewer", 600)
	if err != nil {
		t.Fatal(err)
	}
	recovered, replayed, err := svc.IssuePassportIdempotent(change.ID, "usr_reviewer", record.Key, record.RequestDigest, 600)
	if err != nil || !replayed || recovered.Passport == nil || recovered.Passport.ID != credential.Passport.ID || recovered.Credential != nil || recovered.Code != PassportAlreadyIssuedCode {
		t.Fatalf("passport recovery=%+v replayed=%t err=%v", recovered, replayed, err)
	}
	if len(data.PassportsByChange(change.OrganizationID, change.ID)) != 1 {
		t.Fatal("passport recovery issued another credential")
	}
}

func TestQueueExperimentIdempotencyConcurrentReplayAndConflict(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "idempotent queue", ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "CREATE INDEX CONCURRENTLY idx_idem ON orders(status);", RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_idem;",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 10
	var wg sync.WaitGroup
	results := make(chan model.ChangeRequest, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _, callErr := svc.QueueExperimentIdempotent(change.ID, "usr_developer", "queue-key-0001", "digest-a")
			results <- result
			errs <- callErr
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent replay failed: %v", callErr)
		}
	}
	for result := range results {
		if result.Status != model.StatusExperimentQueued {
			t.Fatalf("unexpected replay status %s", result.Status)
		}
	}
	outbox := data.OutboxByOrganization(change.OrganizationID, true, 0)
	count := 0
	for _, event := range outbox {
		if event.AggregateID == change.ID && event.EventType == "experiment.requested" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("same key queued %d outbox events, want 1", count)
	}
	auditCount := 0
	for _, event := range data.Audits(0) {
		if event.ChangeID == change.ID && event.Action == "QUEUE_EXPERIMENT" {
			auditCount++
		}
	}
	if auditCount != 1 {
		t.Fatalf("same key wrote %d queue audits, want 1", auditCount)
	}
	if _, _, err := svc.QueueExperimentIdempotent(change.ID, "usr_developer", "queue-key-0001", "digest-b"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("digest conflict error=%v", err)
	}
}

func TestIssuePassportIdempotencyNeverReplaysPlaintextToken(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("i", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "idempotent passport", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "restore prior configuration", ReleasePlan: model.ReleasePlan{Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Approve(change.ID, "usr_reviewer", "approved")
	if err != nil {
		t.Fatal(err)
	}
	first, replayed, err := svc.IssuePassportIdempotent(change.ID, "usr_reviewer", "passport-key-01", "digest", 600)
	if err != nil || replayed || first.Credential == nil || first.Credential.Token == "" {
		t.Fatalf("first issuance must return token once: result=%+v replayed=%t err=%v", first, replayed, err)
	}
	second, replayed, err := svc.IssuePassportIdempotent(change.ID, "usr_reviewer", "passport-key-01", "digest", 600)
	if err != nil || !replayed || second.Passport == nil {
		t.Fatalf("retry must return safe reference: result=%+v replayed=%t err=%v", second, replayed, err)
	}
	if second.Credential != nil || second.Code != PassportAlreadyIssuedCode || second.Passport.ID != first.Credential.Passport.ID {
		t.Fatalf("retry exposed or changed credential: %+v", second)
	}
	if strings.Contains(string(second.Passport.ID), first.Credential.Token) {
		t.Fatal("token leaked into safe retry result")
	}
	passports := data.PassportsByChange(change.OrganizationID, change.ID)
	if len(passports) != 1 {
		t.Fatalf("retry issued %d passports, want 1", len(passports))
	}
}
