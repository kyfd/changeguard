# Agent-Authored Change Submission: Implementation Design

> **状态：延后提案（Deferred）。首版不实现。**
>
> 首版边界是"模型工具只读 + 确定性编排在已认证业务接口上写入"。
> 本文描述的"服务账号代提"属于第二阶段扩展，排在首版之后。
>
> 阅读前请先看 `docs/agent-baseline.md` 第 4 节：本文有两处与当前代码不符——
> Stage 1 的第 4 个调用点（`VerifyGate`）在该路径上**没有会话 actor 可检查**；
> "前置阶段无外部副作用"也不成立（`QueueExperiment` 会在影子库真实执行 SQL）。

Companion to `docs/adr/0002-agent-authored-change-submission.md`. The ADR fixes the invariants; this document is the build order.

Sequencing rule: **every change that widens capability is preceded by the control that bounds it.** The service account is not created before the actor-type rejection exists, and the agent is not wired to the write path before the originator check exists.

---

## Stage 1 — Actor type and the closed door

Ship the refusal before anything can be refused.

**`internal/model`**

Add an actor type to `model.User`, defaulting to human so every existing row and session keeps its current meaning:

```go
type ActorType string

const (
    ActorHuman   ActorType = "HUMAN"
    ActorService ActorType = "SERVICE"
)
```

`model.AuditEvent` already carries `ActorType` and `AuthMethod` (`internal/model/model.go:277-278`); populate them instead of adding fields.

**`internal/service`**

A single guard, applied at the top of each privileged transition, before capability and role checks:

```go
func requireHumanActor(actor model.User) error {
    if actor.Type == model.ActorService {
        return ErrForbidden
    }
    return nil
}
```

Call sites — these four and no others:

| Function | File |
|---|---|
| `Approve` | `internal/service/service.go:1113` |
| `Reject` | `internal/service/service.go:1174` |
| `IssuePassport` | `internal/service/passport.go:69` |
| `VerifyGate` when `consume=true` | `internal/service/passport.go:145` |

Ordering matters: the actor-type check must run before `canUseApplication` and before any role comparison, so that a misconfigured grant or an accidental `技术负责人` role on a service account cannot reach the privileged path.

**Tests before Stage 2 begins.** Construct a service-account user, grant it `review` and the owner role, and assert all four functions still return `ErrForbidden`. This test is what makes the rest of the plan safe; it should fail loudly if anyone later reorders the checks.

---

## Stage 2 — Originator and the separation-of-duties repair

This stage closes the silent gap named in the ADR. It lands before submission is possible.

**Schema.** Add `originator_id` to the change table, nullable, no backfill. Human-authored rows keep it empty and are unaffected.

**`model.ChangeRequest`.** Add `OriginatorID string` with `json:"originator_id"` (snake_case per `AGENTS.md`). It is response-only: the field must be ignored if present in `CreateChangeInput`, and set by the server from the authenticated originator context.

**`internal/service/service.go`, in `Create`.** Alongside the existing forced `Status`/`Risk`/`Version` assignment (`service.go:497-498`):

- when the actor is a service account, require a non-empty originator and reject the create otherwise — provenance is mandatory, not best-effort;
- when the actor is human, leave `OriginatorID` empty.

**The comparison.** In `Approve` (`service.go:1128`), `Reject` (`service.go:1180`), and `IssuePassport` (`passport.go:84`), extend the existing submitter check:

```go
if actor.ID == change.SubmitterID {
    return ErrForbidden
}
if change.OriginatorID != "" && actor.ID == change.OriginatorID {
    return ErrForbidden
}
```

All three sites, not just approve. Passport issuance already re-checks the submitter; leaving it out would let the originator issue for a change they requested.

Immutability: no update path may write `OriginatorID` after creation. Add a test that attempts it through every mutating endpoint.

---

## Stage 3 — Idempotent create

**`internal/httpapi/server.go`, POST branch of `handleChanges` (`server.go:1102`).** Reuse `validatedIdempotencyKey(w, r)` (`server.go:1568`) and `requestDigest` (`server.go:1558`), matching the approve handler's shape (`server.go:1246`). Absent key keeps today's behaviour and sets `Idempotency-Status: not-requested`.

**`internal/service/idempotency.go`.** `executeIdempotent` hardcodes `responseRef = "change:" + resource` (`idempotency.go:142`), which assumes the resource ID exists before execution. For create it does not. Follow the inline claim/complete structure of `IssuePassportIdempotent` (`idempotency.go:66-106`): claim on the key, execute, then complete with the resulting change ID as the response reference.

The uniqueness tuple `(OrganizationID, ActorID, Operation, Resource, Key)` (`internal/store/idempotency.go:271`) needs a stable `Resource` for creates, where no ID exists yet. Use a fixed operation-scoped constant so the key alone discriminates within an actor and organization.

Replay returns the original change with `Idempotency-Replayed: true`. A same-key different-body request conflicts through the existing digest comparison.

Tests: sequential retry yields one change; concurrent retry yields one change; differing body with the same key conflicts.

---

## Stage 4 — Service account credential

`internal/auth` is session-cookie and OIDC only (`auth.go:199`); there is no token mechanism to extend. This stage is the largest unknown in the plan and is deliberately placed after the controls that bound it.

Requirements:

- bearer credential, stored hashed, never recoverable after issue — the passport pattern of storing only a SHA256 (`passport.go:131`) applies;
- resolves to a `model.User` with `Type = ActorService`, an organization, and `submit` only;
- CSRF is not applicable; the existing cookie CSRF path (`auth.go:213`) must not be loosened to accommodate it;
- creation and revocation restricted to `EnterpriseAdmin`, both audited;
- rotation without downtime, meaning two valid credentials during overlap;
- the originator is carried per request, distinct from the credential, and is authenticated rather than self-asserted.

The last point is the one to get right: if the originator can be claimed by the caller, Stage 2's check is defeated by a caller that names someone else. The originator must come from a verified human session or an equivalently verified assertion — not from a request field the agent fills in.

---

## Stage 5 — Experiment authorization

`QueueExperiment` requires role `技术负责人` (`service.go:717`). Granting that role to a service account is not an option: `canUseApplication` short-circuits on `RoleOwner` (`service.go:1805-1826`), which would hand the account blanket `submit` and `review` across every application in the organization — precisely the escalation Stage 1 exists to prevent.

Two viable paths:

1. Introduce a distinct `experiment` capability alongside `submit` and `review`, grantable per application, and check it in `QueueExperiment` instead of the role. Cleaner, and it fixes an existing coarseness in the model.
2. Leave step 3 to the human. The agent drives create and submit, then reports that the change is ready for a shadow run.

Path 2 is the smaller change and is the safer default for the first release; path 1 is the better end state. Note that `"submit"` and `"review"` are bare literals today, not constants (`service.go:1805-1826`) — introducing a third is a good moment to make them constants.

---

## Stage 6 — Orchestration

Only after Stages 1–5. The agent performs, per the sequence in the ADR:

1. draft, and present for human confirmation — nothing is written before this;
2. `POST /api/changes` with an idempotency key and the originator context;
3. `POST /api/changes/{id}/submit`;
4. report the outcome.

Rules:

- `CHECK_FAILED` is a reportable result, never a condition to retry around or work around by editing the SQL and resubmitting without the human seeing the finding;
- a change stuck between states is reported as-is; the agent does not invent compensating actions;
- the agent never polls toward approval or prompts the approver;
- any injection detected on this path is a refusal, not a warning, and is audited.

---

## Stage 7 — Approval surface

The single human gate is now the only thing between a model-generated migration and production. Code cannot prevent rubber-stamping; the interface can make provenance unavoidable.

On the approval view for an agent-authored change:

- a clear marker that the change was agent-authored, with the originating human named;
- the originator's verbatim request shown next to the generated artifact;
- agent-generated fields visually distinct from human-entered ones;
- a link to the conversation and tool-call trace;
- generated SQL diffed against the repository artifact behind the digest;
- for `HIGH` risk, `get_rule_findings` expanded and not collapsible;
- `NOT_RUN` and `DEMO_ONLY` rendered distinctly from an executed shadow run, per `AGENTS.md`.

Per `AGENTS.md`, none of this may be the only place an invariant lives — the browser application is a presentation layer, and every rule above is additionally enforced in the service path.

---

## Verification

Per-stage focused tests, then the full suite before a pull request:

```
go test ./internal/service -run TestServiceAccount
go test ./internal/service -run TestOriginator
go test ./internal/service -run TestCreateIdempotent
go test ./...
go vet ./...
go test -race ./...
npm test
```

Coverage required by the ADR, colocated as `*_test.go`:

- service account refused by all four privileged functions even with `review` and the owner role;
- originator cannot approve, reject, or issue for their own agent-authored change; another qualified human can;
- agent-authored create with empty originator is rejected;
- `OriginatorID` unsettable through the request body, immutable after create;
- agent input cannot move `Risk` off `RiskUnknown` or clear a blocking finding;
- create idempotency: sequential, concurrent, and conflicting-body cases;
- organization isolation on every service-account operation;
- attribution survives restart;
- injection on the submission path refuses.

Tests requiring PostgreSQL or Redis skip explicitly when the DSN is absent, and a skipped integration test is not evidence that the environment passed.

---

## Deployment and Rollback

Additive throughout. One nullable column; no backfill. Human submission is untouched, and empty `OriginatorID` preserves existing behaviour exactly. The feature is inert until a service account exists.

Rollback is credential revocation: agent submission stops immediately, every other path is unaffected, and changes already created are approved normally.

---

## What This Design Does Not Do

Stated so that scope creep is visible if it happens later:

- no model-driven approval, issuance, consumption, deployment, or rollback — the `AGENTS.md` prohibitions are unchanged;
- no write access for `internal/agent` tools; `DataSource` (`tools.go:16-20`) stays read-only and the submission path goes through the ordinary authenticated API;
- no change to risk classification: `AdvisoryRisk` still never sets `ChangeRequest.Risk` (`model.go:193-195`);
- no quorum approval — single `ReviewerID` remains (`model.go:244-245`), and the ADR leaves multi-approver for high-risk agent changes as an open question;
- no accuracy benchmark against live models; the offline evaluation (`cmd/changeguard-agent-eval`) still validates protocol and safety only, and is not evidence of drafting quality.
