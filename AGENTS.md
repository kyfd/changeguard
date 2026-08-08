# Repository Guidelines

## Project Structure & Module Organization

ChangeGuard is a Go production-change governance service. `cmd/dbguard` starts the core HTTP application; `cmd/changeguard-gate` is the CI digest/verify/consume client; the Agent gateway and offline evaluator live in the other `cmd` packages. Business state transitions belong in `internal/service`, persistence and organization isolation in `internal/store`, and HTTP/authentication boundaries in `internal/httpapi` and `internal/auth`. Deterministic artifact and passport rules are split across `internal/checker` and `internal/changegate`. External pipeline and post-release evidence parsers live in `internal/integration`. The embedded browser application is under `internal/httpapi/web`. Production, Kubernetes, migration, backup, provenance, and alerting assets are under `deploy`; protocol and operational decisions are documented in `docs`.

## Build, Test, and Development Commands

- `make run` or `go run ./cmd/dbguard`: run the local core service.
- `make test` or `go test ./...`: run the complete Go suite.
- `go test ./internal/service -run TestGovernanceOutcomes`: run a focused test.
- `go vet ./...`: run the repository's Go static checks.
- `go test -race ./...`: run the Linux race gate used by CI.
- `make build`: build `bin/dbguard` with `-trimpath`.
- `go run ./cmd/changeguard-agent-eval`: execute the 24-case offline Agent evaluation.
- `docker compose up --build`: run the PostgreSQL/Redis/shadow-database stack after configuring `.env`.

## Coding Style & Naming Conventions

Run `gofmt` on every changed Go file. Keep package names short and lowercase, exported API types in PascalCase, and JSON fields in snake_case. No separate linter configuration exists; `go vet` and CI are authoritative. Validate embedded JavaScript with `for file in internal/httpapi/web/*.js; do node --check "$file"; done`.

## Testing Guidelines

Tests are colocated as `*_test.go`. Security, organization isolation, idempotency, state-machine transitions, persistence/restart behavior, and failure-closed paths require focused coverage. Do not replace external evidence with random or simulated success data.

## Commit & Pull Request Guidelines

Historical Git metadata was unavailable when this baseline was created. Follow `CONTRIBUTING.md`: keep each commit scoped to one problem and run `go test ./...` first. Pull requests should state the affected governance invariant, verification commands, deployment impact, and rollback path. Never commit `.env`, runtime state, credentials, production data, or generated binaries.
