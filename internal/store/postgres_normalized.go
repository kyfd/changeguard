package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kyfd/changeguard/internal/audit"
	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

const (
	postgresNormalizedClaimOutboxSQL = `SELECT id, payload FROM changeguard_outbox WHERE next_attempt_at<=now() AND (status='PENDING' OR (status='PROCESSING' AND locked_until<=now())) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`
	postgresNormalizedAuditLockSQL   = `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`
	postgresMigrationLockSQL         = `SELECT pg_advisory_xact_lock(726384722)`
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
INSERT INTO changeguard_changes(id, organization_id, version, status, application_id, artifact_sha256, created_at, updated_at, payload)
SELECT item->>'id', item->>'organization_id', COALESCE(NULLIF(item->>'version','')::bigint, 0),
       COALESCE(item->>'status',''), COALESCE(item->>'application_id',''), COALESCE(item->>'artifact_sha256',''),
       NULLIF(item->>'created_at','')::timestamptz, NULLIF(item->>'updated_at','')::timestamptz, item
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

const postgresNormalizedAuthoritySQL = `
WITH ordered AS (
  SELECT organization_id, id,
         COALESCE((SELECT MAX(sequence) FROM changeguard_audit_events existing WHERE existing.organization_id = changeguard_audit_events.organization_id AND existing.sequence IS NOT NULL), 0)
         + row_number() OVER (PARTITION BY organization_id ORDER BY created_at, id) AS seq
  FROM changeguard_audit_events
  WHERE sequence IS NULL
)
UPDATE changeguard_audit_events target
SET sequence = ordered.seq
FROM ordered
WHERE target.organization_id = ordered.organization_id AND target.id = ordered.id;

INSERT INTO changeguard_core_authority(
  name, phase, source_version, changes_count, passports_count, audits_count, verified_at, activated_at, updated_at
)
SELECT 'core-v4.1', 'AUTHORITATIVE', COALESCE((SELECT version FROM dbguard_state WHERE id = 1), 0),
       (SELECT count(*) FROM changeguard_changes),
       (SELECT count(*) FROM changeguard_passports),
       (SELECT count(*) FROM changeguard_audit_events), now(), now(), now()
ON CONFLICT (name) DO UPDATE SET
  phase = 'AUTHORITATIVE',
  source_version = EXCLUDED.source_version,
  changes_count = EXCLUDED.changes_count,
  passports_count = EXCLUDED.passports_count,
  audits_count = EXCLUDED.audits_count,
  verified_at = COALESCE(changeguard_core_authority.verified_at, EXCLUDED.verified_at),
  activated_at = COALESCE(changeguard_core_authority.activated_at, EXCLUDED.activated_at),
  updated_at = EXCLUDED.updated_at;
`

const postgresCoreSignatureSQL = `SELECT concat_ws('|',
  COALESCE((SELECT version FROM dbguard_state WHERE id = 1), 0),
  (SELECT count(*) || ':' || COALESCE(max(version), 0) || ':' || COALESCE(extract(epoch from max(updated_at))::text, '') FROM changeguard_changes),
  (SELECT count(*) || ':' || COALESCE(max(lease_generation), 0) || ':' || COALESCE(extract(epoch from max(updated_at))::text, '') FROM changeguard_outbox),
  (SELECT count(*) || ':' || COALESCE(extract(epoch from max(issued_at))::text, '') || ':' || count(*) FILTER (WHERE status <> 'ACTIVE') FROM changeguard_passports),
  (SELECT count(*) || ':' || COALESCE(max(sequence), 0) FROM changeguard_audit_events),
  (SELECT count(*) || ':' || COALESCE(extract(epoch from max(updated_at))::text, '') FROM changeguard_idempotency_records)
)`

type passportDelta struct {
	Passport     *model.StoredPassport
	Expired      []model.StoredPassport
	Change       *model.ChangeRequest
	NewAudits    []model.AuditEvent
	ChangeAudits []model.AuditEvent
	Replayed     bool
}

func (b *postgresBackend) migrateNormalized(ctx context.Context) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, postgresMigrationLockSQL); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresStateMigration); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresNormalizedMigration); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, postgresJointReleaseMigration); err != nil {
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
	if _, err = tx.Exec(ctx, postgresNormalizedAuthoritySQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (b *postgresBackend) Signature(ctx context.Context) (string, error) {
	var sig string
	if err := b.pool.QueryRow(ctx, postgresCoreSignatureSQL).Scan(&sig); err != nil {
		return "", err
	}
	return sig, nil
}

func loadCoreTablesTx(ctx context.Context, tx pgx.Tx, data *state) error {
	changes := make([]model.ChangeRequest, 0)
	rows, err := tx.Query(ctx, `SELECT payload FROM changeguard_changes ORDER BY created_at DESC NULLS LAST, id DESC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var item model.ChangeRequest
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return err
		}
		changes = append(changes, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	data.Changes = changes

	outbox := make([]model.OutboxEvent, 0)
	rows, err = tx.Query(ctx, `SELECT payload FROM changeguard_outbox ORDER BY updated_at ASC, id ASC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var item model.OutboxEvent
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return err
		}
		outbox = append(outbox, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	data.Outbox = outbox

	passports := make([]model.StoredPassport, 0)
	rows, err = tx.Query(ctx, `SELECT payload FROM changeguard_passports ORDER BY issued_at ASC, id ASC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var item model.StoredPassport
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return err
		}
		passports = append(passports, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	data.Passports = passports

	audits := make([]model.AuditEvent, 0)
	rows, err = tx.Query(ctx, `SELECT payload FROM changeguard_audit_events ORDER BY organization_id ASC, sequence ASC NULLS LAST, created_at ASC, id ASC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var item model.AuditEvent
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return err
		}
		audits = append(audits, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	data.Audits = audits

	idempotency := make([]model.IdempotencyRecord, 0)
	rows, err = tx.Query(ctx, `SELECT payload FROM changeguard_idempotency_records ORDER BY updated_at ASC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var item model.IdempotencyRecord
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return err
		}
		idempotency = append(idempotency, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	data.IdempotencyRecords = idempotency
	return nil
}

func syncNormalizedState(ctx context.Context, tx pgx.Tx, data state) error {
	materializeExpiredPassports(&data, time.Now().UTC())
	// Match Gate's bytewise change-row order, with passports locked only
	// after change rows. Gate need not join the legacy snapshot row lock.
	changes := slices.SortedFunc(slices.Values(data.Changes), func(a, b model.ChangeRequest) int { return strings.Compare(a.ID, b.ID) })
	for _, item := range changes {
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
	if len(data.Audits) > 0 {
		orgSet := make(map[string]struct{}, 2)
		for _, item := range data.Audits {
			orgSet[item.OrganizationID] = struct{}{}
		}
		orgs := make([]string, 0, len(orgSet))
		for orgID := range orgSet {
			orgs = append(orgs, orgID)
		}
		sort.Strings(orgs)
		for _, orgID := range orgs {
			if _, err := tx.Exec(ctx, postgresNormalizedAuditLockSQL, orgID); err != nil {
				return err
			}
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

func appendOrgAuditsTx(ctx context.Context, tx pgx.Tx, change *model.ChangeRequest, events ...model.AuditEvent) ([]model.AuditEvent, error) {
	if len(events) == 0 {
		return nil, nil
	}
	orgSet := make(map[string]struct{}, 2)
	for _, event := range events {
		orgSet[event.OrganizationID] = struct{}{}
	}
	orgs := make([]string, 0, len(orgSet))
	for orgID := range orgSet {
		orgs = append(orgs, orgID)
	}
	sort.Strings(orgs)
	for _, orgID := range orgs {
		if _, err := tx.Exec(ctx, postgresNormalizedAuditLockSQL, orgID); err != nil {
			return nil, err
		}
	}

	lastByOrg := make(map[string]*model.AuditEvent, len(orgs))
	seqByOrg := make(map[string]int64, len(orgs))
	changeCache := make(map[string]*model.ChangeRequest)
	if change != nil && change.ID != "" {
		copyChange := *change
		changeCache[change.OrganizationID+"|"+change.ID] = &copyChange
	}

	linkedEvents := make([]model.AuditEvent, 0, len(events))
	for _, event := range events {
		if event.ID == "" {
			event.ID = NewID("audit_")
		}
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		if event.ChangeID != "" {
			event.ResourceType = "CHANGE"
			event.ResourceID = event.ChangeID
			cacheKey := event.OrganizationID + "|" + event.ChangeID
			bound, ok := changeCache[cacheKey]
			if !ok {
				var raw []byte
				err := tx.QueryRow(ctx, `SELECT payload FROM changeguard_changes WHERE id=$1 AND organization_id=$2`, event.ChangeID, event.OrganizationID).Scan(&raw)
				if err == nil {
					var loaded model.ChangeRequest
					if json.Unmarshal(raw, &loaded) == nil {
						bound = &loaded
					}
				}
				changeCache[cacheKey] = bound
			}
			if bound != nil {
				if event.ResourceVersionAfter == 0 {
					event.ResourceVersionAfter = bound.Version
				}
				if event.ResourceVersionBefore == 0 && bound.Version > 1 {
					event.ResourceVersionBefore = bound.Version - 1
				}
				if event.RequestDigest == "" {
					event.RequestDigest = bound.ArtifactSHA256
				}
			}
		}

		prev, loaded := lastByOrg[event.OrganizationID]
		if !loaded {
			var maxSeq int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM changeguard_audit_events WHERE organization_id=$1`, event.OrganizationID).Scan(&maxSeq); err != nil {
				return nil, err
			}
			seqByOrg[event.OrganizationID] = maxSeq
			var prevRaw []byte
			err := tx.QueryRow(ctx, `SELECT payload FROM changeguard_audit_events WHERE organization_id=$1 ORDER BY sequence DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, event.OrganizationID).Scan(&prevRaw)
			if err == nil {
				var previous model.AuditEvent
				if err := json.Unmarshal(prevRaw, &previous); err != nil {
					return nil, err
				}
				prev = &previous
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			lastByOrg[event.OrganizationID] = prev
		}

		linked, err := audit.Link(event, prev)
		if err != nil {
			return nil, err
		}
		seqByOrg[event.OrganizationID]++
		seq := seqByOrg[event.OrganizationID]
		encoded, err := json.Marshal(linked)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO changeguard_audit_events(organization_id,id,sequence,created_at,hash,prev_hash,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, linked.OrganizationID, linked.ID, seq, linked.CreatedAt, linked.Hash, linked.PrevHash, encoded); err != nil {
			return nil, err
		}
		copyLinked := linked
		lastByOrg[event.OrganizationID] = &copyLinked
		linkedEvents = append(linkedEvents, linked)
	}
	return linkedEvents, nil
}

func upsertChangeRowTx(ctx context.Context, tx pgx.Tx, item model.ChangeRequest) error {
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO changeguard_changes(id,organization_id,version,status,application_id,artifact_sha256,created_at,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET organization_id=EXCLUDED.organization_id,version=EXCLUDED.version,status=EXCLUDED.status,
		application_id=EXCLUDED.application_id,artifact_sha256=EXCLUDED.artifact_sha256,created_at=EXCLUDED.created_at,
		updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, item.ID, item.OrganizationID, item.Version, item.Status, item.ApplicationID, item.ArtifactSHA256, item.CreatedAt, item.UpdatedAt, payload)
	return err
}

func upsertOutboxRowTx(ctx context.Context, tx pgx.Tx, item model.OutboxEvent) error {
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO changeguard_outbox(id,organization_id,aggregate_id,event_type,status,next_attempt_at,locked_until,lease_generation,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (id) DO UPDATE SET organization_id=EXCLUDED.organization_id,aggregate_id=EXCLUDED.aggregate_id,event_type=EXCLUDED.event_type,
		status=EXCLUDED.status,next_attempt_at=EXCLUDED.next_attempt_at,locked_until=EXCLUDED.locked_until,
		lease_generation=EXCLUDED.lease_generation,updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, item.ID, item.OrganizationID, item.AggregateID, item.EventType, item.Status, item.NextAttemptAt, item.LockedUntil, item.LeaseGeneration, item.UpdatedAt, payload)
	return err
}

func (b *postgresBackend) createChange(ctx context.Context, change model.ChangeRequest, auditEvent model.AuditEvent) (model.ChangeRequest, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.ChangeRequest{}, nil, err
	}
	defer tx.Rollback(ctx)
	if err := upsertChangeRowTx(ctx, tx, change); err != nil {
		return model.ChangeRequest{}, nil, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, &change, auditEvent)
	if err != nil {
		return model.ChangeRequest{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ChangeRequest{}, nil, err
	}
	return change, linked, nil
}

func (b *postgresBackend) updateChange(ctx context.Context, id string, update func(*model.ChangeRequest) error, audits ...model.AuditEvent) (model.ChangeRequest, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.ChangeRequest{}, nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_changes WHERE id=$1 FOR UPDATE`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ChangeRequest{}, nil, ErrNotFound
	}
	if err != nil {
		return model.ChangeRequest{}, nil, err
	}
	var change model.ChangeRequest
	if err := json.Unmarshal(raw, &change); err != nil {
		return model.ChangeRequest{}, nil, err
	}
	if err := update(&change); err != nil {
		return model.ChangeRequest{}, nil, err
	}
	change.UpdatedAt = time.Now()
	change.Version++
	if err := upsertChangeRowTx(ctx, tx, change); err != nil {
		return model.ChangeRequest{}, nil, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, &change, audits...)
	if err != nil {
		return model.ChangeRequest{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ChangeRequest{}, nil, err
	}
	return change, linked, nil
}

func (b *postgresBackend) updateChangeWithOutbox(ctx context.Context, id string, update func(*model.ChangeRequest) error, event model.OutboxEvent, audits ...model.AuditEvent) (model.ChangeRequest, model.OutboxEvent, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_changes WHERE id=$1 FOR UPDATE`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, ErrNotFound
	}
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	var change model.ChangeRequest
	if err := json.Unmarshal(raw, &change); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	if err := update(&change); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	now := time.Now()
	change.UpdatedAt = now
	change.Version++
	if event.ID == "" {
		event.ID = NewID("out_")
	}
	if event.Status == "" {
		event.Status = model.OutboxPending
	}
	if event.MaxAttempts <= 0 {
		event.MaxAttempts = 5
	}
	if event.NextAttemptAt.IsZero() {
		event.NextAttemptAt = now
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = now
	}
	event.UpdatedAt = now
	if err := upsertChangeRowTx(ctx, tx, change); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	if err := upsertOutboxRowTx(ctx, tx, event); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, &change, audits...)
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	return change, event, linked, nil
}

func (b *postgresBackend) ensureExperimentOutbox(ctx context.Context) ([]model.OutboxEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT c.id, c.organization_id, COALESCE(NULLIF(c.artifact_sha256,''), c.payload->>'artifact_sha256', '')
		FROM changeguard_changes c
		WHERE c.status IN ('EXPERIMENT_QUEUED', 'EXPERIMENT_RUNNING')
		  AND NOT EXISTS (
		    SELECT 1 FROM changeguard_outbox o
		    WHERE o.event_type = 'experiment.requested'
		      AND o.aggregate_id = c.id
		      AND o.status NOT IN ('COMPLETED', 'DEAD')
		  )`)
	if err != nil {
		return nil, err
	}
	type missing struct {
		id, orgID, sha string
	}
	var items []missing
	for rows.Next() {
		var m missing
		if err := rows.Scan(&m.id, &m.orgID, &m.sha); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(items) == 0 {
		return nil, nil
	}
	now := time.Now()
	created := make([]model.OutboxEvent, 0, len(items))
	for _, m := range items {
		ev := model.OutboxEvent{
			ID: NewID("out_"), OrganizationID: m.orgID, AggregateType: "change", AggregateID: m.id,
			EventType: "experiment.requested", Status: model.OutboxPending, MaxAttempts: 5, InputSHA256: m.sha,
			NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := upsertOutboxRowTx(ctx, tx, ev); err != nil {
			return nil, err
		}
		created = append(created, ev)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

func (b *postgresBackend) claimOutbox(ctx context.Context, workerID string, lease time.Duration) (model.OutboxEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.OutboxEvent{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	var raw []byte
	err = tx.QueryRow(ctx, postgresNormalizedClaimOutboxSQL).Scan(&id, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OutboxEvent{}, ErrNotFound
	}
	if err != nil {
		return model.OutboxEvent{}, err
	}
	var claimed model.OutboxEvent
	if err := json.Unmarshal(raw, &claimed); err != nil {
		return model.OutboxEvent{}, err
	}
	if claimed.InputSHA256 == "" && claimed.EventType == "experiment.requested" {
		var sha string
		_ = tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(artifact_sha256,''), payload->>'artifact_sha256', '') FROM changeguard_changes WHERE id=$1`, claimed.AggregateID).Scan(&sha)
		claimed.InputSHA256 = sha
	}
	now := time.Now().UTC()
	until := now.Add(lease)
	claimed.Status = model.OutboxProcessing
	claimed.Attempts++
	if claimed.AttemptID == "" {
		claimed.AttemptID = NewID("attempt_")
	}
	claimed.LeaseGeneration++
	claimed.LockedBy = workerID
	claimed.LockedUntil = &until
	claimed.UpdatedAt = now
	if claimed.Stage == "" {
		claimed.Stage = model.OutboxStagePrepare
		claimed.StageStartedAt = timePointer(now)
	}
	claimed.StageUpdatedAt = timePointer(now)
	if err := upsertOutboxRowTx(ctx, tx, claimed); err != nil {
		return model.OutboxEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.OutboxEvent{}, err
	}
	return claimed, nil
}

func (b *postgresBackend) mutateOutboxRow(ctx context.Context, id string, fn func(*model.OutboxEvent, time.Time) error) (model.OutboxEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.OutboxEvent{}, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_outbox WHERE id=$1 FOR UPDATE`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OutboxEvent{}, ErrNotFound
	}
	if err != nil {
		return model.OutboxEvent{}, err
	}
	var event model.OutboxEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return model.OutboxEvent{}, err
	}
	if err := fn(&event, time.Now().UTC()); err != nil {
		return model.OutboxEvent{}, err
	}
	if err := upsertOutboxRowTx(ctx, tx, event); err != nil {
		return model.OutboxEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.OutboxEvent{}, err
	}
	return event, nil
}

func (b *postgresBackend) completeOutbox(ctx context.Context, id, workerID string, generation uint64) (model.OutboxEvent, error) {
	return b.mutateOutboxRow(ctx, id, func(event *model.OutboxEvent, now time.Time) error {
		if !validOutboxLease(event, workerID, generation, now) {
			return ErrConcurrentWrite
		}
		event.Status = model.OutboxCompleted
		event.CompletedAt = timePointer(now)
		event.LockedBy = ""
		event.LockedUntil = nil
		event.LastError = ""
		event.UpdatedAt = now
		return nil
	})
}

func (b *postgresBackend) renewOutbox(ctx context.Context, id, workerID string, generation uint64, lease time.Duration) (model.OutboxEvent, error) {
	return b.mutateOutboxRow(ctx, id, func(event *model.OutboxEvent, now time.Time) error {
		if !validOutboxLease(event, workerID, generation, now) {
			return ErrConcurrentWrite
		}
		lockedUntil := now.Add(lease)
		event.LockedUntil = &lockedUntil
		event.UpdatedAt = now
		return nil
	})
}

func (b *postgresBackend) failOutbox(ctx context.Context, id, workerID string, generation uint64, cause error) (model.OutboxEvent, error) {
	return b.mutateOutboxRow(ctx, id, func(event *model.OutboxEvent, now time.Time) error {
		if !validOutboxLease(event, workerID, generation, now) {
			return ErrConcurrentWrite
		}
		event.LastError = cause.Error()
		event.LockedBy = ""
		event.LockedUntil = nil
		event.UpdatedAt = now
		if event.Attempts >= event.MaxAttempts {
			event.Status = model.OutboxDead
		} else {
			event.Status = model.OutboxPending
			event.AttemptID = ""
			event.Stage = ""
			event.StageStartedAt = nil
			event.StageUpdatedAt = nil
			event.ResultDigest = ""
			delay := time.Duration(1<<minInt(event.Attempts, 6)) * time.Second
			event.NextAttemptAt = now.Add(delay)
		}
		return nil
	})
}

func (b *postgresBackend) checkpointExperimentOutbox(ctx context.Context, id, workerID string, generation uint64, stage model.OutboxStage, inputSHA256 string) (model.OutboxEvent, error) {
	return b.mutateOutboxRow(ctx, id, func(event *model.OutboxEvent, now time.Time) error {
		if !validOutboxLease(event, workerID, generation, now) || event.InputSHA256 != inputSHA256 {
			return ErrConcurrentWrite
		}
		if event.Stage != stage {
			event.Stage = stage
			event.StageStartedAt = timePointer(now)
		}
		event.StageUpdatedAt = timePointer(now)
		event.UpdatedAt = now
		return nil
	})
}

func (b *postgresBackend) finalizeExperimentOutbox(ctx context.Context, id, workerID string, generation uint64, attemptID, inputSHA256, resultDigest string, update func(*model.ChangeRequest) error, audits ...model.AuditEvent) (model.ChangeRequest, model.OutboxEvent, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	defer tx.Rollback(ctx)
	var outRaw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_outbox WHERE id=$1 FOR UPDATE`, id).Scan(&outRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, ErrNotFound
	}
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	var event model.OutboxEvent
	if err := json.Unmarshal(outRaw, &event); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	now := time.Now().UTC()
	if !validOutboxLease(&event, workerID, generation, now) || event.AttemptID != attemptID || event.InputSHA256 != inputSHA256 {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, ErrConcurrentWrite
	}
	var chgRaw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_changes WHERE id=$1 FOR UPDATE`, event.AggregateID).Scan(&chgRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, ErrNotFound
	}
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	var change model.ChangeRequest
	if err := json.Unmarshal(chgRaw, &change); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	if event.ResultDigest != "" {
		if event.ResultDigest != resultDigest {
			return model.ChangeRequest{}, model.OutboxEvent{}, nil, ErrConcurrentWrite
		}
		return change, event, nil, nil
	}
	if err := update(&change); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	change.UpdatedAt = now
	change.Version++
	if err := upsertChangeRowTx(ctx, tx, change); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, &change, audits...)
	if err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	event.ResultDigest = resultDigest
	event.Stage = model.OutboxStageFinalize
	event.StageStartedAt = timePointer(now)
	event.StageUpdatedAt = timePointer(now)
	event.UpdatedAt = now
	if err := upsertOutboxRowTx(ctx, tx, event); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ChangeRequest{}, model.OutboxEvent{}, nil, err
	}
	return change, event, linked, nil
}

func (b *postgresBackend) retryOutbox(ctx context.Context, organizationID, id string, auditEvents ...model.AuditEvent) (model.OutboxEvent, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.OutboxEvent{}, nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_outbox WHERE id=$1 AND organization_id=$2 FOR UPDATE`, id, organizationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OutboxEvent{}, nil, ErrNotFound
	}
	if err != nil {
		return model.OutboxEvent{}, nil, err
	}
	var event model.OutboxEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return model.OutboxEvent{}, nil, err
	}
	if event.Status != model.OutboxDead {
		return model.OutboxEvent{}, nil, errors.New("event is not dead")
	}
	now := time.Now()
	event.Status = model.OutboxPending
	event.Attempts = 0
	event.AttemptID = ""
	event.Stage = ""
	event.StageStartedAt = nil
	event.StageUpdatedAt = nil
	event.ResultDigest = ""
	event.LastError = ""
	event.NextAttemptAt = now
	event.UpdatedAt = now
	if err := upsertOutboxRowTx(ctx, tx, event); err != nil {
		return model.OutboxEvent{}, nil, err
	}
	var linked []model.AuditEvent
	if len(auditEvents) > 0 {
		entry := auditEvents[0]
		entry.OrganizationID = organizationID
		if entry.ID == "" {
			entry.ID = NewID("audit_")
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = now
		}
		linked, err = appendOrgAuditsTx(ctx, tx, nil, entry)
		if err != nil {
			return model.OutboxEvent{}, nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.OutboxEvent{}, nil, err
	}
	return event, linked, nil
}

func (b *postgresBackend) abortIdempotency(ctx context.Context, claim model.IdempotencyRecord) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_idempotency_records WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5 FOR UPDATE`, claim.OrganizationID, claim.ActorID, claim.Operation, claim.Resource, claim.Key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var item model.IdempotencyRecord
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	if item.RequestDigest != claim.RequestDigest {
		return ErrNotFound
	}
	if item.Status != IdempotencyPending || !matchingIdempotencyClaim(item, claim) {
		return ErrIdempotencyClaimLost
	}
	tag, err := tx.Exec(ctx, `DELETE FROM changeguard_idempotency_records WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrIdempotencyClaimLost
	}
	return tx.Commit(ctx)
}

func (b *postgresBackend) completeIdempotency(ctx context.Context, claim model.IdempotencyRecord, result []byte, responseRef string, status int, resultDigest string) (model.IdempotencyRecord, model.IdempotencyRecord, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_idempotency_records WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5 FOR UPDATE`, claim.OrganizationID, claim.ActorID, claim.Operation, claim.Resource, claim.Key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, ErrNotFound
	}
	if err != nil {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, err
	}
	var item model.IdempotencyRecord
	if err := json.Unmarshal(raw, &item); err != nil {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, err
	}
	if item.RequestDigest != claim.RequestDigest {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, ErrNotFound
	}
	if item.Status == IdempotencySucceeded {
		if item.ResponseRef != responseRef || item.HTTPStatus != status || item.ResultDigest == "" || subtle.ConstantTimeCompare([]byte(item.ResultDigest), []byte(resultDigest)) != 1 {
			return model.IdempotencyRecord{}, model.IdempotencyRecord{}, ErrIdempotencyResultConflict
		}
		return publicIdempotencyRecord(item), item, nil
	}
	if !matchingIdempotencyClaim(item, claim) {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, ErrIdempotencyClaimLost
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
	encoded, err := json.Marshal(item)
	if err != nil {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE changeguard_idempotency_records SET status=$6,updated_at=$7,payload=$8 WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key, item.Status, item.UpdatedAt, encoded)
	if err != nil {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, err
	}
	if tag.RowsAffected() != 1 {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, ErrIdempotencyClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return model.IdempotencyRecord{}, model.IdempotencyRecord{}, err
	}
	return publicIdempotencyRecord(item), item, nil
}

func (b *postgresBackend) takeoverIdempotency(ctx context.Context, observed model.IdempotencyRecord) (model.IdempotencyRecord, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.IdempotencyRecord{}, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_idempotency_records WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5 FOR UPDATE`, observed.OrganizationID, observed.ActorID, observed.Operation, observed.Resource, observed.Key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IdempotencyRecord{}, ErrNotFound
	}
	if err != nil {
		return model.IdempotencyRecord{}, err
	}
	var item model.IdempotencyRecord
	if err := json.Unmarshal(raw, &item); err != nil {
		return model.IdempotencyRecord{}, err
	}
	if item.RequestDigest != observed.RequestDigest {
		return model.IdempotencyRecord{}, ErrNotFound
	}
	if item.Status != IdempotencyPending || observed.Generation == 0 || item.Generation != observed.Generation {
		return model.IdempotencyRecord{}, ErrIdempotencyClaimLost
	}
	item.Generation++
	if err := assignIdempotencyClaim(&item); err != nil {
		return model.IdempotencyRecord{}, err
	}
	item.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(item)
	if err != nil {
		return model.IdempotencyRecord{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE changeguard_idempotency_records SET updated_at=$6,payload=$7 WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, item.OrganizationID, item.ActorID, item.Operation, item.Resource, item.Key, item.UpdatedAt, encoded)
	if err != nil {
		return model.IdempotencyRecord{}, err
	}
	if tag.RowsAffected() != 1 {
		return model.IdempotencyRecord{}, ErrIdempotencyClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return model.IdempotencyRecord{}, err
	}
	return item, nil
}

func (b *postgresBackend) beginIdempotency(ctx context.Context, record model.IdempotencyRecord) (model.IdempotencyRecord, bool, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	record.Status = IdempotencyPending
	record.Generation = 1
	if err := assignIdempotencyClaim(&record); err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = record.CreatedAt
	// The primary key is the cross-instance claim; no read-then-insert window exists.
	encoded, err := json.Marshal(record)
	if err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO changeguard_idempotency_records(organization_id,actor_id,operation,resource,idempotency_key,request_digest,status,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, record.OrganizationID, record.ActorID, record.Operation, record.Resource, record.Key, record.RequestDigest, record.Status, record.UpdatedAt, encoded)
	if err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	if tag.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return model.IdempotencyRecord{}, false, err
		}
		return record, true, nil
	}
	var existingRaw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_idempotency_records WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND resource=$4 AND idempotency_key=$5`, record.OrganizationID, record.ActorID, record.Operation, record.Resource, record.Key).Scan(&existingRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IdempotencyRecord{}, false, ErrIdempotencyPending
	}
	if err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	var existing model.IdempotencyRecord
	if err := json.Unmarshal(existingRaw, &existing); err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	if existing.RequestDigest != record.RequestDigest {
		return publicIdempotencyRecord(existing), false, ErrIdempotencyConflict
	}
	if existing.Status == IdempotencySucceeded {
		return publicIdempotencyRecord(existing), false, nil
	}
	return publicIdempotencyRecord(existing), false, ErrIdempotencyPending
}

func (b *postgresBackend) cleanupExpiredIdempotency(ctx context.Context, organizationID string, at time.Time, limit int) (int, error) {
	tag, err := b.pool.Exec(ctx, `WITH target AS (
		SELECT organization_id, actor_id, operation, resource, idempotency_key
		FROM changeguard_idempotency_records
		WHERE organization_id=$1 AND status=$2 AND NULLIF(payload->>'expires_at','') IS NOT NULL AND (payload->>'expires_at')::timestamptz <= $3
		LIMIT $4
	)
	DELETE FROM changeguard_idempotency_records r USING target t
	WHERE r.organization_id=t.organization_id AND r.actor_id=t.actor_id AND r.operation=t.operation AND r.resource=t.resource AND r.idempotency_key=t.idempotency_key`,
		organizationID, IdempotencySucceeded, at, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (b *postgresBackend) appendAudit(ctx context.Context, event model.AuditEvent) (model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.AuditEvent{}, err
	}
	defer tx.Rollback(ctx)
	linked, err := appendOrgAuditsTx(ctx, tx, nil, event)
	if err != nil {
		return model.AuditEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.AuditEvent{}, err
	}
	if len(linked) == 0 {
		return model.AuditEvent{}, nil
	}
	return linked[0], nil
}

func (b *postgresBackend) createPassport(ctx context.Context, passport model.Passport, auditEvent model.AuditEvent) (model.StoredPassport, []model.StoredPassport, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.StoredPassport{}, nil, nil, err
	}
	defer tx.Rollback(ctx)
	var existingID string
	err = tx.QueryRow(ctx, `SELECT id FROM changeguard_passports WHERE id=$1`, passport.ID).Scan(&existingID)
	if err == nil {
		return model.StoredPassport{}, nil, nil, ErrConflict
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return model.StoredPassport{}, nil, nil, err
	}

	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `SELECT payload FROM changeguard_passports WHERE organization_id=$1 AND change_id=$2 AND status='ACTIVE' FOR UPDATE`, passport.OrganizationID, passport.ChangeID)
	if err != nil {
		return model.StoredPassport{}, nil, nil, err
	}
	var actives []model.StoredPassport
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return model.StoredPassport{}, nil, nil, err
		}
		var item model.StoredPassport
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return model.StoredPassport{}, nil, nil, err
		}
		if item.ID == passport.ID || now.Before(item.ExpiresAt) {
			rows.Close()
			return model.StoredPassport{}, nil, nil, ErrConflict
		}
		actives = append(actives, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return model.StoredPassport{}, nil, nil, err
	}
	rows.Close()

	auditsToAppend := make([]model.AuditEvent, 0, len(actives)+1)
	for i := range actives {
		actives[i].Status = model.PassportExpired
		encoded, err := json.Marshal(actives[i])
		if err != nil {
			return model.StoredPassport{}, nil, nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE changeguard_passports SET status='EXPIRED', payload=$2 WHERE id=$1`, actives[i].ID, encoded); err != nil {
			return model.StoredPassport{}, nil, nil, err
		}
		auditsToAppend = append(auditsToAppend, model.AuditEvent{
			OrganizationID: actives[i].OrganizationID,
			ChangeID:       actives[i].ChangeID,
			ActorID:        "system",
			ActorName:      "ChangeGuard",
			ActorType:      "SYSTEM",
			Action:         "PASSPORT_EXPIRED",
			PassportID:     actives[i].ID,
			Detail:         "签发新通行证前物化既有通行证自然过期状态",
			CreatedAt:      now,
		})
	}
	auditsToAppend = append(auditsToAppend, auditEvent)

	stored := model.StoredPassport{Passport: passport, TokenSHA256Stored: passport.TokenSHA256}
	stored.Passport.TokenSHA256 = ""
	encoded, err := json.Marshal(stored)
	if err != nil {
		return model.StoredPassport{}, nil, nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO changeguard_passports(id,organization_id,change_id,token_sha256,status,expires_at,issued_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, stored.ID, stored.OrganizationID, stored.ChangeID, stored.TokenSHA256Stored, stored.Status, stored.ExpiresAt, stored.IssuedAt, encoded); err != nil {
		if postgresErrorIsUnique(err) {
			return model.StoredPassport{}, nil, nil, ErrConflict
		}
		return model.StoredPassport{}, nil, nil, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, nil, auditsToAppend...)
	if err != nil {
		return model.StoredPassport{}, nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.StoredPassport{}, nil, nil, err
	}
	return stored, actives, linked, nil
}

func loadChangeAuditsTx(ctx context.Context, tx pgx.Tx, organizationID, changeID string) ([]model.AuditEvent, error) {
	rows, err := tx.Query(ctx, `SELECT payload FROM changeguard_audit_events WHERE organization_id=$1 AND payload->>'change_id'=$2 ORDER BY sequence ASC NULLS LAST, created_at ASC, id ASC`, organizationID, changeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.AuditEvent, 0, 8)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var ev model.AuditEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		items = append(items, ev)
	}
	return items, rows.Err()
}

func (b *postgresBackend) loadOrgPoliciesTx(ctx context.Context, tx pgx.Tx, organizationID string) ([]model.RiskPolicy, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT COALESCE(payload->'policies', '[]'::jsonb) FROM dbguard_state WHERE id=1`).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var all []model.RiskPolicy
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	filtered := make([]model.RiskPolicy, 0, len(all))
	for _, p := range all {
		if p.OrganizationID == organizationID {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

func (b *postgresBackend) usePassport(ctx context.Context, id, tokenSHA256, consumer string, at time.Time, consume bool, auditEvent model.AuditEvent, expected *model.Passport, job *model.GitLabJob) (model.Passport, passportDelta, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	defer tx.Rollback(ctx)
	var org, changeID string
	if err = tx.QueryRow(ctx, "SELECT organization_id,change_id FROM changeguard_passports WHERE id=$1", id).Scan(&org, &changeID); errors.Is(err, pgx.ErrNoRows) {
		return model.Passport{}, passportDelta{}, ErrNotFound
	} else if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	// Serialize discovery with plan creation and supersession, even if the
	// change is not yet a member. Read Committed sees membership after the wait.
	if err = lockReleaseOrganization(ctx, tx, org); err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	plan, position, err := postgresReleaseMembership(ctx, tx, org, changeID)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	ids := []string{changeID}
	if position >= 0 {
		// Only the target and its predecessors participate in this decision.
		for _, step := range plan.Steps[:position] {
			ids = append(ids, step.ChangeID)
		}
	}
	changes, err := releaseChangesTx(ctx, tx, org, ids, releaseChangeUpdate)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	snapshot := releaseSnapshot{data: state{Changes: changes}}
	var passRaw []byte
	err = tx.QueryRow(ctx, "SELECT payload FROM changeguard_passports WHERE id=$1 AND organization_id=$2 FOR UPDATE", id, org).Scan(&passRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Passport{}, passportDelta{}, ErrNotFound
	}
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	var item model.StoredPassport
	if err := json.Unmarshal(passRaw, &item); err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	if item.OrganizationID != org || item.ChangeID != changeID || item.ID != id {
		return model.Passport{}, passportDelta{}, ErrPassportTokenMismatch
	}
	replayed, err := validatePassportUse(item, expected, tokenSHA256, consumer, consume)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	clockNow := at.IsZero()
	if clockNow {
		at = time.Now().UTC()
	}
	if !replayed && !at.Before(item.ExpiresAt) {
		item.Status = model.PassportExpired
		auditEvent.Action = "PASSPORT_EXPIRED"
		encoded, err := json.Marshal(item)
		if err != nil {
			return model.Passport{}, passportDelta{}, err
		}
		if _, err = tx.Exec(ctx, "UPDATE changeguard_passports SET status=$2,payload=$3 WHERE id=$1 AND status='ACTIVE'", id, item.Status, encoded); err != nil {
			return model.Passport{}, passportDelta{}, err
		}
		linked, err := appendOrgAuditsTx(ctx, tx, nil, auditEvent)
		if err != nil {
			return model.Passport{}, passportDelta{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return model.Passport{}, passportDelta{}, err
		}
		return model.Passport{}, passportDelta{Passport: &item, NewAudits: linked}, ErrPassportExpired
	}
	bound := snapshot.change(org, changeID)
	if bound.ID == "" {
		return model.Passport{}, passportDelta{}, ErrPassportChangeInvalid
	}
	// Read the latest committed rules after waiting for the normalized locks.
	// MVCC neither waits for nor holds the legacy snapshot write lock.
	policies, err := b.loadOrgPoliciesTx(ctx, tx, item.OrganizationID)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	if err := validatePassportChange(item, bound, changegate.RuleSetVersion(policies), replayed); err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	updatedPlan := plan
	if position >= 0 {
		updatedPlan, err = snapshot.gate(plan, position, item, job, at, consume)
		if err != nil {
			return model.Passport{}, passportDelta{}, err
		}
	}
	if consume || job != nil {
		existing, err := loadJobBindingTx(ctx, tx, "passport_id=$1", item.ID)
		if err != nil {
			return model.Passport{}, passportDelta{}, err
		}
		if existing.PassportID == "" && job != nil {
			existing, err = loadJobBindingTx(ctx, tx, "source=$1 AND job_id=$2", job.Source, job.JobID)
			if err != nil {
				return model.Passport{}, passportDelta{}, err
			}
		}
		if err := validateJobBinding(existing, item, plan, job, replayed); err != nil {
			return model.Passport{}, passportDelta{}, err
		}
	}
	if !consume || replayed {
		delta := passportDelta{Passport: &item, Change: &bound, Replayed: replayed}
		if audits, aErr := loadChangeAuditsTx(ctx, tx, item.OrganizationID, item.ChangeID); aErr == nil {
			delta.ChangeAudits = audits
		}
		if !consume && expected != nil {
			delta.NewAudits, err = appendOrgAuditsTx(ctx, tx, nil, auditEvent)
			if err != nil {
				return model.Passport{}, passportDelta{}, err
			}
			if err = tx.Commit(ctx); err != nil {
				return model.Passport{}, passportDelta{}, err
			}
		}
		return publicPassport(item, false), delta, nil
	}
	if job != nil {
		if err := insertJobBindingTx(ctx, tx, bindJob(job, item, plan)); err != nil {
			return model.Passport{}, passportDelta{}, err
		}
	}
	if position >= 0 {
		if err := updateReleaseTx(ctx, tx, updatedPlan); err != nil {
			return model.Passport{}, passportDelta{}, err
		}
	}
	item.Status = model.PassportConsumed
	item.ConsumedAt = &at
	item.ConsumedBy = consumer
	bound.Status = model.StatusCompleted
	bound.UpdatedAt = at
	bound.Version++
	bound.Timeline = append(bound.Timeline, model.TimelineEntry{ID: NewID("timeline_"), Status: model.StatusCompleted, Title: "CI 通行证已消费", Detail: "制品摘要、环境与规则版本验证通过，生产变更自动闭环", Actor: consumer, CreatedAt: at})
	auditEvent.Action = "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED"
	auditEvent.Detail += "；变更状态已原子更新为 COMPLETED"
	encodedPassport, err := json.Marshal(item)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	tag, err := tx.Exec(ctx, "UPDATE changeguard_passports SET status=$2,payload=$3 WHERE id=$1 AND status='ACTIVE'", id, item.Status, encodedPassport)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	if tag.RowsAffected() != 1 {
		return model.Passport{}, passportDelta{}, ErrPassportInactive
	}
	if err := upsertChangeRowTx(ctx, tx, bound); err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, &bound, auditEvent)
	if err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.Passport{}, passportDelta{}, err
	}
	return publicPassport(item, false), passportDelta{Passport: &item, Change: &bound, NewAudits: linked}, nil
}

func (b *postgresBackend) revokePassport(ctx context.Context, organizationID, id, actorID string, at time.Time, auditEvent model.AuditEvent) (model.Passport, model.StoredPassport, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM changeguard_passports WHERE id=$1 AND organization_id=$2 FOR UPDATE`, id, organizationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Passport{}, model.StoredPassport{}, nil, ErrNotFound
	}
	if err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	var item model.StoredPassport
	if err := json.Unmarshal(raw, &item); err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	if item.Status != model.PassportActive {
		return model.Passport{}, model.StoredPassport{}, nil, ErrPassportInactive
	}
	item.Status = model.PassportRevoked
	item.RevokedAt = &at
	item.RevokedByID = actorID
	encoded, err := json.Marshal(item)
	if err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE changeguard_passports SET status=$2, payload=$3 WHERE id=$1`, item.ID, item.Status, encoded); err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	linked, err := appendOrgAuditsTx(ctx, tx, nil, auditEvent)
	if err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Passport{}, model.StoredPassport{}, nil, err
	}
	return publicPassport(item, false), item, linked, nil
}

func (b *postgresBackend) revokePassportsByChange(ctx context.Context, organizationID, changeID, actorID string, at time.Time, auditEvent model.AuditEvent) ([]model.StoredPassport, []model.AuditEvent, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT payload FROM changeguard_passports WHERE organization_id=$1 AND change_id=$2 AND status='ACTIVE' FOR UPDATE`, organizationID, changeID)
	if err != nil {
		return nil, nil, err
	}
	var items []model.StoredPassport
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var item model.StoredPassport
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return nil, nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	if len(items) == 0 {
		return nil, nil, nil
	}
	for i := range items {
		items[i].Status = model.PassportRevoked
		items[i].RevokedAt = &at
		items[i].RevokedByID = actorID
		encoded, err := json.Marshal(items[i])
		if err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE changeguard_passports SET status=$2, payload=$3 WHERE id=$1`, items[i].ID, items[i].Status, encoded); err != nil {
			return nil, nil, err
		}
	}
	linked, err := appendOrgAuditsTx(ctx, tx, nil, auditEvent)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return items, linked, nil
}

func postgresErrorIsUnique(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}
