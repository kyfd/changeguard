"""草案契约：模型不能自封"已确认"、版本要对应实际修订、方言要说清支持范围。

这些缺陷此前没有任何测试覆盖，因此长期存在：

- `parse_model_draft` 直接把模型输出的 `confirmed` 采信进 `Assumption`，
  而"已获人工确认"是**人的结论**；离线确定性生成器自己也写过 `confirmed: true`。
- 草案 `version` 硬编码为 1，`revision_notes` 字段存在但从未被写入——
  一份改了三轮的草案看起来仍像第一版。
- 非 PostgreSQL 的目标也会拿到 `CREATE INDEX CONCURRENTLY`、`SET lock_timeout`
  这类 PG 专属语法，并且被标成可用的草案。
- `ClarifyRequest.note` 被接收后直接丢弃；`schema_snapshot` 根本无法通过补充信息补齐。
"""

from __future__ import annotations

import json
from dataclasses import replace
from typing import Any

import pytest

from app.config import Settings
from app.llm.provider import DeterministicProvider, DraftRequest
from app.retrieval.corpus import build_retriever
from app.schemas.drafts import (
    ClarifyRequest,
    DatabaseKind,
    TaskStatus,
    TaskSlots,
)
from app.service import AgentService
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.workflow.graph import DraftParseError, DraftWorkflow, WorkflowDeps, parse_model_draft
from tests.conftest import (
    DEMO_DIR,
    SCHEMA_SNAPSHOT,
    bare_request,
    complete_request,
    complete_slots,
    run,
)

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

# v1 缺 lock_timeout，会被确定性扫描阻断；v2 补齐后可以通过——用来制造一次真实修订。
BLOCKED_DRAFT = json.dumps(
    {
        "sql": "CREATE INDEX CONCURRENTLY idx_orders_user ON orders (user_id);",
        "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user;",
        "assumptions": [],
        "open_questions": [],
        "advisory_risk": "MEDIUM",
        "advice_summary": "缺少 lock_timeout",
    },
    ensure_ascii=False,
)

CLEAN_DRAFT = json.dumps(
    {
        "sql": (
            "SET lock_timeout = '3s';\n\n"
            "CREATE INDEX CONCURRENTLY idx_orders_user_created ON orders (user_id, created_at DESC);"
        ),
        "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
        "assumptions": [{"statement": "索引列按查询形态推断", "needs_confirmation": True}],
        "open_questions": [],
        "advisory_risk": "LOW",
        "advice_summary": "已使用并发建索引",
    },
    ensure_ascii=False,
)


class ScriptedProvider:
    """按顺序返回固定文本，并记录每次收到的请求。"""

    name = "scripted"

    def __init__(self, texts: list[str]) -> None:
        self._texts = list(texts)
        self.requests: list[DraftRequest] = []

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "scripted"}

    async def generate(self, request: DraftRequest) -> str:
        self.requests.append(request)
        index = min(len(self.requests) - 1, len(self._texts) - 1)
        return self._texts[index]


def assumption_payload(*, extra: dict[str, Any] | None = None) -> str:
    assumption: dict[str, Any] = {"statement": "该表为热表"}
    if extra:
        assumption.update(extra)
    return json.dumps(
        {
            "sql": "SET lock_timeout = '3s';\n\nCREATE INDEX CONCURRENTLY idx_x ON orders (user_id);",
            "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_x;",
            "assumptions": [assumption],
            "open_questions": [],
            "advisory_risk": "LOW",
            "advice_summary": "占位",
        },
        ensure_ascii=False,
    )


# ---------------------------------------------------------------------------
# C2：模型不能自封"已获人工确认"
# ---------------------------------------------------------------------------


def test_model_supplied_confirmed_is_rejected() -> None:
    """`confirmed` 表示已获人工确认，模型无权声明——直接拒绝，不静默忽略。"""
    with pytest.raises(DraftParseError) as error:
        parse_model_draft(
            assumption_payload(extra={"confirmed": True}),
            requirement="x",
            slots=complete_slots(),
            evidence_pool=[],
        )
    assert "未声明字段" in str(error.value)


def test_model_supplied_confirmed_false_is_also_rejected() -> None:
    """写 False 同样是未声明字段：允许它出现就意味着允许 True 出现。"""
    with pytest.raises(DraftParseError):
        parse_model_draft(
            assumption_payload(extra={"confirmed": False}),
            requirement="x",
            slots=complete_slots(),
            evidence_pool=[],
        )


def test_assumption_flags_are_server_decided() -> None:
    """模型说"不需要确认"也不作数：未获确认的假设就需要确认。"""
    draft = parse_model_draft(
        assumption_payload(extra={"needs_confirmation": False}),
        requirement="x",
        slots=complete_slots(),
        evidence_pool=[],
    )
    assert len(draft.assumptions) == 1
    assert draft.assumptions[0].confirmed is False
    assert draft.assumptions[0].needs_confirmation is True


def test_deterministic_provider_never_marks_an_assumption_confirmed(settings: Settings) -> None:
    """离线生成器曾把热表假设标成"已确认"——那正是要修掉的失败模式。"""
    provider = DeterministicProvider()
    request = DraftRequest(
        requirement="热表索引变更",
        slots=complete_slots(),
        schema_snapshot="orders 是热表，写入量很高。",
    )
    text = run(provider.generate(request))
    draft = parse_model_draft(text, requirement="x", slots=complete_slots(), evidence_pool=[])
    assert draft.assumptions, "该输入应当产生假设"
    assert all(item.confirmed is False for item in draft.assumptions)
    assert all(item.needs_confirmation is True for item in draft.assumptions)


# ---------------------------------------------------------------------------
# C3：版本与修订说明对应实际修订
# ---------------------------------------------------------------------------


def test_revised_draft_reports_its_real_version_and_notes(settings: Settings) -> None:
    """经过一轮修订的草案必须显示为第 2 版，并说明这一版改了什么。"""
    scoped = replace(settings, max_revisions=2)
    provider = ScriptedProvider([BLOCKED_DRAFT, CLEAN_DRAFT])
    workflow = DraftWorkflow(
        WorkflowDeps(
            settings=scoped,
            provider=provider,
            trusted_context=CONTEXT,
            toolbox_factory=lambda snapshot: Toolbox(
                settings=scoped, retriever=build_retriever(scoped.demo_dir), schema_snapshot=snapshot
            ),
        )
    )
    state = {
        "task_id": "t_contract",
        "requirement": "订单索引",
        "slots": complete_request().model_dump(mode="json"),
        "schema_snapshot": SCHEMA_SNAPSHOT,
        "max_revisions": scoped.max_revisions,
        "events": [],
        "revisions": 0,
    }
    result = run(workflow.run(state))  # type: ignore[arg-type]

    draft = result.get("draft") or {}
    assert len(provider.requests) >= 2, "该输入应当产生一次修订"
    assert draft.get("version") == 2, f"经过一轮修订的草案应为第 2 版，实际 {draft.get('version')}"
    assert draft.get("revision_notes"), "修订版必须说明它响应了哪些确定性检查反馈"


# ---------------------------------------------------------------------------
# C6：方言支持范围
# ---------------------------------------------------------------------------


def test_unsupported_dialect_stops_instead_of_emitting_postgres_syntax(settings: Settings) -> None:
    """MySQL 目标必须停在支持边界，而不是拿到 PG 专属语法并被标成可用草案。"""
    service = AgentService(settings)
    request = complete_request(database=DatabaseKind.MYSQL)

    view, _ = run(service.create_task(request, CONTEXT))

    assert view.status is TaskStatus.FAILED, f"不支持的方言应停下，实际 {view.status}"
    assert view.draft is None, "不得为该方言产出草案"
    assert "PostgreSQL" in (view.error or ""), view.error


def test_unsupported_dialect_does_not_reach_scanning(settings: Settings) -> None:
    """停在入口，因此不会拿 PG 规则去扫 MySQL 输入。"""
    service = AgentService(settings)
    view, _ = run(service.create_task(complete_request(database=DatabaseKind.MYSQL), CONTEXT))
    kinds = [item.kind for item in (view.events or []) if hasattr(item, "kind")]
    assert "run_check" not in kinds, f"不支持的方言不应进入确定性扫描：{kinds}"


def test_postgresql_target_still_produces_a_draft(settings: Settings) -> None:
    """支持范围内的目标不受影响——这条防止把"加边界"做成"全都拒绝"。"""
    service = AgentService(settings)
    view, _ = run(service.create_task(complete_request(database=DatabaseKind.POSTGRESQL), CONTEXT))
    assert view.status is TaskStatus.DRAFT_READY, view.error
    assert view.draft is not None


def test_deterministic_provider_refuses_a_foreign_dialect() -> None:
    """即使绕过工作流直接调用，生成器也不会产出方言不匹配的 SQL。"""
    provider = DeterministicProvider()
    slots = TaskSlots(
        application="order-service",
        environment="生产",
        database=DatabaseKind.MYSQL,
        table="orders",
    )
    with pytest.raises(RuntimeError):
        run(provider.generate(DraftRequest(requirement="x", slots=slots)))


# ---------------------------------------------------------------------------
# C4 / C5：补充信息的两个字段
# ---------------------------------------------------------------------------


def full_clarify(**overrides: Any) -> ClarifyRequest:
    """一份信息完整的补充请求，便于让工作流真的走到生成阶段。"""
    request = complete_request()
    data: dict[str, Any] = {
        "application": request.application,
        "environment": request.environment,
        "database": request.database,
        "table": request.table,
        "query_sql": request.query_sql,
        "planned_at": request.planned_at,
        "planned_at_timezone": request.planned_at_timezone,
        "schema_snapshot": request.schema_snapshot,
    }
    data.update(overrides)
    return ClarifyRequest(**data)


def test_clarification_note_reaches_the_model(settings: Settings) -> None:
    """`note` 以前被接收后丢弃；现在必须写进记录并出现在模型看到的需求文本里。"""
    provider = ScriptedProvider([CLEAN_DRAFT])
    service = AgentService(replace(settings, max_revisions=0), provider=provider)

    view, _ = run(service.create_task(bare_request(), CONTEXT))
    assert view.status is TaskStatus.NEEDS_INFO

    note = "本次变更必须在 21:30 之前完成，且不得影响订单查询。"
    run(service.clarify(view.task_id, full_clarify(note=note), CONTEXT))

    stored = service._repository.get(view.task_id)
    assert note in (stored.get("clarification_notes") or []), "补充说明必须写入任务记录"
    assert provider.requests, "提供完整槽位后应当走到生成阶段"
    assert note in provider.requests[-1].requirement, "补充说明必须出现在模型看到的需求文本里"


def test_clarification_can_supply_the_schema_snapshot(settings: Settings) -> None:
    """缺快照时必须能通过补充信息补齐，否则这是一条走不通的死路。"""
    provider = ScriptedProvider([CLEAN_DRAFT])
    service = AgentService(replace(settings, max_revisions=0), provider=provider)

    view, _ = run(service.create_task(bare_request(), CONTEXT))
    assert view.status is TaskStatus.NEEDS_INFO

    snapshot = "CREATE TABLE orders (id bigint, user_id bigint, created_at timestamptz);"
    run(service.clarify(view.task_id, full_clarify(schema_snapshot=snapshot), CONTEXT))

    stored = service._repository.get(view.task_id)
    assert stored.get("schema_snapshot") == snapshot, "补充的表结构快照必须生效"
    assert provider.requests[-1].schema_snapshot == snapshot, "快照必须传给模型"


def test_clarify_without_snapshot_keeps_the_existing_one(settings: Settings) -> None:
    """只提供槽位时不覆盖已有快照——避免把材料清空。"""
    provider = ScriptedProvider([CLEAN_DRAFT])
    service = AgentService(replace(settings, max_revisions=0), provider=provider)

    view, _ = run(service.create_task(bare_request(schema_snapshot=SCHEMA_SNAPSHOT), CONTEXT))
    run(service.clarify(view.task_id, full_clarify(schema_snapshot=None), CONTEXT))

    assert service._repository.get(view.task_id).get("schema_snapshot") == SCHEMA_SNAPSHOT
