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
from app.schemas.drafts import DatabaseKind, TaskSlots, TaskStatus, ToolResult
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
SLOTS = TaskSlots(application="order-service", environment="生产", database=DatabaseKind.POSTGRESQL, table="orders")


def hit(evidence_id: str, doc_id: str = "norms/sql-change-standards") -> dict[str, Any]:
    return {
        "evidence_id": evidence_id,
        "doc_id": doc_id,
        "title": "规范",
        "section": "并发建索引",
        "snippet": "片段",
        "source": f"{doc_id}.md",
        # 规范片段必须带可核对身份（版本或来源），否则必需证据判定会判为不足。
        "version": "v1.0",
        "status": "active",
        "score": 1.0,
        # 适用范围必须标注且覆盖目标数据库，否则不能作为必需证据（unknown 不等于有效）。
        "applicability": "PostgreSQL 生产库",
    }


def deprecated_hit(evidence_id: str) -> dict[str, Any]:
    payload = hit(evidence_id)
    payload["status"] = "deprecated"
    return payload


def unversioned_hit(evidence_id: str) -> dict[str, Any]:
    payload = hit(evidence_id)
    payload["version"] = ""
    payload["source"] = ""
    return payload


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


SNAPSHOT = "CREATE TABLE orders (id bigint, user_id bigint, created_at timestamptz);"


async def investigate(loop: BoundedInvestigation, *, schema_snapshot: str = SNAPSHOT):
    return await loop.run(requirement="订单索引", slots=SLOTS, schema_snapshot=schema_snapshot)


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
    assert outcome.report.stop_reason == StopReason.TOOL_FAILED.value
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
# 必需证据判定：不止看 norms/ 前缀
# ---------------------------------------------------------------------------


def test_deprecated_norms_alone_do_not_count_as_sufficient() -> None:
    """命中的规范全部已废弃时不得判为"够了"——前缀一样，但引用的是失效条款。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [deprecated_hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry)))

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("废弃" in item for item in outcome.report.missing_required), outcome.report.missing_required


def test_norms_without_version_or_source_are_insufficient() -> None:
    """规范片段必须带可核对身份，否则无法说明依据的是哪一版。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [unversioned_hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry)))

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("版本或来源" in item for item in outcome.report.missing_required)


def test_targeted_table_without_snapshot_is_insufficient() -> None:
    """指定了目标表却没有快照时无法核对字段名，不得判为证据足够。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry), schema_snapshot=""))

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("结构快照" in item for item in outcome.report.missing_required)


# ---------------------------------------------------------------------------
# 证据的**适用性**：不是"命中 norms/ 且快照非空"就算够
# ---------------------------------------------------------------------------


def _sufficient_outcome(hit_payload: dict[str, Any], *, snapshot: str = SNAPSHOT):
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [hit_payload]})
    loop = build(planner, registry)
    return run(loop.run(requirement="订单索引", slots=SLOTS, schema_snapshot=snapshot))


def test_snapshot_without_the_target_table_is_insufficient() -> None:
    """快照非空但里面没有目标表 → 不能据此生成草案。"""
    other = "CREATE TABLE audit_log (id bigint);"
    outcome = _sufficient_outcome(hit("norms/x#1"), snapshot=other)

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("未找到目标表" in item for item in outcome.report.missing_required), outcome.report.missing_required


def test_unparseable_snapshot_is_insufficient() -> None:
    """解析不出任何表定义时，必须显式判为"无法核对"，不能当成已核对。"""
    outcome = _sufficient_outcome(hit("norms/x#1"), snapshot="字段：id, user_id, created_at")

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("未能识别出任何表定义" in item for item in outcome.report.missing_required), outcome.report.missing_required


def test_norm_with_mismatched_applicability_is_insufficient() -> None:
    """规范适用范围是 MySQL，而目标是 PostgreSQL → 不能作为必需证据。"""
    payload = hit("norms/x#1")
    payload["applicability"] = "MySQL 生产库"
    outcome = _sufficient_outcome(payload)

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("适用范围" in item for item in outcome.report.missing_required), outcome.report.missing_required


def test_norm_without_applicability_is_insufficient() -> None:
    """文档没写适用范围 = 无法核对 = 不算适用（unknown 不等于有效）。"""
    payload = hit("norms/x#1")
    payload["applicability"] = ""
    outcome = _sufficient_outcome(payload)

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("适用范围" in item for item in outcome.report.missing_required), outcome.report.missing_required


def test_unknown_status_norm_is_not_treated_as_valid() -> None:
    """状态未知的规范不算有效证据。"""
    payload = hit("norms/x#1")
    payload["status"] = "unknown"
    outcome = _sufficient_outcome(payload)

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert any("未知" in item for item in outcome.report.missing_required), outcome.report.missing_required


def test_applicability_is_scoped_per_database() -> None:
    """同一个适用范围文本对不同目标库的判定必须一致且可解释。"""
    from app.workflow.investigate import _applicable_to

    assert _applicable_to("PostgreSQL 生产库", "postgresql") is True
    assert _applicable_to("MySQL 生产库", "postgresql") is False
    assert _applicable_to("", "postgresql") is False


# ---------------------------------------------------------------------------
# 结构化观察与 usage 已知性
# ---------------------------------------------------------------------------


def test_all_tool_results_are_fed_back_not_only_search_hits() -> None:
    """表结构等非 hits 结果同样要反馈给下一轮决策，不能丢掉。"""
    registry = FakeRegistry({"search_norms": [hit("norms/x#1")]})

    class NonSearchRegistry(FakeRegistry):
        async def call(self, name: str, args: Any, context: Any) -> ToolResult:
            self.calls.append((name, dict(args or {})))
            if name == "scan_sql":
                return ToolResult(
                    ok=True,
                    tool=name,
                    data={"status": "BLOCKED", "items": [{"code": "MISSING_LOCK_TIMEOUT"}]},
                    data_version="scan-v2",
                )
            return await super().call(name, args, context)

    planner = ScriptedPlanner(
        [
            CallTool("search_norms", {"query": "索引"}),
            CallTool("scan_sql", {"sql": "CREATE INDEX ..."}),
            Finish(),
        ]
    )
    seen: list[list[Any]] = []

    class ObservingPlanner(ScriptedPlanner):
        async def plan(self, **kwargs: Any):
            seen.append(list(kwargs.get("observations") or []))
            return await super().plan(**kwargs)

    loop = build(ObservingPlanner(list(planner._actions)), NonSearchRegistry())
    outcome = run(investigate(loop))

    # 第一轮没有观察；第二轮应当看到 search_norms；第三轮应当看到 search_norms + scan_sql。
    assert len(seen) == 3
    assert [item.tool for item in seen[-1]] == ["search_norms", "scan_sql"]
    scanned = seen[-1][-1]
    assert scanned.kind == "material", "非 hits 结果不能被当成搜索片段丢掉"
    assert "MISSING_LOCK_TIMEOUT" in scanned.summary
    assert scanned.payload_digest, "结构化观察必须带内容摘要哈希，截断不等于无法核对"
    assert scanned.data_version == "scan-v2"
    assert outcome.report.observations


def test_failed_tool_is_observed_with_its_error() -> None:
    planner = ScriptedPlanner([CallTool("scan_sql", {"sql": "x"}), Finish()])
    registry = FakeRegistry(failing=("scan_sql",))

    outcome = run(investigate(build(planner, registry)))

    failures = [item for item in outcome.report.observations if not item.ok]
    assert len(failures) == 1
    assert "注入的工具失败" in failures[0].error
    assert failures[0].payload_digest == "", "失败的调用没有内容摘要"


def test_timeout_is_observed_too() -> None:
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "慢"})])
    registry = FakeRegistry(hang=("search_norms",))

    outcome = run(investigate(build(planner, registry, tool_timeout=0.01)))

    assert outcome.report.observations
    assert outcome.report.observations[0].kind == "timeout"
    assert "超时" in outcome.report.observations[0].error


def test_usage_is_unknown_rather_than_zero() -> None:
    """provider 未提供 usage 时必须标 unknown，不得填 0 冒充已知消耗。"""
    planner = ScriptedPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry)))

    usage = outcome.report.usage
    assert usage.known is False
    assert usage.prompt_tokens is None and usage.completion_tokens is None
    assert usage.cost_estimate is None
    assert "unknown" in outcome.report.summary()


def test_usage_is_recorded_when_the_decider_reports_it() -> None:
    class ReportingPlanner(ScriptedPlanner):
        last_usage = {"prompt_tokens": 120, "completion_tokens": 30}

    planner = ReportingPlanner([CallTool("search_norms", {"query": "索引"}), Finish()])
    registry = FakeRegistry({"search_norms": [hit("norms/x#1")]})

    outcome = run(investigate(build(planner, registry)))

    assert outcome.report.usage.known is True
    assert outcome.report.usage.prompt_tokens == 120
    assert outcome.report.usage.completion_tokens == 30


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
    """要求模型决策但 provider 不具备该能力时：明确失败，且不产出草案。

    注意这条断言此前写的是"状态仍是 DRAFT_READY 且草案为空"——那等于把被审核的缺陷
    当成了期望行为（决策者不可用却照常走完生成）。现在断言的是正确结果：
    **明确失败、没有草案、没有进入生成**。
    """
    scoped = replace(
        settings, investigation_strategy="bounded_agent", investigation_planner="provider", max_revisions=0
    )
    service = AgentService(scoped)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.status is TaskStatus.FAILED, f"决策者不可用必须明确失败，实际 {view.status}"
    assert view.draft is None, "不得在决策者不可用时产出草案"
    details = " ".join(item.detail for item in view.events)
    assert "planner=unavailable" in details, "必须标明决策者不可用，而不是写成规则或模型"
    assert "decide" in (view.error or ""), view.error


# ---------------------------------------------------------------------------
# 工作台要看到的执行轨迹与预算
# ---------------------------------------------------------------------------


def test_task_view_exposes_strategy_investigation_and_budget(settings: Settings) -> None:
    """策略、停止原因、工具观察与预算必须真的出现在视图里，而不是只留在日志中。"""
    scoped = replace(settings, investigation_strategy="bounded_agent", max_revisions=0)
    service = AgentService(scoped)

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.strategy == "bounded_agent"
    assert view.investigation is not None
    assert view.investigation["strategy"] == "bounded_agent"
    assert view.investigation["planner"] == "rule"
    assert view.investigation["stop_reason"], "停止原因必须可见"
    assert isinstance(view.investigation["tool_observations"], list)
    assert view.investigation["tool_calls"] >= 1
    # provider 未提供 usage 时必须是 unknown，不得填 0 冒充已知消耗。
    assert view.usage is not None
    assert view.usage["known"] is False
    assert view.usage["prompt_tokens"] is None


def test_task_view_reports_the_actual_fixed_strategy(settings: Settings) -> None:
    """默认（固定流程）也要如实标注策略，不能让界面误以为走了模型调查。"""
    service = AgentService(replace(settings, investigation_strategy="fixed_workflow", max_revisions=0))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert view.strategy == "fixed_workflow"
    assert (view.investigation or {}).get("strategy") == "fixed_workflow"
