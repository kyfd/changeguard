# ADR 0001: Idempotent Passport Consume

## Context

Gate consume is the production-deployment critical path. A CI job can POST `/api/gate/consume`, have the server commit `ACTIVE → CONSUMED`, then lose the HTTP response. The same job retries with the same token, artifact digest, environment, and consumer.

v3.0.1 treats any already-consumed passport as `409 PASSPORT_REPLAY`. That blocks a second *logical* consume, but it also makes a successful first consume look like a failure to the client that never received the 200.

## Problem

The client cannot distinguish:

- first consume succeeded, response was lost;
- another pipeline already consumed the passport.

A retry after a lost response currently fails closed in the wrong direction: the original job cannot confirm that it already holds the only successful consume.

## Security Invariants

1. One passport produces at most one logical consume.
2. A lost HTTP response must not prevent the original consumer from confirming that first result.
3. The same token, artifact digest, environment, and consumer replay the first public consume snapshot without writing a second consume audit or changing timestamps.
4. A different consumer, artifact digest, or environment must not reuse the first consume.
5. Consume success, change completion, and the consume audit remain one atomic persistence unit.
6. Concurrent callers, including two store instances sharing PostgreSQL, still satisfy 1–5.

## Functional Requirements

- First successful consume remains `200 GATE_ALLOWED` with `passport.status=CONSUMED`.
- Identical consume after that success returns the same public snapshot and `200 GATE_ALLOWED`.
- A conflicting consume remains `409 PASSPORT_REPLAY`.
- Binding mismatches remain `403` and do not change stored state.
- `verify` stays read-only and does not replay consume.

## Failure Modes

- Response lost after commit: retry with the same consumer succeeds as a replay.
- Two different CI jobs race: one logical consume, the other gets `PASSPORT_REPLAY`.
- Same token, different artifact or environment: `ARTIFACT_MISMATCH` / `ENVIRONMENT_MISMATCH`.
- Persistence failure on the first consume: no leaked `CONSUMED` or `COMPLETED` state; retry may attempt the first consume again.

## Concurrency Model

File store holds the process write lock for the whole consume. PostgreSQL locks the passport row (`SELECT ... FOR UPDATE`) and still uses `UPDATE ... WHERE status='ACTIVE'` for the first consume. Replay is a locked read of an already `CONSUMED` row that matches token hash and consumer; it does not insert a second audit event.

## Options

1. Treat consume as a generic `Idempotency-Key` mutation and store the first HTTP snapshot.
2. Replay from the consumed passport record when token hash, artifact, environment, and consumer match.
3. Keep `409` and require clients to inspect passport status before retrying.

Option 3 already exists and is insufficient: `changeguard-gate consume` has no status lookup, and a lost 200 is indistinguishable from a competing consume.

Option 1 is larger than necessary. The token is already a one-time credential; persisting another snapshot risks writing token material into idempotency records.

## Decision

Use option 2. Passport consume is independently idempotent for the original consumer. Do not attach Gate consume to `changeguard_idempotency_records` in this change.

Replay returns the original `consumed_at` / `consumed_by`. It does not mint a new audit event. HTTP may advertise the replay with `Idempotency-Replayed: true`; the JSON `code` stays `GATE_ALLOWED` so existing CLI jobs continue on HTTP 200.

This change does not split `COMPLETED` from consume, add `deployment_attempts`, rotate HMAC keys, or verify CI workload identity beyond the existing consumer string.

## Database Transaction

First consume: lock passport, verify token and bindings, set `CONSUMED`, set change `COMPLETED`, append one consume audit, commit.

Replay: lock passport, verify token hash and consumer, confirm artifact and environment still match the stored passport, return the existing public record, commit nothing.

## Migration

None. Existing `consumed_by` is sufficient. Empty `consumed_by` on a consumed passport is treated as a conflict, not a replay.

## Rollback

Revert the service and store matching rules. Existing consumed passports remain consumed; only retry behavior changes.

## Testing Strategy

- In-process: first consume succeeds; same consumer replay succeeds with unchanged timestamps; different consumer remains inactive/conflict.
- PostgreSQL two-store: after one success, the second instance returns the first snapshot for the same consumer.
- Existing 100-way and two-store races still allow only one logical consume.

## Observability

No new metric in this change. Replay is visible from HTTP `Idempotency-Replayed` and the absence of a second consume audit.

## Compatibility

Clients that already treat HTTP 200 as “safe to deploy” keep working. Clients that treated every second consume as a hard failure must stop treating same-consumer retries as incidents. `COMPLETED` still means the passport was consumed, not that production is healthy.
