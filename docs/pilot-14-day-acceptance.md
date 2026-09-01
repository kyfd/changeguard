# 14-day ChangeGuard pilot acceptance

Use this for a small, authorized pilot of 5–10 applications across SQL, configuration, and Kubernetes changes. It is an operating template, not evidence that a real customer pilot has already occurred.

## Before day 1

- Select applications, teams, environments, and a comparison baseline.
- Record the outcome metric definition version and source watermark.
- Connect GitLab/Jenkins terminal events and Operations outcome signals.
- Confirm application grants, reviewer separation, retention policy, backup, and rollback contacts.

## Daily checks

- Gate coverage and rejected replay attempts.
- Missing release, rollback, incident, or business-SLI signals.
- Approval and experiment queue lead time.
- Dead or overdue Outbox events.
- PostgreSQL, Redis, shadow runner, readiness, and backup alerts.

## Day 7 review

- Validate sample counts before interpreting percentages.
- Drill down every failed deployment, rollback, and linked incident to its change and audit proof.
- Treat `NOT_RUN`, `DEMO_ONLY`, and missing signals as unavailable evidence, never success.
- Correct integration mapping and stale data before expanding scope.

## Day 14 exit criteria

- All targeted production workflows use Gate verify/consume.
- No unauthorized or replayed release is accepted.
- Independent approval and application grants work as configured.
- SQL changes use real PostgreSQL shadow evidence with rollback verification.
- Audit chain, Evidence Bundle, backup, and restore-stage verification pass.
- Report coverage, lead-time, failure, rollback, incident, and SLI metrics with numerator, denominator, window, sample size, definition version, and freshness.
