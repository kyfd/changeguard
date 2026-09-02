# Repository Guidelines

## Scope

ChangeGuard is a Go service for governing production changes. Keep deterministic enforcement in the core path: static checks, authorization, approval state, artifact digests, passport issuance, and atomic consumption must not depend on optional model output or unverified external evidence.

## Repository Layout

- `cmd/dbguard`: core HTTP service.
- `cmd/changeguard-gate`: CI digest, verify, and consume client.
- `cmd/changeguard-evidence`: evidence bundle export and offline verification.
- `internal/service`: business state transitions and authorization decisions.
- `internal/store`: persistence, transactions, idempotency, and organization isolation.
- `internal/httpapi` and `internal/auth`: HTTP and authentication boundaries.
- `internal/checker` and `internal/changegate`: deterministic artifact and passport rules.
- `internal/integration`: pipeline and post-release evidence parsers.
- `internal/httpapi/web`: embedded browser application.
- `deploy`: Compose, Kubernetes, production installation, migration, backup, provenance, and alerts.
- `docs`: product, architecture, security, integration, and operations documentation.

## Build and Test

- `go run ./cmd/dbguard` or `make run`: run the core service.
- `go test ./...` or `make test`: run the Go test suite.
- `go test ./internal/service -run TestGovernanceOutcomes`: run a focused service test.
- `go vet ./...`: run Go static checks.
- `go test -race ./...`: run the race suite used by CI on Linux.
- `make build`: build `bin/dbguard` with `-trimpath`.
- `npm test`: run the web test suite.
- `for file in internal/httpapi/web/*.js; do node --check "$file"; done`: syntax-check embedded JavaScript.
- `docker compose up --build`: start PostgreSQL, Redis, and the shadow database after configuring `.env`.

Use Go 1.25 or newer. CI tests Go 1.25 and 1.26.

## Change Rules

- Run `gofmt` on every changed Go file.
- Keep package names short and lowercase, exported Go names in PascalCase, and JSON fields in snake_case.
- Put business invariants in the service or domain path, not only in HTTP handlers or the browser UI.
- Check organization and application authorization on every relevant store and service operation.
- Preserve failure-closed behavior for Gate, authentication, shadow validation, persistence, and external evidence parsing.
- Do not replace missing runtime evidence with simulated success values. `NOT_RUN` and `DEMO_ONLY` must remain distinguishable from executed validation.
- Model-facing tools remain read-only and cannot approve, issue or consume passports, run SQL, deploy, roll back, or apply upgrades.

## Tests

Tests are colocated as `*_test.go`. Add focused coverage for security boundaries, organization isolation, idempotency, state transitions, persistence and restart behavior, concurrent passport consumption, and failure paths. Tests that require PostgreSQL or Redis should skip explicitly when their test DSN is absent; a skipped integration test is not evidence that the target environment passed.

## Commits and Pull Requests

Do not push directly to `main`. Keep each commit scoped to one problem. Before opening a pull request, run the relevant focused tests and normally `go test ./...`, `go vet ./...`, and `npm test`.

A pull request should describe the affected invariant, verification commands, deployment impact, compatibility considerations, and rollback path. Never commit `.env`, runtime state, credentials, production data, database dumps, or generated binaries. See `CONTRIBUTING.md` for the contributor workflow.
