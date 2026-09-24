"""材料人工确认：记录确认人/时间/版本与内容哈希、幂等、失效，并守住语义边界。

**材料确认 ≠ 治理审批 ≠ 执行许可**：确认只回答"谁在什么时候看过哪一版材料"，
不改变任务状态，也不授予任何执行权利；审批与通行证仍只由 Go 治理服务负责。
"""

from __future__ import annotations

import json

import pytest

from app.config import Settings
from app.schemas.drafts import ConfirmRequest, TaskStatus
from app.service import AgentService, TaskNotConfirmable, TaskNotFound
from app.tools.registry import TrustedContext
from tests.conftest import bare_request, complete_request, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

BLOCKING_DRAFT = json.dumps(
    {
        "sql": "CREATE INDEX idx_orders_user ON orders (user_id);",
        "rollback_sql": "DROP INDEX idx_orders_user;",
        "assumptions": [],
        "open_questions": [],
        "advisory_risk": "LOW",
        "advice_summary": "看起来没问题",
    },
    ensure_ascii=False,
)


class ScriptedProvider:
    """固定输出，用于构造确定的终态（例如被确定性检查阻断）。"""

    name = "scripted"

    def __init__(self, text: str) -> None:
        self._text = text

    def describe(self) -> dict[str, str]:
        return {"provider": self.name, "model": "scripted"}

    async def generate(self, _request) -> str:  # noqa: ANN001
        return self._text


def prepared(settings: Settings) -> tuple[AgentService, str]:
    service = AgentService(settings)
    view, _ = run(service.create_task(complete_request(), CONTEXT))
    assert view.status is TaskStatus.DRAFT_READY, view.error
    assert view.material_hash
    return service, view.task_id


# ---------------------------------------------------------------------------
# 记录内容
# ---------------------------------------------------------------------------


def test_confirm_records_who_when_and_which_material(settings: Settings) -> None:
    service, task_id = prepared(settings)

    confirmed = run(service.confirm(task_id, CONTEXT, ConfirmRequest(note="已核对 SQL 与回滚", material_hash=run(service.get_task(task_id, CONTEXT)).material_hash)))

    assert len(confirmed.confirmations) == 1
    record = confirmed.confirmations[0]
    assert record.confirmed_by == "alice"
    assert record.confirmed_organization == "org_demo"
    assert record.confirmed_at is not None
    assert record.material_version, "必须记录所确认的材料版本"
    assert record.material_hash == confirmed.material_hash, "必须绑定到当前材料内容"
    assert record.note == "已核对 SQL 与回滚"
    assert record.active is True


def test_confirm_is_idempotent_for_the_same_material(settings: Settings) -> None:
    """同一人 + 同一版本 + 同一内容重复确认：不新增记录、不重复写事件。"""
    service, task_id = prepared(settings)

    first = run(service.confirm(task_id, CONTEXT, ConfirmRequest(material_hash=run(service.get_task(task_id, CONTEXT)).material_hash)))
    second = run(service.confirm(task_id, CONTEXT, ConfirmRequest(material_hash=run(service.get_task(task_id, CONTEXT)).material_hash)))

    assert len(first.confirmations) == 1
    assert len(second.confirmations) == 1, "重复确认不得新增记录"
    assert second.confirmations[0].confirmation_id == first.confirmations[0].confirmation_id
    assert [item.kind for item in second.events].count("confirmed") == 1


def test_confirm_does_not_approve_or_grant_execution(settings: Settings) -> None:
    """确认不是审批、也不是执行许可：状态不变，也没有放行语义。"""
    service, task_id = prepared(settings)

    confirmed = run(service.confirm(task_id, CONTEXT, ConfirmRequest(material_hash=run(service.get_task(task_id, CONTEXT)).material_hash)))

    assert confirmed.status is TaskStatus.DRAFT_READY, "材料确认不得改变任务状态"
    detail = " ".join(item.detail for item in confirmed.events if item.kind == "confirmed")
    assert "不构成治理审批" in detail
    assert not hasattr(confirmed, "approval")


# ---------------------------------------------------------------------------
# 前置条件与授权
# ---------------------------------------------------------------------------


def test_confirm_requires_material(settings: Settings) -> None:
    service = AgentService(settings)
    view, _ = run(service.create_task(bare_request(), CONTEXT))
    assert view.status is TaskStatus.NEEDS_INFO

    with pytest.raises(TaskNotConfirmable):
        run(service.confirm(view.task_id, CONTEXT))


def test_confirm_requires_the_creator(settings: Settings) -> None:
    service, task_id = prepared(settings)

    with pytest.raises(TaskNotFound):
        run(service.confirm(task_id, TrustedContext(user_id="bob", organization_id="org_demo")))


# ---------------------------------------------------------------------------
# 失效：新修订使旧确认失效（保留痕跡）
# ---------------------------------------------------------------------------


def test_a_changed_draft_invalidates_the_previous_confirmation(settings: Settings) -> None:
    service, task_id = prepared(settings)
    first = run(service.confirm(task_id, CONTEXT, ConfirmRequest(material_hash=run(service.get_task(task_id, CONTEXT)).material_hash)))
    assert len(first.confirmations) == 1

    # 模拟"重新生成了一份内容不同的草案"。
    record = service._repository.get(task_id)
    record["draft"]["sql"] = record["draft"]["sql"] + "\n-- 重新生成"
    service._repository.save(record)

    second = run(service.confirm(task_id, CONTEXT, ConfirmRequest(note="看过修订版", material_hash=run(service.get_task(task_id, CONTEXT)).material_hash)))

    invalidated = [item for item in second.confirmations if not item.active]
    active = [item for item in second.confirmations if item.active]
    assert len(second.confirmations) == 2, "旧确认必须保留痕跡，不物理删除"
    assert len(invalidated) == 1 and invalidated[0].invalidate_reason
    assert len(active) == 1
    assert active[0].material_hash != invalidated[0].material_hash


def test_confirm_rejects_a_stale_material_hash(settings: Settings) -> None:
    """停留在旧页面的用户不能确认已经更新过的材料。"""
    service, task_id = prepared(settings)

    with pytest.raises(TaskNotConfirmable) as error:
        run(service.confirm(task_id, CONTEXT, ConfirmRequest(material_hash="stale-hash")))
    assert "刷新" in str(error.value), error.value

    # 带上当前哈希则可以确认：所见与所确认一致。
    current = run(service.get_task(task_id, CONTEXT)).material_hash
    confirmed = run(service.confirm(task_id, CONTEXT, ConfirmRequest(material_hash=current)))
    assert confirmed.confirmations, "带上正确哈希时必须可以确认"


def test_input_change_invalidates_stale_results_but_keeps_the_budget() -> None:
    """改输入要让旧结果失效，但**不得**清掉已累计的调用预算。"""
    from app.service import _apply_input_version

    record = {
        "input_version": "stale-version",  # 与按当前 requirement/slots/snapshot 算出的值不同
        "requirement": "给订单表准备索引变更。",
        "slots": {"table": "orders"},
        "schema_snapshot": "",
        "draft": {"sql": "CREATE INDEX ..."},
        "questions": [{"field": "planned_at"}],
        "investigation": {"strategy": "bounded_agent", "stop_reason": "EVIDENCE_SUFFICIENT"},
        "evidence_note": "旧的检索备注",
        "usage": {"requests": 3, "prompt_tokens": 300, "charged_unknown_tokens": 0},
    }

    _apply_input_version(record)

    assert record["input_version"] != "stale-version"
    assert record["draft"] is None, "旧草案必须失效"
    assert record["questions"] == []
    assert record["investigation"] is None, "旧调查轨迹必须失效，不能和新输入混在一起"
    assert record["evidence_note"] is None
    assert record["usage"]["requests"] == 3, "预算必须续算，改输入不等于没花过钱"


def test_changing_input_invalidates_the_previous_confirmation(settings: Settings) -> None:
    """输入/材料版本变化后旧确认失效（走补充信息 → 重算版本 → 重新执行）。"""
    service = AgentService(settings, provider=ScriptedProvider(BLOCKING_DRAFT))
    created, _ = run(service.create_task(complete_request(), CONTEXT))
    assert created.status is TaskStatus.CHECK_BLOCKED, created.error

    confirmed = run(service.confirm(created.task_id, CONTEXT, ConfirmRequest(material_hash=created.material_hash)))
    assert len(confirmed.confirmations) == 1 and confirmed.confirmations[0].active

    # 补充信息改变了槽位 → 输入版本变化 → 旧确认不再适用。
    from app.schemas.drafts import ClarifyRequest

    rerun = run(service.clarify(created.task_id, ClarifyRequest(table="orders_v2"), CONTEXT))
    assert rerun.confirmations, "确认记录必须保留（用于审计）"
    assert all(not item.active for item in rerun.confirmations), "输入变化后旧确认必须失效"
    assert any(item.invalidate_reason for item in rerun.confirmations)
