-- ChangeGuard v3.0 core authority expansion.
-- This migration is expand-only: legacy dbguard_state remains available during rollback.

ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS application_id text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS artifact_sha256 text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS created_at timestamptz;
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS updated_at timestamptz;
CREATE INDEX IF NOT EXISTS changeguard_changes_org_created_idx
  ON changeguard_changes (organization_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS changeguard_changes_org_application_status_idx
  ON changeguard_changes (organization_id, application_id, status);

ALTER TABLE changeguard_audit_events ADD COLUMN IF NOT EXISTS sequence bigint;
WITH ordered AS (
  SELECT organization_id, id,
         row_number() OVER (PARTITION BY organization_id ORDER BY created_at, id) AS sequence
  FROM changeguard_audit_events
  WHERE sequence IS NULL
)
UPDATE changeguard_audit_events target
SET sequence = ordered.sequence
FROM ordered
WHERE target.organization_id = ordered.organization_id AND target.id = ordered.id;
CREATE UNIQUE INDEX IF NOT EXISTS changeguard_audit_sequence_idx
  ON changeguard_audit_events (organization_id, sequence) WHERE sequence IS NOT NULL;
CREATE INDEX IF NOT EXISTS changeguard_audit_chain_v3_idx
  ON changeguard_audit_events (organization_id, sequence, created_at, id);

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
  CHECK (phase IN ('BACKFILLED', 'VERIFIED', 'AUTHORITATIVE'))
);

INSERT INTO changeguard_core_authority(
  name, phase, source_version, changes_count, passports_count, audits_count, verified_at, updated_at
)
SELECT 'core-v3', 'VERIFIED', COALESCE((SELECT version FROM dbguard_state WHERE id = 1), 0),
       (SELECT count(*) FROM changeguard_changes),
       (SELECT count(*) FROM changeguard_passports),
       (SELECT count(*) FROM changeguard_audit_events), now(), now()
ON CONFLICT (name) DO UPDATE SET
  source_version = EXCLUDED.source_version,
  changes_count = EXCLUDED.changes_count,
  passports_count = EXCLUDED.passports_count,
  audits_count = EXCLUDED.audits_count,
  verified_at = EXCLUDED.verified_at,
  updated_at = EXCLUDED.updated_at;
