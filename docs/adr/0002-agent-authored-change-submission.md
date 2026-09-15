# ADR 0002: Agent-Authored Change Submission

## Status

**Deferred proposal（延后提案，不属于首版范围）。**

首版的边界是：**模型工具保持只读，写操作由确定性编排在已认证业务接口上完成，不由模型直接发起。**
本 ADR 描述的"服务账号代提"排在其后，作为第二阶段扩展。

### 经代码核对后需要修正的表述

以下四处在写成本文时是推断，核对后不成立，保留原文仅作记录：

1. **"前置阶段没有外部副作用"不成立。** `QueueExperiment` 会在隔离 PostgreSQL 影子库
   **真实执行**迁移与回滚，这是外部副作用，不是"纯粹的内存操作"。
2. **"错误草案可以删除"当前不成立。** 仓库中没有删除变更的接口
   （检索 `DeleteChange` 与变更删除路由，0 命中），"the remedy is deletion" 只是设想。
3. **"agent 只需要 `submit` 权限"与第 3 步冲突。** `QueueExperiment` 要求 `技术负责人`
   角色（`internal/service/service.go:717`），而该角色会在 `canUseApplication`
   短路放行全部应用（`service.go:1805-1826`）。本文第 140 行已自述此冲突。
4. **"`VerifyGate(consume=true)` 按 actor 类型拒绝服务账号"在实现层落不了地。**
   该函数没有会话 actor：审计主体是合成的
   `model.User{ID: "ci:" + consumer, Role: "CI"}`（`internal/service/passport.go:211`）。
   要落实这条不变量，必须先决定是否把 Gate 调用方身份引入认证层。

### 与首版边界的关系

本文引言中的 "the model has exactly one write capability" 属于**延后提案的目标**，
不是当前状态，也不属于首版。首版不授予模型任何写能力。

---

*以下为原始提案内容，未做删改。*

## Context

Today a change request is created by a human through the browser application or by a client calling `POST /api/changes` with a session cookie. The model path in `internal/agent` is advisory only: `DataSource` (`internal/agent/tools.go:16-20`) structurally has no write method, and `cmd/dbguard/main.go:150-151` documents this as deliberate.

The proposal is to let an operator describe an intended change in natural language, have an agent draft it and submit it, and keep exactly one human approval gate before release.

This is not the same as "the model approves changes". The distinction is the whole point of this ADR.

## Why the State Machine Makes This Tractable

`model.ChangeRequest` moves through (`internal/model/model.go:29-38`):

```
DRAFT -> CHECKING -> CHECK_FAILED | READY_FOR_EXPERIMENT
      -> EXPERIMENT_QUEUED -> EXPERIMENT_RUNNING
      -> WAITING_APPROVAL -> APPROVED -> COMPLETED
```

The segment before `WAITING_APPROVAL` has two properties that bound agent risk:

1. **No external side effect.** Nothing in `DRAFT`, `READY_FOR_EXPERIMENT`, or `EXPERIMENT_*` touches a production database. The shadow run is isolated by construction.
2. **Reversible.** A wrong draft is a discardable row. The remedy is deletion, not rollback.

The segment after `WAITING_APPROVAL` is guarded by three independent human-held controls that this ADR does not modify:

| Control | Location | Guard |
|---|---|---|
| Approve | `internal/service/service.go:1113` | `review` capability; role must be `数据库审核人` or `技术负责人`; high risk requires `技术负责人` (`service.go:1141`) |
| Issue passport | `internal/service/passport.go:69` | Actor must be the recorded approver, `actor.ID == change.ReviewerID` (`passport.go:84`); artifact digest, live rule-set version, and a real shadow run are all re-verified |
| Consume | `internal/store/postgres_normalized.go:596` | `SELECT ... FOR UPDATE` plus a guarded `UPDATE ... WHERE status='ACTIVE'`; `RowsAffected != 1` fails |

Therefore the agent needs the `submit` capability and nothing else. The prohibitions in `AGENTS.md` — approve, issue, consume, run SQL, deploy, roll back, apply upgrades — all remain intact and continue to be enforced structurally rather than by prompt.

Two existing invariants are load-bearing here and must not be relaxed:

- `Create` forces `Status: DRAFT`, `Risk: RiskUnknown`, `Version: 1` (`service.go:497-498`). An agent cannot assert its own risk level; risk is written only by the deterministic checker.
- `Create` already enforces credential redaction, length limits, artifact normalization, and a future `planned_at` (`service.go:453-484`). These apply to agent-authored input unchanged.

## Decision

Introduce a **service account** actor type that may hold the `submit` capability and no other. All agent-authored submissions are attributed to that service account.

A service account:

- authenticates with a bearer credential, not a session cookie;
- carries `ActorType = SERVICE` and an `AuthMethod` identifying the credential class, using the fields already present on `model.AuditEvent` (`internal/model/model.go:277-278`);
- is rejected by `Approve`, `Reject`, `IssuePassport`, and `VerifyGate(consume=true)` on actor type alone, before any capability or role check runs;
- records the human who initiated the conversation in a new `OriginatorID` field on `ChangeRequest`.

## Security Invariants

1. A service account can drive a change no further than `WAITING_APPROVAL`. Any attempt to approve, reject, issue, or consume fails on actor type, independently of capability grants and role.
2. Approval of an agent-authored change is performed by a human who is neither the service account nor the change's `OriginatorID`.
3. Risk classification and blocking findings are produced only by the deterministic checker. Agent-supplied text never sets `ChangeRequest.Risk` and never clears a finding.
4. A retried submission caused by a lost response does not create a second change request.
5. Every agent-authored change is attributable to both the service account and the initiating human, and the attribution survives restart.
6. Content reaching the agent from repositories, artifacts, or change descriptions is untrusted and cannot escalate into an unreviewed production change.
7. The approving human can distinguish agent-generated content from human-authored content before approving.

## The Separation-of-Duties Gap

This is the central risk of the decision and is recorded here explicitly because its failure mode is silent.

Separation of duties is enforced by comparing identities (`service.go:1128`, `service.go:1180`, `passport.go:84`):

```go
if actor.ID == change.SubmitterID { return ErrForbidden }
```

When an agent submits under a service account, `SubmitterID` is the service account. The human who asked for the change is no longer the submitter of record. **That human can then approve the change they themselves requested, and the check above will pass.** No error is raised; the control simply stops applying.

Invariant 2 therefore cannot be satisfied by the existing comparison. It requires a second comparison against the initiating human:

```go
if actor.ID == change.SubmitterID { return ErrForbidden }
if change.OriginatorID != "" && actor.ID == change.OriginatorID { return ErrForbidden }
```

`OriginatorID` must be:

- set by the server from the authenticated originator context, never from the request body;
- immutable after `Create`;
- applied in `Approve`, `Reject`, and `IssuePassport`, matching the three places the existing submitter check appears.

An agent-authored change with an empty `OriginatorID` is a change whose provenance was lost. Such a change must be rejected at creation rather than accepted with a weaker control.

## Rejected Alternative: Acting-As Identity

The considered alternative was for the agent to submit as the originating human, so that `SubmitterID` remains that person and the existing check keeps working with no schema change.

It was rejected for this iteration: it requires impersonation in the authentication layer, which is a larger and more dangerous surface than an additional identity field, and it blurs audit attribution between the human and the agent.

The cost of rejecting it is that separation of duties now depends on an added comparison rather than on the existing one. That comparison is the single most important line of code in this design, and is called out in the test requirements below.

## Automation Bias

Before this change, an approver reviews a form a colleague filled in. After it, the approver reviews text a model produced. The realistic failure is not a hallucinated `DROP TABLE`; the checker catches that. It is a plausible, well-formatted, subtly wrong migration approved by someone who has approved forty correct ones.

The approval surface must therefore make provenance visible:

- agent-generated fields are labelled as such and visually distinct from human-authored fields;
- the originating human's verbatim request is shown alongside the generated artifact;
- the conversation and tool-call trace that produced the change are retrievable from the change;
- generated SQL is diffed against the repository artifact the digest refers to;
- for `HIGH` risk, the full `get_rule_findings` output is expanded and cannot be collapsed;
- `NOT_RUN` and `DEMO_ONLY` remain visually distinct from an executed shadow run, as required by `AGENTS.md`.

## Prompt Injection Becomes a Write-Path Concern

`internal/agent/injection.go` detects injection in order to protect an advisory opinion. Once the agent can submit, the same injection protects a production artifact, and the consequence of a miss changes from "bad advice" to "a crafted migration reaches a human approver wearing the costume of a routine one".

Mitigations, in order of strength:

1. The deterministic checker runs on agent-authored SQL exactly as on human-authored SQL. An injected instruction cannot suppress a blocking finding, because findings are computed server-side after submission.
2. All repository- and artifact-derived text stays wrapped as untrusted (`tools.go:288`, `WrapUntrustedChange`).
3. Injection detection already runs at the gateway before the model sees the question (`internal/agentgateway/gateway.go:324`).
4. Detected injection on a write-capable path is a hard refusal, not a warning, and is audited.

Injection detection is a defence-in-depth layer, not the primary control. The primary control is that the checker and the human approver both sit downstream of anything the agent produces.

## Submission Is Multi-Step

The flow to `WAITING_APPROVAL` is not automatic. A client calls, in order (`server.go:1102`, `:1222`, `:1224`, `:1239`):

1. `POST /api/changes` — `DRAFT`
2. `POST /api/changes/{id}/submit` — runs the checker synchronously; yields `CHECK_FAILED`, or `WAITING_APPROVAL` when there is no SQL, or `READY_FOR_EXPERIMENT` (`service.go:603-684`)
3. `POST /api/changes/{id}/experiment` — `EXPERIMENT_QUEUED`, when SQL is present (`service.go:699`)
4. poll `GET /api/changes/{id}` until the worker reaches `WAITING_APPROVAL` (`service.go:1358`)

Consequences for the agent orchestration:

- a partially advanced change is a normal intermediate state, not an error;
- step 2 landing on `CHECK_FAILED` is a legitimate outcome the agent reports back, never one it retries around;
- `QueueExperiment` additionally requires role `技术负责人` (`service.go:717`), so either the service account is granted that role for experiments or the human triggers step 3. Granting a service account the owner role would also grant it blanket `submit`/`review` across every application, because `canUseApplication` short-circuits on `RoleOwner` (`service.go:1805-1826`). It must not be granted. The capability needed for step 3 has to be separated from the owner role first.

## Idempotency

`POST /api/changes` has no idempotency key today. `Idempotency-Key` is honoured on experiment, approve, and passport issue (`server.go:1225`, `:1246`, `:1310`) and on gate consume (`server.go:1383`), but not on create (`server.go:1102`).

A human double-clicking submit is rare. An agent retrying after a timeout is routine. Without a key, invariant 4 fails and the queue fills with duplicate drafts.

The generic helper `executeIdempotent` hardcodes `responseRef = "change:" + resource` (`internal/service/idempotency.go:142`) and therefore assumes the resource ID exists before execution, which is false for a create. Create must follow the inline claim/complete shape used by `IssuePassportIdempotent` (`idempotency.go:66-106`), keyed on the client-supplied `Idempotency-Key` with the existing `(OrganizationID, ActorID, Operation, Resource, Key)` tuple (`internal/store/idempotency.go:271`) and the existing request-digest conflict check (`server.go:1558`).

## Failure Modes

- **Agent misunderstands the request.** Caught by the human confirmation before submission and by the approver. Produces a discarded draft.
- **Agent produces dangerous SQL.** Caught by the deterministic checker at step 2, yielding `CHECK_FAILED`. The agent cannot override it.
- **Injected instruction in a repository or artifact.** Caught by injection detection, then by the checker, then by the approver.
- **Originator approves their own agent-authored change.** Caught only by the `OriginatorID` comparison. If that comparison is absent or `OriginatorID` is empty, this failure is silent. This is the reason invariant 2 is stated separately from invariant 1.
- **Service account credential leaks.** The holder can create drafts and consume model quota. They cannot approve, issue, or consume a passport. Blast radius is bounded by the actor-type rejection, not by the credential's capability list.
- **Submission retried after a lost response.** Replays the first result once create is idempotent; creates a duplicate change until then.
- **Model unavailable.** Drafting is unavailable; the browser application path is unaffected. The runtime already degrades to a deterministic fallback rather than failing (`internal/agent/runtime.go:892`).
- **Approver rubber-stamps.** Not addressable by code alone. Mitigated by the provenance surface above and detectable by tracking approval latency on agent-authored changes against human-authored ones.

## Test Requirements

Colocated `*_test.go`, per `AGENTS.md`.

- A service account is rejected by `Approve`, `Reject`, `IssuePassport`, and `VerifyGate(consume=true)`, including when it has been granted `review` and the owner role. Rejection must be on actor type.
- The originator cannot approve their own agent-authored change; a different qualified human can.
- An agent-authored change cannot be created with an empty `OriginatorID`.
- `OriginatorID` cannot be set or altered through the request body on create or on any subsequent update.
- Agent-supplied fields cannot set `Risk` away from `RiskUnknown` at creation, and cannot clear a blocking finding.
- Repeated create with the same `Idempotency-Key` yields one change; a differing body with the same key conflicts.
- Concurrent creates with the same key yield one change.
- Organization isolation holds for every service-account operation.
- Attribution to both the service account and the originator survives a restart.
- Injection detection on the submission path refuses rather than warns.

## Deployment Impact

Additive. Existing human submission is unchanged, and `OriginatorID` is empty for human-authored changes, where the existing submitter comparison continues to apply unmodified. A migration adds the column; no backfill is required. The feature is inert until a service account exists.

## Rollback

Revoke the service-account credential. Agent submission stops; all other paths are unaffected. Changes already created remain valid and are approved through the normal path. The schema column may remain in place.

## Open Questions

1. Which credential class backs the service account, given that no API token mechanism exists today (`internal/auth` is session- and OIDC-based only, `auth.go:199`)? Rotation and revocation must be designed with it.
2. How is step 3 authorized without granting the owner role, given that `QueueExperiment` currently requires `技术负责人` (`service.go:717`) and that role short-circuits all capability checks?
3. Is one approver sufficient for agent-authored high-risk changes? The system supports a single `ReviewerID` (`model.go:244-245`) and has no quorum mechanism. Requiring two approvers for this class would need new state.
4. Should agent-authored changes be restricted to a subset of environments or change types before the accuracy baseline exists?
