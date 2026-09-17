"""受约束调查循环：预算是硬边界，完成条件由代码判定。

这里锁定的是"受约束"三个字的实际含义——提示词里写"最多查 3 次"，模型可以不遵守；
下面这些断言检查的是**代码**是否真的拦住了：

- 轮次与**累计**工具调用两个上限；
- 相同工具 + 相同参数再次出现即判定无进展（空转），第二次不会被真的执行；
- 单工具超时按失败处理，不让循环挂住；
- "证据够不够"由确定性判定，**决策者说"够了"不算数**；
- provider 不具备原生动作能力时**显式报告不可用**，不得用规则顶替并宣称是模型决策。
"""

from __future__ import annotations

import asyncio
from dataclasses import replace
from typing import Any

import pytest

from app.config import Settings
from app.llm.provider import DeterministicProvider
from app.schemas.drafts import TaskSlots, TaskStatus, ToolResult
from app.service import AgentService
from app.tools.registry import TrustedContext
from app.workflow.investigate import (
    AskUser,
    BoundedInvestigation,
    CallTool,
    Finish,
    PlannerUnavailable,
    RulePlanner,
    ScriptedPlanner,
    StopReason,
    build_planner,
)
from tests.conftest import complete_request, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")
SLOTS = TaskSlots(application="order-service", environment="生产", table="orders")


def hit(evidence_id: str, doc_id: str = "norms/sql-change-standards") -> dict[str, Any]:
    return {
        "evidence_id": evidence_id,
        "doc_id": doc_id,
        "title": "规范",
        "section": "并发建索引",
        "snippet": "片段",
        "source": f"{doc_id}.md",
        "status": "active",
        "score": 1.0,
    }


class FakeRegistry:
    """记录调用、可按需挂住或失败。"""

    def __init__(
        self,
        hits: dict[str, list[dict[str, Any]]] | None = None,
        *,
        hang: tuple[str, ...] = (),
        failing: tuple[str, ...] = (),
    ) -> None:
        self.calls: list[tuple[str, dict[str, Any]]] = []
        self._hits = hits or {}
        self._hang = set(hang)
        self._failing = set(failing)

    async def call(self, name: str, args: Any, _context: Any) -> ToolResult:
        self.calls.append((name, dict(args or {})))
        if name in self._hang:
            await asyncio.Event().wait()  # 永不返回，用于验证超时
        if name in self._failing:
            return ToolResult(ok=False, tool=name, error="注入的工具失败")
        return ToolResult(ok=True, tool=name, data={"hits": self._hits.get(name, [])})


def build(
    planner: Any,
    registry: FakeRegistry,
    *,
    max_rounds: int = 4,
    max_tool_calls: int = 8,
    tool_timeout: float = 5.0,
) -> BoundedInvestigation:
    return BoundedInvestigation(
        planner=planner,
        registry=registry,
        context=CONTEXT,
        max_rounds=max_rounds,
        max_total_tool_calls=max_tool_calls,
        tool_timeout_seconds=tool_timeout,
    )


async def investigate(loop: BoundedInvestigation):
    return await loop.run(requirement="订单索引", slots=SLOTS)


# ---------------------------------------------------------------------------
# 预算
# ---------------------------------------------------------------------------


def test_round_budget_is_enforced() -> None:
    """轮次上限是硬边界：决策者一直要求调用也不会超出。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": f"q{i}"}) for i in range(10)])
    registry = FakeRegistry()

    outcome = run(investigate(build(planner, registry, max_rounds=2)))

    assert outcome.report.rounds == 2
    assert outcome.report.tool_calls == 2
    assert outcome.report.stop_reason == StopReason.ROUNDS_EXHAUSTED.value


def test_tool_call_budget_is_cumulative_across_rounds() -> None:
    """只限轮次挡不住"一轮里调很多次"，因此累计工具调用是独立上限。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": f"q{i}"}) for i in range(10)])
    registry = FakeRegistry()

    outcome = run(investigate(build(planner, registry, max_rounds=10, max_tool_calls=2)))

    assert outcome.report.tool_calls == 2
    assert len(registry.calls) == 2, "超出预算的工具调用不得真的执行"
    assert outcome.report.stop_reason == StopReason.TOOL_CALLS_EXHAUSTED.value


def test_repeated_identical_call_is_no_progress_and_not_executed_twice() -> None:
    """相同工具 + 相同参数只会得到同样结果，属于空转，第二次不执行。"""
    action = CallTool("search_norms", {"query": "同一个查询"})
    planner = ScriptedPlanner([action, action, action])
    registry = FakeRegistry()

    outcome = run(investigate(build(planner, registry, max_rounds=5)))

    assert len(registry.calls) == 1, f"重复调用不应真的执行，实际 {registry.calls}"
    assert outcome.report.stop_reason == StopReason.NO_PROGRESS.value
    assert any("无进展" in note for note in outcome.report.notes)


def test_same_tool_with_different_arguments_is_allowed() -> None:
    """空转检测只看参数是否相同，不误伤换了参数的重新检索。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "a"}), CallTool("search_norms", {"query": "b"})])
    registry = FakeRegistry()

    outcome = run(investigate(build(planner, registry, max_rounds=5)))

    assert len(registry.calls) == 2
    assert outcome.report.stop_reason != StopReason.NO_PROGRESS.value


def test_tool_timeout_does_not_hang_the_loop() -> None:
    """单工具超时按失败处理：循环必须返回，而不是挂住。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "慢"})])
    registry = FakeRegistry(hang=("search_norms",))

    outcome = run(investigate(build(planner, registry, tool_timeout=0.01)))

    assert outcome.report.stop_reason == StopReason.TOOL_FAILED.value
    assert any("按失败处理" in note for note in outcome.report.notes)


# ---------------------------------------------------------------------------
# 完成条件由代码判定
# ---------------------------------------------------------------------------


def test_decider_cannot_declare_evidence_sufficient() -> None:
    """决策者说"调查完成"不算数：没有任何规范片段就仍是证据不足。"""
    outcome = run(investigate(build(ScriptedPlanner([Finish()]), FakeRegistry())))

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert outcome.report.missing_required, "必须给出缺什么，而不是含糊地结束"


def test_evidence_sufficient_only_when_required_evidence_present() -> None:
    """拿到必需证据后，决策者结束调查才被判为"证据足够"。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry)))

    assert outcome.report.stop_reason == StopReason.EVIDENCE_SUFFICIENT.value
    assert outcome.report.missing_required == []
    assert [item.evidence_id for item in outcome.evidence] == ["norms/x#1"]


def test_exhausted_budget_with_required_evidence_is_not_reported_as_insufficient() -> None:
    """预算用尽但必需证据已经齐了，就不该被说成"证据不足"——如实改判。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}) for _ in range(10)])
    registry = FakeRegistry({"search_norms": [hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry, max_rounds=1)))

    assert outcome.report.stop_reason == StopReason.EVIDENCE_SUFFICIENT.value
    assert outcome.report.missing_required == []


def test_ask_user_stops_with_insufficient_evidence() -> None:
    outcome = run(investigate(build(ScriptedPlanner([AskUser("缺少目标表结构")]), FakeRegistry())))

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("缺少目标表结构" in note for note in outcome.report.notes)


def test_failed_tool_is_recorded_and_does_not_become_success() -> None:
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "x"}), Finish()])
    registry = FakeRegistry(failing=("search_norms",))

    outcome = run(investigate(build(planner, registry)))

    assert outcome.evidence == []
    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("注入的工具失败" in note for note in outcome.report.notes)


# ---------------------------------------------------------------------------
# 决策者能力必须显式声明
# ---------------------------------------------------------------------------


def test_provider_planner_reports_unavailable_when_provider_lacks_the_contract() -> None:
    """provider 只会生成草案文本，不具备动作决策能力——必须显式报告，不能用规则顶替。"""
    with pytest.raises(PlannerUnavailable) as error:
        build_planner(replace(Settings(), investigation_planner="provider"), DeterministicProvider())
    assert "decide" in str(error.value)


def test_rule_planner_is_labelled_as_rules_not_as_a_model() -> None:
    """规则决策者不得被包装成"模型的决定"。"""
    planner = build_planner(Settings(), DeterministicProvider())
    assert isinstance(planner, RulePlanner)
    assert planner.name == "rule"


# ---------------------------------------------------------------------------
# 接进工作流
# ---------------------------------------------------------------------------


def test_default_strategy_is_the_existing_fixed_workflow() -> None:
    """新循环在验证充分前不改变既有行为。"""
    assert Settings().investigation_strategy == "fixed_workflow"


def test_bounded_agent_strategy_produces_a_draft_and_reports_its_trace(settings: Settings) -> None:
    scoped = replace(settings, investigation_strategy="bounded_agent", max_revisions=0)
    service = AgentService(scoped)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.DRAFT_READY, view.error
    assert view.draft is not None
    details = " ".join(item.detail for item in view.events)
    assert "调查循环" in details, "循环的决策者与停止原因必须留在事件里"
    assert "planner=rule" in details, "必须标明决策者是规则而不是模型"


def test_bounded_agent_reports_planner_unavailable_instead_of_faking_a_model_decision(
    settings: Settings,
) -> None:
    """要求模型决策但 provider 不具备该能力时：明确报告未启动，且不说成是模型的选择。"""
    scoped = replace(
        settings, investigation_strategy="bounded_agent", investigation_planner="provider", max_revisions=0
    )
    service = AgentService(scoped)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    details = " ".join(item.detail for item in view.events)
    assert "调查循环未启动" in details
    assert "decide" in details
    # 没有可引用片段时必须如实标注，而不是装作调查过了。
    assert view.status in {TaskStatus.DRAFT_READY, TaskStatus.CHECK_BLOCKED}
    assert (view.draft is not None) and not view.draft.evidence
