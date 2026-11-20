-- ChangeGuard v4.1 core authority graduation.
-- Expands core_authority to AUTHORITATIVE for the five normalized core tables
-- (changes, outbox, passports, audit_events, idempotency_records).
-- Legacy dbguard_state remains for low-frequency org/policy/member metadata
-- and expand-only rollback compatibility.

ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS application_id text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS artifact_sha256 text NOT NULL DEFAULT '';
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS created_at timestamptz;
ALTER TABLE changeguard_changes ADD COLUMN IF NOT EXISTS updated_at timestamptz;

ALTER TABLE changeguard_audit_events ADD COLUMN IF NOT EXISTS sequence bigint;
WITH ordered AS (
  SELECT organization_id, id,
         COALESCE((SELECT MAX(sequence) FROM changeguard_audit_events existing WHERE existing.organization_id = changeguard_audit_events.organization_id AND existing.sequence IS NOT NULL), 0)
         + row_number() OVER (PARTITION BY organization_id ORDER BY created_at, id) AS sequence
  FROM changeguard_audit_events
  WHERE sequence IS NULL
)
UPDATE changeguard_audit_events target
SET sequence = ordered.sequence
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
