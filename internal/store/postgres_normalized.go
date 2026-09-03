package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kyfd/changeguard/internal/audit"
	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

const (
	postgresNormalizedClaimOutboxSQL = `SELECT id FROM changeguard_outbox WHERE next_attempt_at<=now() AND (status='PENDING' OR (status='PROCESSING' AND locked_until<=now())) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`
	postgresNormalizedAuditLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`
)

const postgresNormalizedMigration = `
CREATE TABLE IF NOT EXISTS changeguard_normalization_migrations (
  name text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS changeguard_changes (
  id text PRIMARY KEY,
  organization_id text NOT NULL,
  version bigint NOT NULL,
  status text NOT NULL DEFAULT '',
  application_id text NOT NULL DEFAULT '',
  artifact_sha256 text NOT NULL DEFAULT '',
  created_at timestamptz,
  updated_at timestamptz,
  payload jsonb NOT NULL
);
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS application_id text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS artifact_sha256 text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS created_at timestamptz;
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS updated_at timestamptz;
CREATE INDEX IF NOT EXISTS changeguard_changes_org_idx ON changeguard_changes (organization_id);
CREATE INDEX IF NOT EXISTS changeguard_changes_org_created_idx ON changeguard_changes (organization_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS changeguard_changes_org_application_status_idx ON changeguard_changes (organization_id, application_id, status);
CREATE TABLE IF NOT EXISTS changeguard_outbox (
  id text PRIMARY KEY,
  organization_id text NOT NULL,
  aggregate_id text NOT NULL,
  event_type text NOT NULL,
  status text NOT NULL,
  next_attempt_at timestamptz NOT NULL,
  locked_until timestamptz,
  lease_generation bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL,
  payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS changeguard_outbox_claim_idx ON changeguard_outbox (next_attempt_at, id)
  WHERE status IN ('PENDING', 'PROCESSING');
CREATE INDEX IF NOT EXISTS changeguard_outbox_org_idx ON changeguard_outbox (organization_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS changeguard_passports (
  id text PRIMARY KEY,
  organization_id text NOT NULL,
  change_id text NOT NULL,
  token_sha256 text NOT NULL,
  status text NOT NULL,
  expires_at timestamptz NOT NULL,
  issued_at timestamptz NOT NULL,
  payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS changeguard_passports_change_idx ON changeguard_passports (organization_id, change_id, issued_at DESC);
CREATE TABLE IF NOT EXISTS changeguard_audit_events (
  organization_id text NOT NULL,
  id text NOT NULL,
  sequence bigint,
  created_at timestamptz NOT NULL,
  hash text NOT NULL,
  prev_hash text NOT NULL DEFAULT '',
  payload jsonb NOT NULL,
  PRIMARY KEY (organization_id, id)
);
ALTER TABLE changeguard_audit_events ADD COLUMN IF NOT EXISTS sequence bigint;
CREATE UNIQUE INDEX IF NOT EXISTS changeguard_audit_sequence_idx ON changeguard_audit_events (organization_id, sequence) WHERE sequence IS NOT NULL;
CREATE INDEX IF NOT EXISTS changeguard_audit_chain_idx ON changeguard_audit_events (organization_id, sequence, created_at, id);
CREATE TABLE IF NOT EXISTS changeguard_core_authority (
  name text PRIMARY KEY,
  phase text NOT NULL,
  source_version bigint NOT NULL DEFAULT 0,
  changes_count bigint NOT NULL DEFAULT 0,
  passports_count bigint NOT NULL DEFAULT 0,
  audits_count bigint NOT NULL DEFAULT 0,
  verified_at timestamptz,
  activated_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (phase IN ('BACKFILLED','VERIFIED','AUTHORITATIVE'))
);
CREATE TABLE IF NOT EXISTS changeguard_idempotency_records (
  organization_id text NOT NULL,
  actor_id text NOT NULL,
  operation text NOT NULL,
  resource text NOT NULL,
  idempotency_key text NOT NULL,
  request_digest text NOT NULL,
  status text NOT NULL,
  updated_at timestamptz NOT NULL,
  payload jsonb NOT NULL,
  PRIMARY KEY (organization_id, actor_id, operation, resource, idempotency_key)
);
`

const postgresNormalizedExpirePassports = `
UPDATE changeguard_passports
SET status = 'EXPIRED', payload = jsonb_set(payload, '{status}', '"EXPIRED"'::jsonb, true)
WHERE status = 'ACTIVE' AND expires_at <= now();
UPDATE dbguard_state
SET payload = jsonb_set(
      payload,
      '{passports}',
      COALESCE((
        SELECT jsonb_agg(
          CASE
            WHEN item->>'status' = 'ACTIVE' AND (item->>'expires_at')::timestamptz <= now()
              THEN jsonb_set(item, '{status}', '"EXPIRED"'::jsonb, true)
            ELSE item
          END
          ORDER BY ordinal
        )
        FROM jsonb_array_elements(CASE WHEN jsonb_typeof(payload->'passports')='array' THEN payload->'passports' ELSE '[]'::jsonb END) WITH ORDINALITY AS passports(item, ordinal)
      ), '[]'::jsonb),
      true
    ),
    version = version + 1,
    updated_at = now()
WHERE id = 1 AND EXISTS (
  SELECT 1
  FROM jsonb_array_elements(CASE WHEN jsonb_typeof(payload->'passports')='array' THEN payload->'passports' ELSE '[]'::jsonb END) item
  WHERE item->>'status' = 'ACTIVE' AND (item->>'expires_at')::timestamptz <= now()
);
`

const postgresNormalizedPassportIndexMigration = `
CREATE UNIQUE INDEX IF NOT EXISTS changeguard_passports_one_active_idx
  ON changeguard_passports (organization_id, change_id) WHERE status = 'ACTIVE'
`

const postgresNormalizedBackfill = `
INSERT INTO changeguard_changes(id, organization_id, version, payload)
SELECT item->>'id', item->>'organization_id', COALESCE(NULLIF(item->>'version','')::bigint, 0), item
FROM dbguard_state s CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(s.payload->'changes')='array' THEN s.payload->'changes' ELSE '[]'::jsonb END) item WHERE s.id=1
ON CONFLICT (id) DO NOTHING;
INSERT INTO changeguard_outbox(id, organization_id, aggregate_id, event_type, status, next_attempt_at, locked_until, lease_generation, updated_at, payload)
SELECT item->>'id', item->>'organization_id', item->>'aggregate_id', item->>'event_type', item->>'status',
       COALESCE(NULLIF(item->>'next_attempt_at','')::timestamptz, now()), NULLIF(item->>'locked_until','')::timestamptz,
       COALESCE(NULLIF(item->>'lease_generation','')::bigint,0), COALESCE(NULLIF(item->>'updated_at','')::timestamptz,now()), item
FROM dbguard_state s CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(s.payload->'outbox')='array' THEN s.payload->'outbox' ELSE '[]'::jsonb END) item WHERE s.id=1
ON CONFLICT (id) DO NOTHING;
INSERT INTO changeguard_passports(id, organization_id, change_id, token_sha256, status, expires_at, issued_at, payload)
SELECT item->>'id', item->>'organization_id', item->>'change_id', item->>'token_sha256',
       CASE WHEN item->>'status' = 'ACTIVE' AND (item->>'expires_at')::timestamptz <= now() THEN 'EXPIRED' ELSE item->>'status' END,
       (item->>'expires_at')::timestamptz, (item->>'issued_at')::timestamptz,
       CASE WHEN item->>'status' = 'ACTIVE' AND (item->>'expires_at')::timestamptz <= now()
            THEN jsonb_set(item, '{status}', '"EXPIRED"'::jsonb, true) ELSE item END
FROM dbguard_state s CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(s.payload->'passports')='array' THEN s.payload->'passports' ELSE '[]'::jsonb END) item WHERE s.id=1
ON CONFLICT (id) DO NOTHING;
INSERT INTO changeguard_audit_events(organization_id,id,created_at,hash,prev_hash,payload)
SELECT item->>'organization_id', item->>'id', (item->>'created_at')::timestamptz, COALESCE(item->>'hash',''), COALESCE(item->>'prev_hash',''), item
FROM dbguard_state s CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(s.payload->'audits')='array' THEN s.payload->'audits' ELSE '[]'::jsonb END) item WHERE s.id=1
ON CONFLICT (organization_id,id) DO NOTHING;
INSERT INTO changeguard_idempotency_records(organization_id,actor_id,operation,resource,idempotency_key,request_digest,status,updated_at,payload)
SELECT item->>'organization_id', item->>'actor_id', item->>'operation', item->>'resource', item->>'key', item->>'request_digest', item->>'status',
       COALESCE(NULLIF(item->>'updated_at','')::timestamptz,now()), item
FROM dbguard_state s CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(s.payload->'idempotency_records')='array' THEN s.payload->'idempotency_records' ELSE '[]'::jsonb END) item WHERE s.id=1
ON CONFLICT (organization_id,actor_id,operation,resource,idempotency_key) DO NOTHING;
INSERT INTO changeguard_normalization_migrations(name) VALUES ('core-v1') ON CONFLICT (name) DO NOTHING;
`

func (b *postgresBackend) migrateNormalized(ctx context.Context) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, postgresNormalizedMigration); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresNormalizedExpirePassports); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresNormalizedBackfill); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresNormalizedExpirePassports); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresNormalizedPassportIndexMigration); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func syncNormalizedState(ctx context.Context, tx pgx.Tx, data state) error {
	materializeExpiredPassports(&data, time.Now().UTC())
	for _, item := range data.Changes {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO changeguard_changes(id,organization_id,version,status,application_id,artifact_sha256,created_at,updated_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (id) DO UPDATE SET organization_id=EXCLUDED.organization_id,version=EXCLUDED.version,status=EXCLUDED.status,
			application_id=EXCLUDED.application_id,artifact_sha256=EXCLUDED.artifact_sha256,created_at=EXCLUDED.created_at,
			updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload
			WHERE changeguard_changes.version <= EXCLUDED.version`, item.ID, item.OrganizationID, item.Version, item.Status, item.ApplicationID, item.ArtifactSHA256, item.CreatedAt, item.UpdatedAt, payload); err != nil {
			return err
		}
	}
	for _, item := range data.Outbox {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO changeguard_outbox(id,organization_id,aggregate_id,event_type,status,next_attempt_at,locked_until,lease_generation,updated_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (id) DO UPDATE SET organization_id=EXCLUDED.organization_id,aggregate_id=EXCLUDED.aggregate_id,event_type=EXCLUDED.event_type,
			status=EXCLUDED.status,next_attempt_at=EXCLUDED.next_attempt_at,locked_until=EXCLUDED.locked_until,
			lease_generation=EXCLUDED.lease_generation,updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload
			WHERE changeguard_outbox.lease_generation <= EXCLUDED.lease_generation AND changeguard_outbox.updated_at <= EXCLUDED.updated_at`, item.ID, item.OrganizationID, item.AggregateID, item.EventType, item.Status, item.NextAttemptAt, item.LockedUntil, item.LeaseGeneration, item.UpdatedAt, payload); err != nil {
			return err
		}
	}
	for _, item := range data.Passports {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO changeguard_passports(id,organization_id,change_id,token_sha256,status,expires_at,issued_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status,payload=EXCLUDED.payload
			WHERE changeguard_passports.status='ACTIVE' OR changeguard_passports.status=EXCLUDED.status`, item.ID, item.OrganizationID, item.ChangeID, item.TokenSHA256Stored, item.Status, item.ExpiresAt, item.IssuedAt, payload); err != nil {
			return err
		}
	}
	for _, item := range data.Audits {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO changeguard_audit_events(organization_id,id,sequence,created_at,hash,prev_hash,payload)
			VALUES($1,$2,(SELECT COALESCE(MAX(sequence),0)+1 FROM changeguard_audit_events WHERE organization_id=$1),$3,$4,$5,$6)
			ON CONFLICT (organization_id,id) DO NOTHING`, item.OrganizationID, item.ID, item.CreatedAt, item.Hash, item.PrevHash, payload); err != nil {
			return err
		}
	}
	for _, item := range data.IdempotencyRecords {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO changeguard_idempotency_records(organization_id,actor_id,operation,resource,idempotency_key,request_digest,status,updated_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (organization_id,actor_id,operation,resource,idempotency_key) DO UPDATE SET status=EXCLUDED.status,updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload
			WHERE changeguard_idempotency_records.updated_at <= EXCLUDED.updated_at`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key, item.RequestDigest, item.Status, item.UpdatedAt, payload); err != nil {
			return err
		}
	}
	return nil
}

func loadStateForUpdate(ctx context.Context, tx pgx.Tx) (state, int64, error) {
	var payload []byte
	var version int64
	if err := tx.QueryRow(ctx, "SELECT payload,version FROM dbguard_state WHERE id=1 FOR UPDATE").Scan(&payload, &version); err != nil {
		return state{}, 0, err
	}
	var data state
	if err := json.Unmarshal(payload, &data); err != nil {
		return state{}, 0, err
	}
	return data, version, nil
}

func persistLegacyWitness(ctx context.Context, tx pgx.Tx, data state, version int64) ([]byte, int64, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, version, err
	}
	result, err := tx.Exec(ctx, "UPDATE dbguard_state SET version=version+1,payload=$1,updated_at=now() WHERE id=1 AND version=$2", payload, version)
	if err != nil {
		return nil, version, err
	}
	if result.RowsAffected() != 1 {
		return nil, version, ErrConcurrentWrite
	}
	return payload, version + 1, nil
}

func (b *postgresBackend) claimOutbox(ctx context.Context, workerID string, lease time.Duration) (model.OutboxEvent, []byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	defer tx.Rollback(ctx)
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	var id string
	err = tx.QueryRow(ctx, postgresNormalizedClaimOutboxSQL).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OutboxEvent{}, nil, 0, ErrNotFound
	}
	if err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	now := time.Now().UTC()
	until := now.Add(lease)
	var claimed model.OutboxEvent
	for i := range data.Outbox {
		if data.Outbox[i].ID == id {
			e := &data.Outbox[i]
			e.Status = model.OutboxProcessing
			e.Attempts++
			if e.AttemptID == "" {
				e.AttemptID = NewID("attempt_")
			}
			e.LeaseGeneration++
			e.LockedBy = workerID
			e.LockedUntil = &until
			e.UpdatedAt = now
			if e.Stage == "" {
				e.Stage = model.OutboxStagePrepare
				e.StageStartedAt = timePointer(now)
			}
			e.StageUpdatedAt = timePointer(now)
			claimed = *e
			break
		}
	}
	if claimed.ID == "" {
		return model.OutboxEvent{}, nil, 0, fmt.Errorf("normalized outbox %s missing from legacy witness", id)
	}
	encoded, err := json.Marshal(claimed)
	if err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE changeguard_outbox SET status=$2, locked_until=$3, lease_generation=$4, updated_at=$5, payload=$6 WHERE id=$1`, claimed.ID, claimed.Status, claimed.LockedUntil, claimed.LeaseGeneration, claimed.UpdatedAt, encoded); err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
	if err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.OutboxEvent{}, nil, 0, err
	}
	return claimed, payload, newVersion, nil
}

func (b *postgresBackend) abortIdempotency(ctx context.Context, claim model.IdempotencyRecord) ([]byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	for i := range data.IdempotencyRecords {
		item := data.IdempotencyRecords[i]
		if !sameIdempotencyScope(item, claim) || item.RequestDigest != claim.RequestDigest {
			continue
		}
		if item.Status != IdempotencyPending || !matchingIdempotencyClaim(item, claim) {
			return nil, 0, ErrIdempotencyClaimLost
		}
		data.IdempotencyRecords = append(data.IdempotencyRecords[:i], data.IdempotencyRecords[i+1:]...)
		tag, err := tx.Exec(ctx, `DELETE FROM changeguard_idempotency_records WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key)
		if err != nil {
			return nil, 0, err
		}
		if tag.RowsAffected() != 1 {
			return nil, 0, ErrIdempotencyClaimLost
		}
		payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
		if err != nil {
			return nil, 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, 0, err
		}
		return payload, newVersion, nil
	}
	return nil, 0, ErrNotFound
}

func (b *postgresBackend) completeIdempotency(ctx context.Context, claim model.IdempotencyRecord, result []byte, responseRef string, status int, resultDigest string) (model.IdempotencyRecord, []byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.IdempotencyRecord{}, nil, 0, err
	}
	defer tx.Rollback(ctx)
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return model.IdempotencyRecord{}, nil, 0, err
	}
	for i := range data.IdempotencyRecords {
		item := &data.IdempotencyRecords[i]
		if !sameIdempotencyScope(*item, claim) || item.RequestDigest != claim.RequestDigest {
			continue
		}
		if item.Status == IdempotencySucceeded {
			if item.ResponseRef != responseRef || item.HTTPStatus != status || item.ResultDigest == "" || subtle.ConstantTimeCompare([]byte(item.ResultDigest), []byte(resultDigest)) != 1 {
				return model.IdempotencyRecord{}, nil, 0, ErrIdempotencyResultConflict
			}
			return publicIdempotencyRecord(*item), nil, version, nil
		}
		if !matchingIdempotencyClaim(*item, claim) {
			return model.IdempotencyRecord{}, nil, 0, ErrIdempotencyClaimLost
		}
		now := time.Now().UTC()
		item.Status = IdempotencySucceeded
		item.HTTPStatus = status
		item.Result = append(item.Result[:0], result...)
		item.ResultDigest = resultDigest
		item.ResponseRef = responseRef
		item.UpdatedAt = now
		item.CompletedAt = &now
		expiresAt := now.Add(72 * time.Hour)
		item.ExpiresAt = &expiresAt
		encoded, err := json.Marshal(*item)
		if err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		tag, err := tx.Exec(ctx, `UPDATE changeguard_idempotency_records SET status=$6,updated_at=$7,payload=$8 WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key, item.Status, item.UpdatedAt, encoded)
		if err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		if tag.RowsAffected() != 1 {
			return model.IdempotencyRecord{}, nil, 0, ErrIdempotencyClaimLost
		}
		payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
		if err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		return publicIdempotencyRecord(*item), payload, newVersion, nil
	}
	return model.IdempotencyRecord{}, nil, 0, ErrNotFound
}

func (b *postgresBackend) takeoverIdempotency(ctx context.Context, observed model.IdempotencyRecord) (model.IdempotencyRecord, []byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.IdempotencyRecord{}, nil, 0, err
	}
	defer tx.Rollback(ctx)
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return model.IdempotencyRecord{}, nil, 0, err
	}
	for i := range data.IdempotencyRecords {
		item := &data.IdempotencyRecords[i]
		if !sameIdempotencyScope(*item, observed) || item.RequestDigest != observed.RequestDigest {
			continue
		}
		if item.Status != IdempotencyPending || observed.Generation == 0 || item.Generation != observed.Generation {
			return model.IdempotencyRecord{}, nil, 0, ErrIdempotencyClaimLost
		}
		item.Generation++
		if err := assignIdempotencyClaim(item); err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		item.UpdatedAt = time.Now().UTC()
		encoded, _ := json.Marshal(*item)
		tag, err := tx.Exec(ctx, `UPDATE changeguard_idempotency_records SET updated_at=$6,payload=$7 WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key, item.UpdatedAt, encoded)
		if err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		if tag.RowsAffected() != 1 {
			return model.IdempotencyRecord{}, nil, 0, ErrIdempotencyClaimLost
		}
		payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
		if err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return model.IdempotencyRecord{}, nil, 0, err
		}
		return *item, payload, newVersion, nil
	}
	return model.IdempotencyRecord{}, nil, 0, ErrNotFound
}

func (b *postgresBackend) beginIdempotency(ctx context.Context, record model.IdempotencyRecord) (model.IdempotencyRecord, bool, []byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.IdempotencyRecord{}, false, nil, 0, err
	}
	defer tx.Rollback(ctx)
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return model.IdempotencyRecord{}, false, nil, 0, err
	}
	for _, existing := range data.IdempotencyRecords {
		if sameIdempotencyScope(existing, record) {
			if existing.RequestDigest != record.RequestDigest {
				return existing, false, nil, version, ErrIdempotencyConflict
			}
			if existing.Status == IdempotencySucceeded {
				return existing, false, nil, version, nil
			}
			return existing, false, nil, version, ErrIdempotencyPending
		}
	}
	now := time.Now().UTC()
	record.Status = IdempotencyPending
	record.Generation = 1
	if err := assignIdempotencyClaim(&record); err != nil {
		return model.IdempotencyRecord{}, false, nil, 0, err
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = record.CreatedAt
	// The primary key is the cross-instance claim; no read-then-insert window exists.
	encoded, _ := json.Marshal(record)
	tag, err := tx.Exec(ctx, `INSERT INTO changeguard_idempotency_records(organization_id,actor_id,operation,resource,idempotency_key,request_digest,status,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, record.OrganizationID, record.ActorID, record.Operation, record.Resource, record.Key, record.RequestDigest, record.Status, record.UpdatedAt, encoded)
	if err != nil {
		return model.IdempotencyRecord{}, false, nil, 0, err
	}
	if tag.RowsAffected() != 1 {
		return model.IdempotencyRecord{}, false, nil, version, ErrIdempotencyPending
	}
	data.IdempotencyRecords = append(data.IdempotencyRecords, record)
	payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
	if err != nil {
		return model.IdempotencyRecord{}, false, nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.IdempotencyRecord{}, false, nil, 0, err
	}
	return record, true, payload, newVersion, nil
}

func (b *postgresBackend) appendAudit(ctx context.Context, event model.AuditEvent) (model.AuditEvent, []byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	defer tx.Rollback(ctx)
	// One transaction-scoped advisory lock serializes each organization's chain,
	// including the empty-chain case where no row can be locked yet.
	if _, err = tx.Exec(ctx, postgresNormalizedAuditLockSQL, event.OrganizationID); err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	if event.ID == "" {
		event.ID = NewID("audit_")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	var previous *model.AuditEvent
	for i := len(data.Audits) - 1; i >= 0; i-- {
		if data.Audits[i].OrganizationID == event.OrganizationID {
			candidate := data.Audits[i]
			previous = &candidate
			break
		}
	}
	linked, err := audit.Link(event, previous)
	if err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	data.Audits = append(data.Audits, linked)
	if err = insertAuditRow(ctx, tx, linked); err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
	if err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.AuditEvent{}, nil, 0, err
	}
	return linked, payload, newVersion, nil
}

func (b *postgresBackend) usePassport(ctx context.Context, id, tokenSHA256, consumer string, at time.Time, consume bool, auditEvent model.AuditEvent) (model.Passport, []byte, int64, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.Passport{}, nil, 0, err
	}
	defer tx.Rollback(ctx)
	data, version, err := loadStateForUpdate(ctx, tx)
	if err != nil {
		return model.Passport{}, nil, 0, err
	}
	var rowStatus, rowToken string
	if err = tx.QueryRow(ctx, "SELECT status,token_sha256 FROM changeguard_passports WHERE id=$1 FOR UPDATE", id).Scan(&rowStatus, &rowToken); errors.Is(err, pgx.ErrNoRows) {
		return model.Passport{}, nil, 0, ErrNotFound
	} else if err != nil {
		return model.Passport{}, nil, 0, err
	}
	if subtle.ConstantTimeCompare([]byte(rowToken), []byte(tokenSHA256)) != 1 {
		return model.Passport{}, nil, 0, ErrPassportTokenMismatch
	}
	passportIndex := -1
	for i := range data.Passports {
		if data.Passports[i].ID == id {
			passportIndex = i
			break
		}
	}
	if passportIndex < 0 {
		return model.Passport{}, nil, 0, ErrNotFound
	}
	item := &data.Passports[passportIndex]
	if rowStatus == string(model.PassportExpired) {
		return model.Passport{}, nil, 0, ErrPassportExpired
	}
	if consume && rowStatus == string(model.PassportConsumed) {
		if sameConsumerReplay(*item, rowToken, consumer) {
			return publicPassport(*item, false), nil, version, nil
		}
		return model.Passport{}, nil, 0, ErrPassportReplay
	}
	if rowStatus != string(model.PassportActive) {
		return model.Passport{}, nil, 0, ErrPassportInactive
	}
	if !at.Before(item.ExpiresAt) {
		item.Status = model.PassportExpired
		auditEvent.Action = "PASSPORT_EXPIRED"
		linked, err := linkAuditInState(&data, auditEvent)
		if err != nil {
			return model.Passport{}, nil, 0, err
		}
		encoded, _ := json.Marshal(*item)
		if _, err = tx.Exec(ctx, "UPDATE changeguard_passports SET status=$2,payload=$3 WHERE id=$1 AND status='ACTIVE'", id, item.Status, encoded); err != nil {
			return model.Passport{}, nil, 0, err
		}
		if err = insertAuditRow(ctx, tx, linked); err != nil {
			return model.Passport{}, nil, 0, err
		}
		payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
		if err != nil {
			return model.Passport{}, nil, 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return model.Passport{}, nil, 0, err
		}
		return model.Passport{}, payload, newVersion, ErrPassportExpired
	}
	changeIndex := -1
	for i := range data.Changes {
		if data.Changes[i].ID == item.ChangeID && data.Changes[i].OrganizationID == item.OrganizationID {
			changeIndex = i
			break
		}
	}
	if changeIndex < 0 {
		return model.Passport{}, nil, 0, ErrPassportChangeInvalid
	}
	bound := data.Changes[changeIndex]
	policies := make([]model.RiskPolicy, 0)
	for _, p := range data.Policies {
		if p.OrganizationID == item.OrganizationID {
			policies = append(policies, p)
		}
	}
	if bound.Status != model.StatusApproved || bound.ArtifactSHA256 != item.ArtifactSHA256 || bound.Environment != item.Environment || bound.RuleSetVersion != item.RuleSetVersion || changegate.RuleSetVersion(policies) != item.RuleSetVersion {
		return model.Passport{}, nil, 0, ErrPassportChangeInvalid
	}
	if !consume {
		return publicPassport(*item, false), nil, version, nil
	}
	item.Status = model.PassportConsumed
	item.ConsumedAt = &at
	item.ConsumedBy = consumer
	change := &data.Changes[changeIndex]
	change.Status = model.StatusCompleted
	change.UpdatedAt = at
	change.Version++
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: NewID("timeline_"), Status: model.StatusCompleted, Title: "CI 通行证已消费", Detail: "制品摘要、环境与规则版本验证通过，生产变更自动闭环", Actor: consumer, CreatedAt: at})
	auditEvent.Action = "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED"
	auditEvent.Detail += "；变更状态已原子更新为 COMPLETED"
	linked, err := linkAuditInState(&data, auditEvent)
	if err != nil {
		return model.Passport{}, nil, 0, err
	}
	encoded, _ := json.Marshal(*item)
	tag, err := tx.Exec(ctx, "UPDATE changeguard_passports SET status=$2,payload=$3 WHERE id=$1 AND status='ACTIVE'", id, item.Status, encoded)
	if err != nil {
		return model.Passport{}, nil, 0, err
	}
	if tag.RowsAffected() != 1 {
		return model.Passport{}, nil, 0, ErrPassportInactive
	}
	changePayload, _ := json.Marshal(*change)
	if _, err = tx.Exec(ctx, "UPDATE changeguard_changes SET version=$2,payload=$3 WHERE id=$1", change.ID, change.Version, changePayload); err != nil {
		return model.Passport{}, nil, 0, err
	}
	if err = insertAuditRow(ctx, tx, linked); err != nil {
		return model.Passport{}, nil, 0, err
	}
	payload, newVersion, err := persistLegacyWitness(ctx, tx, data, version)
	if err != nil {
		return model.Passport{}, nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Passport{}, nil, 0, err
	}
	return publicPassport(*item, false), payload, newVersion, nil
}

func linkAuditInState(data *state, event model.AuditEvent) (model.AuditEvent, error) {
	if event.ID == "" {
		event.ID = NewID("audit_")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	var previous *model.AuditEvent
	for i := len(data.Audits) - 1; i >= 0; i-- {
		if data.Audits[i].OrganizationID == event.OrganizationID {
			candidate := data.Audits[i]
			previous = &candidate
			break
		}
	}
	linked, err := audit.Link(event, previous)
	if err == nil {
		data.Audits = append(data.Audits, linked)
	}
	return linked, err
}
func insertAuditRow(ctx context.Context, tx pgx.Tx, event model.AuditEvent) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO changeguard_audit_events(organization_id,id,sequence,created_at,hash,prev_hash,payload)
		VALUES($1,$2,(SELECT COALESCE(MAX(sequence),0)+1 FROM changeguard_audit_events WHERE organization_id=$1),$3,$4,$5,$6)`, event.OrganizationID, event.ID, event.CreatedAt, event.Hash, event.PrevHash, encoded)
	return err
}

func (s *Store) installPostgresSnapshot(payload []byte, version int64) error {
	var latest state
	if err := json.Unmarshal(payload, &latest); err != nil {
		return err
	}
	normalizeState(&latest)
	s.data = latest
	s.version = version
	s.persisted = append(s.persisted[:0], payload...)
	return nil
}

func postgresErrorIsUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}
