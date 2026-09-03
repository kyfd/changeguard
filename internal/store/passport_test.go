package store

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

func passportFixture(now time.Time, ruleSetVersion string) (model.ChangeRequest, model.Passport) {
	change := model.ChangeRequest{OrganizationID: "org_demo", ID: "chg_passport_atomic", Title: "atomic gate", ApplicationID: "app_order", Environment: "生产环境", ChangeType: "配置变更", ArtifactSHA256: "artifact-digest", RuleSetVersion: ruleSetVersion, Status: model.StatusApproved, CreatedAt: now, UpdatedAt: now, Version: 1}
	passport := model.Passport{ID: "pass_atomic", OrganizationID: change.OrganizationID, ChangeID: change.ID, ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, RuleSetVersion: change.RuleSetVersion, ApproverID: "usr_reviewer", Status: model.PassportActive, TokenSHA256: "token-hash", IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	return change, passport
}

func TestCreatePassportMaterializesExpiredActiveBeforeReissue(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	change, expired := passportFixture(now.Add(-20*time.Minute), changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo")))
	expired.ExpiresAt = now.Add(-time.Minute)
	expired.ID = "pass_expired_reissue"
	if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_create_reissue", ChangeID: change.ID, CreatedAt: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(expired, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_issue_old", ChangeID: change.ID, CreatedAt: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	fresh := expired
	fresh.ID = "pass_fresh_reissue"
	fresh.IssuedAt = now
	fresh.ExpiresAt = now.Add(10 * time.Minute)
	fresh.TokenSHA256 = "fresh-token-hash"
	if err := data.CreatePassport(fresh, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_issue_fresh", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatalf("expired ACTIVE must not block reissue: %v", err)
	}
	old, err := data.Passport(expired.ID)
	if err != nil || old.Status != model.PassportExpired {
		t.Fatalf("natural expiry was not materialized: passport=%+v err=%v", old, err)
	}
	foundExpiryAudit := false
	for _, event := range data.Audits(0) {
		if event.Action == "PASSPORT_EXPIRED" && event.PassportID == expired.ID && event.ActorType == "SYSTEM" {
			foundExpiryAudit = true
			break
		}
	}
	if !foundExpiryAudit {
		t.Fatal("reissue must audit materialization of the old natural expiry")
	}
	current, err := data.Passport(fresh.ID)
	if err != nil || current.Status != model.PassportActive {
		t.Fatalf("fresh passport not active: passport=%+v err=%v", current, err)
	}
}

func TestUsePassportConcurrentConsumeCompletesChangeOnce(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	rules := changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo"))
	change, passport := passportFixture(now, rules)
	if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_create_atomic", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_issue_atomic", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const workers = 100
	start := make(chan struct{})
	var wait sync.WaitGroup
	var success atomic.Int32
	var unexpected atomic.Int32
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := data.UsePassport(passport.ID, passport.TokenSHA256, "ci-job", now.Add(time.Second), true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: NewID("audit_"), ChangeID: change.ID, CreatedAt: now.Add(time.Second)})
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrPassportInactive) {
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if success.Load() != workers || unexpected.Load() != 0 {
		t.Fatalf("same-consumer consume must replay as one logical success: success=%d unexpected=%d", success.Load(), unexpected.Load())
	}
	gotPassport, err := data.Passport(passport.ID)
	if err != nil || gotPassport.Status != model.PassportConsumed {
		t.Fatalf("passport was not consumed: %+v err=%v", gotPassport, err)
	}
	gotChange, err := data.Change(change.ID)
	if err != nil || gotChange.Status != model.StatusCompleted {
		t.Fatalf("consume must atomically complete change: %+v err=%v", gotChange, err)
	}
	if len(gotChange.Timeline) == 0 || gotChange.Timeline[len(gotChange.Timeline)-1].Status != model.StatusCompleted {
		t.Fatalf("completion timeline missing: %+v", gotChange.Timeline)
	}
	consumeAudits := 0
	for _, event := range data.AuditsByChange(change.OrganizationID, change.ID) {
		if event.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumeAudits++
		}
	}
	if consumeAudits != 1 {
		t.Fatalf("concurrent same-consumer consume must write one audit, got %d", consumeAudits)
	}
}

func TestUsePassportSaveFailureRollsBackPassportAndChange(t *testing.T) {
	now := time.Now().UTC()
	initial := seedState()
	policies := make([]model.RiskPolicy, 0)
	for _, policy := range initial.Policies {
		if policy.OrganizationID == "org_demo" {
			policies = append(policies, policy)
		}
	}
	change, passport := passportFixture(now, changegate.RuleSetVersion(policies))
	initial.Changes = append(initial.Changes, change)
	stored := model.StoredPassport{Passport: passport, TokenSHA256Stored: passport.TokenSHA256}
	stored.Passport.TokenSHA256 = ""
	initial.Passports = append(initial.Passports, stored)
	payload, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("persistence failed")
	data := &Store{data: initial, backend: &failingSaveBackend{payload: payload, version: 9, err: backendErr}, version: 9, persisted: append([]byte(nil), payload...)}
	_, err = data.UsePassport(passport.ID, passport.TokenSHA256, "ci-job", now.Add(time.Second), true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_failed_consume", ChangeID: change.ID, CreatedAt: now.Add(time.Second)})
	if !errors.Is(err, backendErr) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	gotPassport, err := data.Passport(passport.ID)
	if err != nil || gotPassport.Status != model.PassportActive {
		t.Fatalf("failed save leaked consumed passport: %+v err=%v", gotPassport, err)
	}
	gotChange, err := data.Change(change.ID)
	if err != nil || gotChange.Status != model.StatusApproved {
		t.Fatalf("failed save leaked completed change: %+v err=%v", gotChange, err)
	}
}

func TestUsePassportRejectsRuleChangeUnderStoreLock(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	policies := data.PoliciesByOrganization("org_demo")
	change, passport := passportFixture(now, changegate.RuleSetVersion(policies))
	if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_create_rule_race", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_issue_rule_race", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if len(policies) == 0 {
		t.Fatal("expected organization policies")
	}
	if _, err := data.UpdatePolicy(policies[0].ID, func(policy *model.RiskPolicy) error { policy.Enabled = !policy.Enabled; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := data.UsePassport(passport.ID, passport.TokenSHA256, "ci-rule-race", now.Add(time.Second), true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_rule_race", ChangeID: change.ID, CreatedAt: now.Add(time.Second)}); !errors.Is(err, ErrPassportChangeInvalid) {
		t.Fatalf("changed rule set must be rejected inside atomic consume, got %v", err)
	}
	gotPassport, _ := data.Passport(passport.ID)
	gotChange, _ := data.Change(change.ID)
	if gotPassport.Status != model.PassportActive || gotChange.Status != model.StatusApproved {
		t.Fatalf("rejected consume must not mutate state: passport=%s change=%s", gotPassport.Status, gotChange.Status)
	}
}

func TestUsePassportPersistsExpiryAndAudit(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	change, passport := passportFixture(now.Add(-20*time.Minute), changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo")))
	passport.ExpiresAt = now.Add(-time.Minute)
	if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_create_expired", ChangeID: change.ID, CreatedAt: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_issue_expired", ChangeID: change.ID, CreatedAt: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := data.UsePassport(passport.ID, passport.TokenSHA256, "ci-expired", now, false, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_expired", ChangeID: change.ID, CreatedAt: now}); !errors.Is(err, ErrPassportExpired) {
		t.Fatalf("expired passport must be rejected, got %v", err)
	}
	got, err := data.Passport(passport.ID)
	if err != nil || got.Status != model.PassportExpired {
		t.Fatalf("expiry must be persisted, passport=%+v err=%v", got, err)
	}
	foundAudit := false
	for _, audit := range data.Audits(0) {
		if audit.ID == "audit_expired" && audit.Action == "PASSPORT_EXPIRED" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatal("passport expiry audit was not persisted")
	}
}

func TestUsePassportSameConsumerReplayKeepsFirstSnapshot(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	change, passport := passportFixture(now, changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo")))
	if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_create_replay", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_issue_replay", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	firstAt := now.Add(time.Second)
	first, err := data.UsePassport(passport.ID, passport.TokenSHA256, "ci-job", firstAt, true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_first_consume", ChangeID: change.ID, CreatedAt: firstAt})
	if err != nil || first.Status != model.PassportConsumed || first.ConsumedBy != "ci-job" || first.ConsumedAt == nil || !first.ConsumedAt.Equal(firstAt) {
		t.Fatalf("first consume: %+v err=%v", first, err)
	}
	replay, err := data.UsePassport(passport.ID, passport.TokenSHA256, "ci-job", now.Add(2*time.Second), true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_replay_consume", ChangeID: change.ID, CreatedAt: now.Add(2 * time.Second)})
	if err != nil {
		t.Fatalf("same consumer must replay first consume: %v", err)
	}
	if replay.Status != model.PassportConsumed || replay.ConsumedBy != "ci-job" || replay.ConsumedAt == nil || !replay.ConsumedAt.Equal(firstAt) {
		t.Fatalf("replay mutated consume snapshot: first=%+v replay=%+v", first, replay)
	}
	if _, err := data.UsePassport(passport.ID, passport.TokenSHA256, "other-ci", now.Add(3*time.Second), true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_other_consume", ChangeID: change.ID, CreatedAt: now.Add(3 * time.Second)}); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("different consumer must conflict, got %v", err)
	}
	if _, err := data.UsePassport(passport.ID, passport.TokenSHA256, "   ", now.Add(4*time.Second), true, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_blank_consume", ChangeID: change.ID, CreatedAt: now.Add(4 * time.Second)}); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("blank consumer must conflict, got %v", err)
	}
	consumeAudits := 0
	for _, event := range data.AuditsByChange(change.OrganizationID, change.ID) {
		if event.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumeAudits++
		}
	}
	if consumeAudits != 1 {
		t.Fatalf("replay must not write a second consume audit, got %d", consumeAudits)
	}
	gotChange, err := data.Change(change.ID)
	if err != nil || gotChange.Status != model.StatusCompleted || gotChange.Version != 2 {
		t.Fatalf("replay must not change completed snapshot: %+v err=%v", gotChange, err)
	}
}

func TestPassportsByOrganizationScopesAndOmitsTokenHash(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	ownChange, own := passportFixture(now, changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo")))
	own.ID = "pass_org_own"
	own.TokenSHA256 = "own-token-hash"
	if err := data.CreateChange(ownChange, model.AuditEvent{OrganizationID: ownChange.OrganizationID, ID: "audit_create_own", ChangeID: ownChange.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(own, model.AuditEvent{OrganizationID: own.OrganizationID, ID: "audit_issue_own", ChangeID: own.ChangeID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	foreignChange := ownChange
	foreignChange.ID = "chg_foreign"
	foreignChange.OrganizationID = "org_isolated"
	foreign := own
	foreign.ID = "pass_org_foreign"
	foreign.OrganizationID = "org_isolated"
	foreign.ChangeID = foreignChange.ID
	foreign.TokenSHA256 = "foreign-token-hash"
	if err := data.CreateChange(foreignChange, model.AuditEvent{OrganizationID: foreignChange.OrganizationID, ID: "audit_create_foreign", ChangeID: foreignChange.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(foreign, model.AuditEvent{OrganizationID: foreign.OrganizationID, ID: "audit_issue_foreign", ChangeID: foreign.ChangeID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	got := data.PassportsByOrganization("org_demo")
	if len(got) != 1 || got[0].ID != own.ID || got[0].TokenSHA256 != "" {
		t.Fatalf("organization list leaked foreign passport or token hash: %+v", got)
	}
	if empty := data.PassportsByOrganization("org_missing"); len(empty) != 0 {
		t.Fatalf("unknown organization must return empty, got %+v", empty)
	}
}
