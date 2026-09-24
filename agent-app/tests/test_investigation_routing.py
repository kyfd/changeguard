"""调查结论必须真正控制工作流。

上一版的问题：调查循环只把结论写进事件日志，工作流照常走到草稿生成，于是
下面三种情况都返回 DRAFT_READY 且产出了草案：

1. 决策者不可用（provider 没有原生动作能力）；
2. 决策者要求用户补充信息（AskUser）；
3. 工具预算为零且必需证据缺失。

这些用例断言的是**运行结果**——最终状态、追问内容、草稿是否为空、
生成器被调用了多少次——而不是"事件日志里出现了某个词"。
"""

from __future__ import annotations

import json
from dataclasses import replace
from typing import Any

import pytest

from app.config import Settings
from app.llm.provider import DeterministicProvider
from app.schemas.drafts import TaskStatus
from app.service import AgentService
from app.tools.registry import TrustedContext
from app.workflow import graph as graph_module
from app.workflow.investigate import AskUser, CallTool, Finish, ScriptedPlanner
from tests.conftest import complete_request, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

VALID_DRAFT = json.dumps(
    {
        "sql": (
            "SET lock_timeout = '3s';\n\n"
            "CREATE INDEX CONCURRENTLY idx_orders_user_created ON orders (user_id, created_at DESC);"
        ),
        "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
        "assumptions": [],
        "open_questions": [],
        "advisory_risk": "LOW",
        "advice_summary": "占位结论",
    },
    ensure_ascii=False,
)


class CountingProvider:
    """记录生成调用次数——用来证明"没有进入草稿生成"。"""

    name = "counting"

    def __init__(self) -> None:
        self.generate_calls = 0

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "counting"}

    async def generate(self, _request: Any) -> str:
        self.generate_calls += 1
        return VALID_DRAFT


def bounded(settings: Settings, provider: Any, **overrides: Any) -> AgentService:
    scoped = replace(settings, investigation_strategy="bounded_agent", max_revisions=0, **overrides)
    return AgentService(scoped, provider=provider)


@pytest.mark.parametrize('failure', ['planner', 'tool'])
def test_failure_after_sufficient_evidence_never_generates(settings, monkeypatch, failure):
    from app.workflow.investigate import PlannerDecisionError
    class FailingPlanner(ScriptedPlanner):
        async def plan(self, *, round_index, **kwargs):
            if round_index == 1:
                return CallTool('search_norms', {'query': '索引', 'limit': 3})
            if failure == 'planner':
                raise PlannerDecisionError('invalid decision')
            return CallTool('get_change_context', {'change_id': 'missing'})
    use_planner(monkeypatch, FailingPlanner([]))
    provider = CountingProvider()
    service = bounded(settings, provider)
    view, _ = run(service.create_task(complete_request(), CONTEXT))
    assert view.status is TaskStatus.FAILED
    assert view.draft is None
    assert provider.generate_calls == 0


def use_planner(monkeypatch: pytest.MonkeyPatch, planner: Any) -> None:
    """替换决策者的构造，其余流程保持真实。"""
    monkeypatch.setattr(graph_module, "build_planner", lambda *_args, **_kwargs: planner)


# ---------------------------------------------------------------------------
# 1. 决策者不可用
# ---------------------------------------------------------------------------


def test_unavailable_planner_fails_and_never_generates_a_draft(settings: Settings) -> None:
    provider = CountingProvider()
    # 真实路径：investigation_planner=provider，而 CountingProvider 没有 decide()。
    service = bounded(settings, provider, investigation_planner="provider")

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.FAILED, f"决策者不可用必须明确失败，实际 {view.status}"
    assert view.draft is None, "不得在决策者不可用时产出草案"
    assert provider.generate_calls == 0, f"不得进入草稿生成，实际调用 {provider.generate_calls} 次"
    assert "decide" in (view.error or ""), view.error
    # 没有被伪装成"模型的选择"
    assert "planner=unavailable" in " ".join(item.detail for item in view.events)


# ---------------------------------------------------------------------------
# 2. 决策者要求补充信息
# ---------------------------------------------------------------------------


def test_clarification_request_enters_needs_info_with_questions(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    use_planner(monkeypatch, ScriptedPlanner([AskUser("缺少目标表的结构快照，请补充")]))
    provider = CountingProvider()
    service = bounded(settings, provider)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.NEEDS_INFO, f"应进入待补充状态，实际 {view.status}"
    assert view.draft is None, "等待补充时不得产出草案"
    assert provider.generate_calls == 0, f"不得进入草稿生成，实际调用 {provider.generate_calls} 次"
    assert view.questions, "必须把决策者的补充要求转成追问"
    assert "缺少目标表的结构快照，请补充" in " ".join(item.question for item in view.questions)


def test_user_can_continue_after_supplying_the_requested_information(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """补充信息后能够继续推进——否则 NEEDS_INFO 就是一条死路。"""
    planner = ScriptedPlanner([AskUser("请补充表结构快照")])
    use_planner(monkeypatch, planner)
    provider = CountingProvider()
    service = bounded(settings, provider)

    view, _ = run(service.create_task(complete_request(), CONTEXT))
    assert view.status is TaskStatus.NEEDS_INFO

    # 用户补充后重跑：这次决策者直接调用规范检索并结束，证据齐备 → 可以生成。
    planner._actions = [CallTool("search_norms", {"query": "索引", "limit": 3}), Finish()]
    planner.seen_rounds.clear()
    from app.schemas.drafts import ClarifyRequest

    continued, _ = None, None
    continued = run(service.clarify(view.task_id, ClarifyRequest(table="orders"), CONTEXT))

    assert continued.status is not TaskStatus.NEEDS_INFO, "补充后不应仍停在待补充"
    assert provider.generate_calls >= 1, "补充后应能继续到生成阶段"


# ---------------------------------------------------------------------------
# 3. 预算为零且必需证据缺失
# ---------------------------------------------------------------------------


def test_zero_tool_budget_with_missing_evidence_blocks_the_draft(settings: Settings) -> None:
    provider = CountingProvider()
    service = bounded(settings, provider, max_total_tool_calls=0, max_investigation_rounds=1)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.FAILED, f"必需证据缺失不得进入草稿生成，实际 {view.status}"
    assert view.draft is None, "必需证据缺失时不得产出草案"
    assert provider.generate_calls == 0, f"不得进入草稿生成，实际调用 {provider.generate_calls} 次"
    assert "必需证据缺失" in (view.error or ""), view.error


def test_missing_evidence_is_reported_with_what_is_missing(settings: Settings) -> None:
    """只说"证据不足"没用，必须说清缺什么。"""
    service = bounded(settings, CountingProvider(), max_total_tool_calls=0, max_investigation_rounds=1)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert "规范片段" in (view.error or ""), view.error


def test_insufficient_evidence_from_the_decider_also_blocks(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """决策者直接宣告完成、但没有任何规范证据时，同样不得生成草案。"""
    use_planner(monkeypatch, ScriptedPlanner([Finish()]))
    provider = CountingProvider()
    service = bounded(settings, provider)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.FAILED
    assert view.draft is None
    assert provider.generate_calls == 0


# ---------------------------------------------------------------------------
# 护栏：加边界不能变成"全都拒绝"
# ---------------------------------------------------------------------------


def test_supported_investigation_still_produces_a_draft(settings: Settings) -> None:
    provider = CountingProvider()
    service = bounded(settings, provider)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.DRAFT_READY, view.error
    assert view.draft is not None
    assert provider.generate_calls >= 1


def test_fixed_workflow_behaviour_is_unchanged(settings: Settings) -> None:
    """默认策略不受影响：它的检索路径没有"决策者不可用"这类概念。"""
    provider = CountingProvider()
    service = AgentService(replace(settings, investigation_strategy="fixed_workflow", max_revisions=0), provider=provider)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.DRAFT_READY, view.error
    assert view.draft is not None
    assert provider.generate_calls >= 1
    assert DeterministicProvider is not None  # 保持导入：说明默认路径用的是真实生成器
