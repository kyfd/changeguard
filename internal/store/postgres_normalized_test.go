package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
		"pg_advisory_xact_lock(726384722)",
		"AUTHORITATIVE",
		"ON CONFLICT (name) DO NOTHING",
	}
	combined := postgresMigrationLockSQL + postgresNormalizedMigration + postgresNormalizedExpirePassports + postgresNormalizedPassportIndexMigration + postgresNormalizedBackfill + postgresNormalizedAuthoritySQL + postgresNormalizedClaimOutboxSQL + postgresNormalizedAuditLockSQL
	for _, check := range checks {
		if !strings.Contains(combined, check) {
			t.Fatalf("normalized PostgreSQL contract missing %q", check)
		}
	}
}

func TestPostgresHotPathsDecoupledFromLegacyState(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "postgres_normalized.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	hotPaths := map[string]bool{
		"createChange": true, "updateChange": true, "updateChangeWithOutbox": true,
		"ensureExperimentOutbox": true, "claimOutbox": true, "renewOutbox": true,
		"completeOutbox": true, "failOutbox": true, "checkpointExperimentOutbox": true,
		"finalizeExperimentOutbox": true, "retryOutbox": true,
		"beginIdempotency": true, "completeIdempotency": true,
		"abortIdempotency": true, "takeoverIdempotency": true, "cleanupExpiredIdempotency": true,
		"appendAudit": true, "createPassport": true, "usePassport": true,
		"revokePassport": true, "revokePassportsByChange": true,
	}
	seen := make(map[string]bool, len(hotPaths))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !hotPaths[fn.Name.Name] {
			continue
		}
		seen[fn.Name.Name] = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				if ident.Name == "loadStateForUpdate" || ident.Name == "persistLegacyWitness" || ident.Name == "lockReleaseSettings" {
					t.Fatalf("hot path %s must not reference %s", fn.Name.Name, ident.Name)
				}
			}
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				upper := strings.ToUpper(lit.Value)
				if strings.Contains(upper, "DBGUARD_STATE") && (strings.Contains(upper, "FOR UPDATE") || strings.Contains(upper, "FOR SHARE") ||
					strings.Contains(upper, "FOR NO KEY UPDATE") || strings.Contains(upper, "FOR KEY SHARE")) {
					t.Fatalf("hot path %s must not lock dbguard_state: %s", fn.Name.Name, lit.Value)
				}
			}
			return true
		})
	}
	for name := range hotPaths {
		if !seen[name] {
			t.Fatalf("expected hot path %s in postgres_normalized.go", name)
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
	dsn := strings.TrimSpace(os.Getenv("CHANGEGUARD_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CHANGEGUARD_TEST_POSTGRES_DSN is not set; PostgreSQL multi-instance integration skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cleanup := `DROP TABLE IF EXISTS changeguard_deployment_jobs,changeguard_joint_release_members,changeguard_joint_releases,changeguard_idempotency_records,changeguard_audit_events,changeguard_passports,changeguard_outbox,changeguard_changes,changeguard_normalization_migrations,dbguard_state`
	if _, err = pool.Exec(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), cleanup) })
	t.Setenv("CHANGEGUARD_ENABLE_DEMO_DATA", "true")

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
	changeIndex := -1
	for i, item := range first.data.Changes {
		if item.ID == change.ID {
			changeIndex = i
			break
		}
	}
	if changeIndex < 0 {
		first.mu.Unlock()
		t.Fatal("could not find the selected change after loading the normalized snapshot")
	}
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
			_, err := s.UsePassport(passport.ID, "token-hash", "ci", time.Now().UTC(), true, model.AuditEvent{OrganizationID: change.OrganizationID, ChangeID: change.ID, ActorID: "ci", Action: "CONSUME"})
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
	// 两个实例都必须看到已消费：走重放分支的那一方若只返回成功而不刷新内存快照，
	// 它随后读取到的仍是 ACTIVE。这里覆盖竞争双方，使该缺陷无法被调度顺序掩盖。
	for name, s := range map[string]*Store{"first": first, "second": second} {
		snapshot, err := s.Passport(passport.ID)
		if err != nil || snapshot.Status != model.PassportConsumed || snapshot.ConsumedBy != "ci" {
			t.Fatalf("%s postgres consume snapshot: %+v err=%v", name, snapshot, err)
		}
	}
	if _, err := first.UsePassport(passport.ID, "token-hash", "other-ci", time.Now().UTC(), true, model.AuditEvent{OrganizationID: change.OrganizationID, ChangeID: change.ID, ActorID: "other-ci", Action: "CONSUME"}); !errors.Is(err, ErrPassportReplay) {
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

func TestPostgresConcurrentEmptyMigration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CHANGEGUARD_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CHANGEGUARD_TEST_POSTGRES_DSN is not set; concurrent empty-DB migration skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cleanup := `DROP TABLE IF EXISTS changeguard_deployment_jobs,changeguard_joint_release_members,changeguard_joint_releases,changeguard_core_authority,changeguard_idempotency_records,changeguard_audit_events,changeguard_passports,changeguard_outbox,changeguard_changes,changeguard_normalization_migrations,dbguard_state`
	if _, err = pool.Exec(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), cleanup) })
	t.Setenv("CHANGEGUARD_ENABLE_DEMO_DATA", "true")

	const instances = 4
	start := make(chan struct{})
	stores := make(chan *Store, instances)
	errs := make(chan error, instances)
	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, openErr := NewPostgres(ctx, dsn)
			if openErr != nil {
				errs <- openErr
				return
			}
			stores <- s
		}()
	}
	close(start)
	wg.Wait()
	close(stores)
	close(errs)
	var opened []*Store
	for s := range stores {
		t.Cleanup(s.Close)
		opened = append(opened, s)
	}
	for err := range errs {
		t.Fatalf("concurrent NewPostgres on empty DB failed: %v", err)
	}
	if len(opened) != instances {
		t.Fatalf("expected %d opened stores, got %d", instances, len(opened))
	}
	baseCount := len(opened[0].Changes())
	if baseCount == 0 {
		t.Fatal("expected seeded demo changes")
	}
	for i := 1; i < len(opened); i++ {
		if got := len(opened[i].Changes()); got != baseCount {
			t.Fatalf("instance %d observed %d changes, want %d", i, got, baseCount)
		}
	}
	var phase string
	if err := pool.QueryRow(ctx, `SELECT phase FROM changeguard_core_authority WHERE name='core-v4.1'`).Scan(&phase); err != nil || phase != "AUTHORITATIVE" {
		t.Fatalf("expected core-v4.1 AUTHORITATIVE, got phase=%q err=%v", phase, err)
	}
}

func BenchmarkConcurrentMultiOrgAuditAppend(b *testing.B) {
	data := NewMemory()
	now := time.Now().UTC()
	const orgs = 8
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		worker := 0
		for pb.Next() {
			worker++
			orgID := fmt.Sprintf("org_bench_%d", worker%orgs)
			if err := data.RecordAudit(model.AuditEvent{
				OrganizationID: orgID,
				ActorID:        "usr_bench",
				ActorName:      "Bench",
				Action:         "BENCH_AUDIT",
				CreatedAt:      now,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkMultiPassportConcurrentConsume(b *testing.B) {
	data := NewMemory()
	now := time.Now().UTC()
	rules := changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo"))
	const poolSize = 64
	passports := make([]model.Passport, poolSize)
	for i := 0; i < poolSize; i++ {
		change, passport := passportFixture(now, rules)
		change.ID = fmt.Sprintf("chg_bench_%d", i)
		passport.ID = fmt.Sprintf("pass_bench_%d", i)
		passport.ChangeID = change.ID
		passport.TokenSHA256 = fmt.Sprintf("hash_%d", i)
		if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: fmt.Sprintf("aud_c_%d", i), ChangeID: change.ID, CreatedAt: now}); err != nil {
			b.Fatal(err)
		}
		if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: fmt.Sprintf("aud_p_%d", i), ChangeID: change.ID, CreatedAt: now}); err != nil {
			b.Fatal(err)
		}
		passports[i] = passport
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := passports[i%poolSize]
		if _, err := data.UsePassport(p.ID, p.TokenSHA256, "ci-bench", now.Add(time.Second), true, model.AuditEvent{OrganizationID: p.OrganizationID, ChangeID: p.ChangeID, CreatedAt: now.Add(time.Second)}); err != nil {
			b.Fatal(err)
		}
	}
}
