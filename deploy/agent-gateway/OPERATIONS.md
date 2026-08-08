# ChangeGuard Agent Gateway Operations

This release adds a loopback-only protection layer in front of the existing
ChangeGuard Agent endpoints. It does not replace or mutate the core
`changeguard.service` binary.

## Protected routes

- `POST /api/changes/{id}/agent-ask`
- `POST /api/changes/{id}/submit-check`
- `GET /api/agent-runtime/summary` (enterprise administrators and technical owners)
- `GET /api/agent-runtime/events?limit=20` (sanitized, newest-first audit events)

The gateway enforces request and response limits, per-principal rate limits,
prompt-injection signalling, upstream timeouts, sanitized Prometheus metrics,
and an append-only HMAC-SHA256 audit chain. The audit log records hashes and
operational metadata only; prompts, answers, cookies, authorization headers,
and API keys are never written.

The summary also exposes an operational SLO view. Security/client rejections
are reported separately and do not reduce service availability; only protected
requests that reached a success or service-failure outcome enter the
availability denominator.

The SLO counters and the most recent 512 duration samples are stored in a
fixed-duration checkpoint (24 hours by default). The checkpoint is written
atomically, signed with a domain-separated HMAC, and restored after process or
host restart. A missing checkpoint is initialized safely; an invalid or
externally modified checkpoint degrades readiness instead of silently resetting
the SLO window.

## Health and observability

Run these commands on the server:

```sh
curl -fsS http://127.0.0.1:18081/health/live
curl -fsS http://127.0.0.1:18081/health/ready
curl -fsS http://127.0.0.1:18081/health/slo
systemctl status changeguard-agent-gateway.service
journalctl -u changeguard-agent-gateway.service --since '15 minutes ago'
```

`/health/ready` fails closed if the upstream core is unavailable, the audit
chain cannot be verified, or the persisted metrics checkpoint loses integrity.
`/health/slo` returns HTTP 503 only when the operational objective is degraded;
an empty but healthy window returns HTTP 200 with status `observing`.
`/metrics` is loopback-only unless the configured bearer token is supplied.

## Monitoring and alerting

Install and enable the included timer when this gateway runs on systemd:

```sh
install -m 0644 changeguard-agent-monitor.service /etc/systemd/system/
install -m 0644 changeguard-agent-monitor.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now changeguard-agent-monitor.timer
systemctl start changeguard-agent-monitor.service
systemctl status changeguard-agent-monitor.service changeguard-agent-monitor.timer
```

The monitor checks readiness and SLO once per minute and writes the latest
result atomically to `/opt/changeguard-agent/data/monitor.json`. A degraded
check exits non-zero and is visible in systemd/journal. If
`CHANGEGUARD_AGENT_ALERT_WEBHOOK_URL` is configured, the monitor sends one
notification on transition to degraded and one on recovery. The optional
Bearer token and URL remain in the root-owned environment file and are passed
to curl through a private temporary config rather than command-line arguments.

For Prometheus deployments, load `prometheus-alerts.yaml` and scrape the
loopback `/metrics` endpoint with `CHANGEGUARD_AGENT_METRICS_TOKEN`. The rules
cover process availability, upstream readiness, audit/metrics integrity,
availability and latency objectives, upstream errors, and audit-file growth.

## Data retention

- `metrics.json` is the active signed checkpoint. Do not edit it. It resets at
  the configured fixed-window boundary and remains continuous across restart.
- `monitor.json` is replaceable operational state and may be retained with the
  corresponding release evidence.
- `audit.jsonl` is an HMAC-linked append-only chain. Do **not** truncate it or
  apply ordinary logrotate, because either action breaks startup verification.
- Back up the audit file, metrics checkpoint, monitor state, environment file,
  active release IDs, and Nginx vhost together. Verify that the copies are
  readable before pruning any backup set.
- `deploy/production/changeguard-backup.sh` implements that atomic snapshot,
  writes and verifies `manifest.sha256`, and retains 30 snapshots by default.
  Install it as `/opt/changeguard/backup.sh` only after preserving the previous
  script, then run it once interactively and verify the generated manifest
  before relying on the existing daily cron entry.
- When the core uses file storage, the same backup treats the main JSON, the
  rollback migration witness, and its `.required` marker as one recovery unit.
  It rejects a snapshot unless the JSON hash matches the witness current or
  previous state and the witness self-digest verifies.
- Verify every retained snapshot with
  `deploy/production/changeguard-restore.sh verify` and stage it under a
  dedicated restore root before a release. The restore tool rejects symlinks,
  path traversal, manifest coverage gaps, broad secret-file permissions,
  broken audit/metrics checkpoints, and incomplete witness pairs. It never
  writes directly to the live core data directory; a restored copy must pass
  candidate startup and business-state checks before any explicit activation.
- Alert at 256 MiB and plan a chain-aware archive before the active audit file
  reaches that threshold. Until an archive verifier/checkpoint format is
  deployed, retain the complete active chain.

## Release and rollback

Releases are immutable directories under `/opt/changeguard-agent/releases`.
The `/opt/changeguard-agent/current` and `/opt/changeguard-agent/ui/current`
symlinks select the active gateway and UI release.

To roll back the gateway binary, point `current` to the previous release and
restart the service. To roll back the public routes, restore the timestamped
Nginx vhost backup, run `nginx -t`, then reload Nginx. The original core service
and its data file are not part of this rollback path.
