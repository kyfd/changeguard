# Governance outcome metrics

`GET /api/governance/outcomes?window_days=30` returns aggregate metrics for the
authenticated actor's application scope. The accepted window is 1 to 365 days.
The endpoint is intended for enterprise governance reviews and BI ingestion; it
does not return prompt text, secrets, SQL contents, or individual evidence.

## Metrics backed by current evidence

- throughput: total, production, completed, accepted, and rejected changes;
- risk closure: total, blocking, open blocking, verified, and overdue findings;
- control coverage: deterministic checks, artifact evidence, rollback plans,
  success metrics, progressive delivery, and automatic rollback plans;
- flow: decision lead time, experiment pass rate, blocking-finding closure, and
  on-time finding closure;
- deployment outcomes: the latest linked terminal GitLab/Jenkins outcome per
  change, with a sample count and deployment failure rate;
- operational outcomes: terminal rollback outcomes, linked incident state,
  evidence-backed incident resolution time, and successful releases followed
  by an incident or rollback execution;
- business outcomes: explicit pre/post SLI comparisons, improvement direction,
  and attainment against an optional numeric objective.

All percentages expose their natural sample counts. A zero denominator produces
zero rather than an invented success rate. Decision lead time is measured only
when an APPROVED or REJECTED timeline event exists.

## Deliberately not claimed yet

The response includes `outcome_data_quality`. Each value remains false until the
rolling window contains the corresponding authoritative external signal:

- `release_outcome_observable`: becomes true only when at least one linked
  terminal GitLab/Jenkins outcome exists in the window;
- `rollback_outcome_observable`: requires a linked terminal rollback outcome;
- `incident_linkage_observable`: requires an incident identifier linked to a
  known change;
- `business_sli_observable`: requires a validated pre/post business SLI
  comparison with non-overlapping observation windows.

Business SLI evidence enters aggregation only when its baseline and observation
windows bracket the same change's successful terminal deployment event (with a
five-minute clock/delivery tolerance). A standalone metric comparison is stored
as evidence but cannot be presented as a release outcome.

For changes with several pipeline events, only the latest terminal state is
counted; RUNNING/PENDING events never enter the denominator. Rollback STARTED
proves remediation began but does not become a rollback outcome sample until a
terminal state arrives. Incident resolution time is calculated only when both
open and resolved evidence are linked. A business SLI without explicit values,
direction, and ordered windows is rejected rather than inferred.

Until those signals exist, ChangeGuard must not label approval rate as release
success rate or calculate change-failure rate/MTTR from workflow state alone.

The protected Prometheus endpoint publishes the global 30-day governance set
without high-cardinality change, incident, or metric identifiers: change and
control counts, deployment and rollback outcomes, incident state and resolution
time, post-release remediation rate, SLI direction, objective attainment, and
per-signal observability flags.
