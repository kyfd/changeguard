package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

func TestPostgresNormalizedSQLContracts(t *testing.T) {
	checks := []string{
		"CREATE TABLE IF NOT EXISTS changeguard_outbox",
		"FOR UPDATE SKIP LOCKED",
		"changeguard_passports_one_active_idx",
		"item->>'status' = 'ACTIVE' AND (item->>'expires_at')::timestamptz <= now()",
		"jsonb_set(item, '{status}', '\"EXPIRED\"'::jsonb, true)",
		"PRIMARY KEY (organization_id, actor_id, operation, resource, idempotency_key)",
		"pg_advisory_xact_lock",
		"ON CONFLICT (name) DO NOTHING",
	}
	combined := postgresNormalizedMigration + postgresNormalizedExpirePassports + postgresNormalizedPassportIndexMigration + postgresNormalizedBackfill + postgresNormalizedClaimOutboxSQL + postgresNormalizedAuditLockSQL
	for _, check := range checks {
		if !strings.Contains(combined, check) {
			t.Fatalf("normalized PostgreSQL contract missing %q", check)
		}
	}
}

func TestExpiredActivePassportsMaterializeBeforeProjection(t *testing.T) {
	now := time.Now().UTC()
	data := state{Passports: []model.StoredPassport{
		{Passport: model.Passport{ID: "old-a", OrganizationID: "org", ChangeID: "change", Status: model.PassportActive, IssuedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(-2 * time.Hour)}, TokenSHA256Stored: "a"},
		{Passport: model.Passport{ID: "old-b", OrganizationID: "org", ChangeID: "change", Status: model.PassportActive, IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}, TokenSHA256Stored: "b"},
		{Passport: model.Passport{ID: "new", OrganizationID: "org", ChangeID: "change", Status: model.PassportActive, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}, TokenSHA256Stored: "c"},
	}}
	if !materializeExpiredPassports(&data, now) {
		t.Fatal("expected expired ACTIVE records to be materialized")
	}
	if data.Passports[0].Status != model.PassportExpired || data.Passports[1].Status != model.PassportExpired || data.Passports[2].Status != model.PassportActive {
		t.Fatalf("unexpected statuses: %s %s %s", data.Passports[0].Status, data.Passports[1].Status, data.Passports[2].Status)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(payload), `"status":"ACTIVE"`) != 1 || strings.Count(string(payload), `"status":"EXPIRED"`) != 2 {
		t.Fatalf("legacy witness did not materialize expiry: %s", payload)
	}
}

func TestPostgresNormalizedMultiInstance(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DBGUARD_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("DBGUARD_TEST_POSTGRES_DSN is not set; PostgreSQL multi-instance integration skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cleanup := `DROP TABLE IF EXISTS changeguard_idempotency_records,changeguard_audit_events,changeguard_passports,changeguard_outbox,changeguard_changes,changeguard_normalization_migrations,dbguard_state`
	if _, err = pool.Exec(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), cleanup) })
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "true")

	first, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Repeating the migration is an explicit idempotency requirement.
	if err := first.backend.(*postgresBackend).migrateNormalized(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}

	change := first.data.Changes[0]
	now := time.Now().UTC()
	first.mu.Lock()
	first.data.Outbox = append(first.data.Outbox, model.OutboxEvent{ID: "out_pg_test", OrganizationID: change.OrganizationID, AggregateID: change.ID, AggregateType: "change", EventType: "test", Status: model.OutboxPending, MaxAttempts: 5, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now})
	if err = first.saveLocked(); err != nil {
		first.mu.Unlock()
		t.Fatal(err)
	}
	first.mu.Unlock()
	second, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	var wg sync.WaitGroup
	claims := make(chan error, 2)
	for i, s := range []*Store{first, second} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			_, err := s.ClaimOutbox(string(rune('a'+i)), time.Minute)
			claims <- err
		}(i, s)
	}
	wg.Wait()
	close(claims)
	success, missing := 0, 0
	for err := range claims {
		if err == nil {
			success++
		} else if errors.Is(err, ErrNotFound) {
			missing++
		} else {
			t.Fatalf("claim: %v", err)
		}
	}
	if success != 1 || missing != 1 {
		t.Fatalf("claims success=%d missing=%d", success, missing)
	}

	record := model.IdempotencyRecord{OrganizationID: change.OrganizationID, ActorID: "actor", Operation: "op", Resource: "resource", Key: "same-key", RequestDigest: "digest"}
	results := make(chan error, 2)
	for _, s := range []*Store{first, second} {
		wg.Add(1)
		go func(s *Store) { defer wg.Done(); _, _, err := s.BeginIdempotency(record); results <- err }(s)
	}
	wg.Wait()
	close(results)
	created, pending := 0, 0
	for err := range results {
		if err == nil {
			created++
		} else if errors.Is(err, ErrIdempotencyPending) {
			pending++
		} else {
			t.Fatalf("idempotency: %v", err)
		}
	}
	if created != 1 || pending != 1 {
		t.Fatalf("idempotency created=%d pending=%d", created, pending)
	}

	// Prepare an approved change/passport through the compatibility save, then
	// consume through two independent Store instances.
	first.mu.Lock()
	first.restoreLocked()
	changeIndex := 0
	first.data.Changes[changeIndex].Status = model.StatusApproved
	policies := first.data.Policies
	ruleVersion := changegate.RuleSetVersion(policies)
	first.data.Changes[changeIndex].RuleSetVersion = ruleVersion
	passport := model.StoredPassport{Passport: model.Passport{ID: "passport_pg_test", OrganizationID: change.OrganizationID, ChangeID: change.ID, ArtifactSHA256: first.data.Changes[changeIndex].ArtifactSHA256, Environment: first.data.Changes[changeIndex].Environment, RuleSetVersion: ruleVersion, Status: model.PassportActive, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}, TokenSHA256Stored: "token-hash"}
	first.data.Passports = append(first.data.Passports, passport)
	if err = first.saveLocked(); err != nil {
		first.mu.Unlock()
		t.Fatal(err)
	}
	first.mu.Unlock()
	second.Close()
	second, err = NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	consumes := make(chan error, 2)
	for _, s := range []*Store{first, second} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			_, err := s.UsePassport(passport.ID, "token-hash", "ci", time.Now().UTC(), true, model.AuditEvent{OrganizationID: change.OrganizationID, ActorID: "ci", Action: "CONSUME"})
			consumes <- err
		}(s)
	}
	wg.Wait()
	close(consumes)
	consumed, unexpected := 0, 0
	for err := range consumes {
		if err == nil {
			consumed++
		} else {
			unexpected++
			t.Errorf("same-consumer consume: %v", err)
		}
	}
	if consumed != 2 || unexpected != 0 {
		t.Fatalf("two stores must replay one logical consume: consumed=%d unexpected=%d", consumed, unexpected)
	}
	replayed, err := first.Passport(passport.ID)
	if err != nil || replayed.Status != model.PassportConsumed || replayed.ConsumedBy != "ci" {
		t.Fatalf("postgres consume snapshot: %+v err=%v", replayed, err)
	}
	if _, err := first.UsePassport(passport.ID, "token-hash", "other-ci", time.Now().UTC(), true, model.AuditEvent{OrganizationID: change.OrganizationID, ActorID: "other-ci", Action: "CONSUME"}); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("different consumer must conflict across stores, got %v", err)
	}
	consumeAudits := 0
	for _, event := range first.AuditsByChange(change.OrganizationID, change.ID) {
		if event.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumeAudits++
		}
	}
	if consumeAudits != 1 {
		t.Fatalf("postgres replay must not write a second consume audit, got %d", consumeAudits)
	}

	// Concurrent append is serialized per organization and remains verifiable.
	audits := make(chan error, 2)
	for i, s := range []*Store{first, second} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			audits <- s.RecordAudit(model.AuditEvent{OrganizationID: change.OrganizationID, ID: string(rune('x' + i)), ActorID: "test", Action: "APPEND", CreatedAt: time.Now().UTC()})
		}(i, s)
	}
	wg.Wait()
	close(audits)
	for err := range audits {
		if err != nil {
			t.Fatal(err)
		}
	}
	latest, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if err = latest.VerifyAuditChain(change.OrganizationID); err != nil {
		t.Fatal(err)
	}
}
