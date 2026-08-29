# ChangeGuard v3.0 release acceptance

This checklist is the release gate for the `v3.0` branch and annotated `v3.0.0` tag. Evidence must be an actual command result, CI check, release asset, or deployment observation; a proposed plan is not evidence.

## Production proof

- [ ] `go test ./...`, `go vet ./...`, and Linux `go test -race ./...` pass.
- [ ] PostgreSQL and Redis integration tests run without `SKIP` in CI.
- [ ] CONFIG and SQL Playwright golden paths pass against PostgreSQL, Redis, and the PostgreSQL shadow runner.
- [ ] Gate consume succeeds once and replay is rejected.
- [ ] PostgreSQL and Redis loss makes readiness fail closed and recovery does not require an application restart.

## Data authority

- [ ] `002_core_authority_v3.sql` applies twice without error.
- [ ] Normal business saves do not delete all change, passport, or audit rows.
- [ ] Change updates preserve monotonically increasing versions.
- [ ] Passport issue, expiry, revoke, and consume preserve one-active and one-consume invariants.
- [ ] Audit append order and hash chain verify after concurrent writes.
- [ ] `dbguard_state` remains a rollback witness during the v3.0 rollback window.

## API and product

- [ ] `docs/openapi.yaml` route coverage test passes.
- [ ] Existing array responses remain compatible; opted-in list pagination returns `items`, `next_cursor`, and `has_more`.
- [ ] Controlled application access denies users without a matching grant.
- [ ] Passport bearer token appears only in the first successful issue response.
- [ ] Governance outcomes include a definition version, source watermark, sample counts, and missing-signal state.
- [ ] Retention defaults exist; legal hold prevents destructive audit lifecycle operations.

## Operations and supply chain

- [ ] Base, production, and monitoring Kustomize outputs render.
- [ ] NetworkPolicy, ServiceMonitor, PrometheusRule, backup CronJob, and restore runbook are present.
- [ ] HTTP latency histogram, readiness, Outbox age, and PostgreSQL pool metrics are exposed.
- [ ] Critical vulnerability gate completes; SBOM is attached to the release.
- [ ] Release archive checksum and provenance bind to the annotated tag commit.

## GitHub release

- [ ] `origin/v3.0` exists and points to the reviewed commit.
- [ ] All GitHub Actions checks pass for that commit.
- [ ] Annotated tag `v3.0.0` points to the same commit.
- [ ] GitHub Release is published and its archive/checksum/SBOM assets independently verify.

## Ubuntu deployment

- [ ] Current server version and readiness are recorded without exposing secrets.
- [ ] A pre-deployment state/database backup exists and verifies.
- [ ] The verified immutable release is installed under `/opt/changeguard/releases`.
- [ ] The `current` symlink changes only after preflight and migration verification.
- [ ] systemd, liveness, readiness, authenticated metrics, session, organization isolation, SQL shadow runner, Gate consume/replay, Outbox, and audit chain smoke checks pass.
- [ ] Rollback command and previous immutable release are available; rollback is executed automatically if a critical check fails.
