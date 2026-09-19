"""usage 记账与任务级预算。

锁定四件事：

1. **按请求累计**：每轮 100 输入 + 10 输出，三轮就是 300 + 30，而不是只报最后一次；
2. **缺失不复用**：某次响应没带 usage，不能被上一次的值顶替，也不能因此把总量说成已知；
3. **按任务隔离**：provider 是跨任务共享的，用量必须记在任务自己的记账器上，
   并且任务级 token／费用预算真的会**停止**后续模型调用（未知用量按保守值计入，
   缺定价时费用上限失败关闭）；
4. **记账不因取消而漏**：被取消的请求同样算发出去过；费用只在**不缺任何 usage** 时
   才作为已知数字报出；预算只决定"能不能发下一次"，不会把已经付过钱的响应丢掉。
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import replace
from typing import Any

import httpx
import pytest

from app.budget import PHASE_GENERATE, TaskBudgetExceeded, UsageLedger, usage_scope
from app.config import Settings
from app.llm.provider import DraftRequest, OpenAICompatibleProvider
from app.schemas.drafts import TaskStatus
from app.service import AgentService
from app.tools.registry import TrustedContext
from tests.conftest import complete_request, complete_slots, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

NOT_JSON = "这不是 JSON"


class ModelStub:
    """按调用次序返回 (content, usage)；用完后重复最后一项。"""

    def __init__(self, replies: list[tuple[str, dict[str, int] | None]]) -> None:
        self._replies = replies
        self.calls = 0

    def install(self, monkeypatch: pytest.MonkeyPatch) -> None:
        def handler(_request: httpx.Request) -> httpx.Response:
            content, usage = self._replies[min(self.calls, len(self._replies) - 1)]
            self.calls += 1
            body: dict[str, Any] = {"choices": [{"message": {"role": "assistant", "content": content}}]}
            if usage is not None:
                body["usage"] = usage
            return httpx.Response(200, json=body)

        transport = httpx.MockTransport(handler)
        original = httpx.AsyncClient

        def factory(*args: Any, **kwargs: Any) -> httpx.AsyncClient:
            kwargs["transport"] = transport
            return original(*args, **kwargs)

        monkeypatch.setattr(httpx, "AsyncClient", factory)


def configured(settings: Settings, **overrides: Any) -> Settings:
    base: dict[str, Any] = {
        "llm_base_url": "http://model-stub.invalid/v1",
        "llm_api_key": "stub-key",
        "llm_max_attempts": 1,
        "max_revisions": 0,
    }
    base.update(overrides)
    supported = set(Settings.__dataclass_fields__)
    return replace(settings, **{k: v for k, v in base.items() if k in supported})


def usage_of(view: Any) -> dict[str, Any]:
    assert view.usage is not None
    return view.usage


# ---------------------------------------------------------------------------
# 按请求累计
# ---------------------------------------------------------------------------


def test_usage_accumulates_across_requests(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """三轮各 100 + 10，应报 300 + 30，而不是只报最后一次。"""
    stub = ModelStub([(NOT_JSON, {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(settings, draft_parse_attempts=3)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 3
    assert view.status is TaskStatus.FAILED
    reported = usage_of(view)
    assert reported["requests"] == 3
    assert reported["prompt_tokens"] == 300, reported
    assert reported["completion_tokens"] == 30, reported
    assert reported["missing_responses"] == 0
    assert reported["known"] is True


def test_missing_usage_is_not_replaced_by_the_previous_response(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """第一次有 usage、第二次没有：不得沿用旧值，也不得把总量说成已知。"""
    stub = ModelStub(
        [
            (NOT_JSON, {"prompt_tokens": 100, "completion_tokens": 10}),
            (NOT_JSON, None),
        ]
    )
    stub.install(monkeypatch)
    scoped = configured(settings, draft_parse_attempts=2)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    reported = usage_of(view)
    assert reported["requests"] == 2
    assert reported["reported_responses"] == 1
    assert reported["missing_responses"] == 1
    assert reported["known"] is False, "有响应没报 usage 时总量不能算已知"
    assert reported["prompt_tokens"] == 100, "只能累加真的报了的那一次"
    assert "未提供 usage" in reported["note"]


def test_usage_does_not_leak_between_tasks(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """两个任务各自记账：第二个任务不得包含第一个任务的用量。"""
    stub = ModelStub(
        [
            (json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10}),
            (json.dumps(_draft()), {"prompt_tokens": 500, "completion_tokens": 50}),
        ]
    )
    stub.install(monkeypatch)
    scoped = configured(settings)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    first, _ = run(service.create_task(complete_request(), CONTEXT))
    second, _ = run(service.create_task(complete_request(), CONTEXT))

    assert usage_of(first)["prompt_tokens"] == 100
    assert usage_of(second)["prompt_tokens"] == 500, "第二个任务串到了第一个任务的用量"
    assert usage_of(second)["requests"] == 1


def test_last_usage_is_cleared_when_a_response_omits_it(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """provider 上的 last_usage 只描述**最近一次**响应，缺失即 None。"""
    stub = ModelStub(
        [
            (json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10}),
            (json.dumps(_draft()), None),
        ]
    )
    stub.install(monkeypatch)
    scoped = configured(settings)
    provider = OpenAICompatibleProvider(scoped)

    async def scenario() -> tuple[Any, Any]:
        request = DraftRequest(requirement="x", slots=complete_slots())
        await provider.generate(request)
        first = provider.last_usage
        await provider.generate(request)
        return first, provider.last_usage

    first, second = run(scenario())
    assert first == {"prompt_tokens": 100, "completion_tokens": 10}
    assert second is None, "响应没带 usage 时必须清空，而不是沿用上一次"


# ---------------------------------------------------------------------------
# 任务级预算
# ---------------------------------------------------------------------------


def test_task_token_budget_stops_further_model_calls(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """预算用尽后不再打模型：调用次数被硬性拦住，任务如实失败。"""
    stub = ModelStub([(NOT_JSON, {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(settings, draft_parse_attempts=5, max_task_tokens=150)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 2, f"预算是硬边界，实际打了 {stub.calls} 次"
    assert view.status is TaskStatus.FAILED
    assert "预算" in (view.error or ""), view.error


def test_unknown_usage_is_charged_conservatively_to_the_budget(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """响应不带 usage 时，预算仍要有界：按保守值计入判定并停止。"""
    stub = ModelStub([(NOT_JSON, None)])
    stub.install(monkeypatch)
    scoped = configured(
        settings, draft_parse_attempts=5, max_task_tokens=150, unknown_usage_charge_tokens=100
    )
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 2, f"未提供 usage 时也必须被预算拦住，实际打了 {stub.calls} 次"
    assert view.status is TaskStatus.FAILED
    assert "预算" in (view.error or ""), view.error
    # 真实 token 仍然是"未知"，不能被保守计费冒充成已知。
    assert usage_of(view)["known"] is False
    assert usage_of(view)["prompt_tokens"] is None


def test_cost_limit_without_pricing_fails_closed(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """配了费用上限却没有定价数据：失败关闭，绝不当作"没超预算"。"""
    stub = ModelStub([(NOT_JSON, {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(settings, max_task_cost_estimate=1.0)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 0, "无法判定费用时不应发出模型请求"
    assert view.status is TaskStatus.FAILED
    assert "定价" in (view.error or ""), view.error


def test_cost_limit_with_pricing_stops_further_calls(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """给了单价时，费用上限同样会停止后续调用，并报出折算费用。"""
    stub = ModelStub([(NOT_JSON, {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(
        settings,
        draft_parse_attempts=5,
        max_task_cost_estimate=0.15,
        llm_price_prompt_per_1k=1.0,
        llm_price_completion_per_1k=1.0,
    )
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 2
    assert view.status is TaskStatus.FAILED
    assert "费用预算" in (view.error or ""), view.error
    assert usage_of(view)["cost_estimate"] == pytest.approx(0.22, abs=0.001)


# ---------------------------------------------------------------------------
# 账本持久化、续算与结构化记录
# ---------------------------------------------------------------------------


def test_failed_execution_still_persists_its_accounting(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """执行失败也要留下账目：失败的调用同样花了钱。"""
    stub = ModelStub([(json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(settings)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    from app.workflow.graph import DraftWorkflow

    original = DraftWorkflow._run_check

    async def boom(self: DraftWorkflow, state: Any):
        raise RuntimeError("模拟执行中中断")

    monkeypatch.setattr(DraftWorkflow, "_run_check", boom)
    try:
        view, _ = run(service.create_task(complete_request(), CONTEXT))
    finally:
        monkeypatch.setattr(DraftWorkflow, "_run_check", original)

    assert view.status is TaskStatus.FAILED
    reported = usage_of(view)
    assert reported["requests"] == 1, "失败的执行也必须留下账目"
    assert reported["prompt_tokens"] == 100
    assert reported["calls"], "必须有结构化的逐请求记录"


def test_resume_continues_the_task_budget(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """恢复不得清零任务累计预算：续算后总请求数包含此前执行。"""
    stub = ModelStub(
        [
            (json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10}),
            (json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10}),
        ]
    )
    stub.install(monkeypatch)
    scoped = configured(settings)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    from app.workflow.graph import DraftWorkflow

    original = DraftWorkflow._run_check
    first_boom = {"done": False}

    async def boom_once(self: DraftWorkflow, state: Any):
        if not first_boom["done"]:
            first_boom["done"] = True
            raise RuntimeError("模拟执行中中断")
        return await original(self, state)

    monkeypatch.setattr(DraftWorkflow, "_run_check", boom_once)
    failed, _ = run(service.create_task(complete_request(), CONTEXT))
    assert usage_of(failed)["requests"] == 1

    monkeypatch.setattr(DraftWorkflow, "_run_check", original)
    resumed = run(service.resume(failed.task_id, CONTEXT))

    assert usage_of(resumed)["requests"] >= 1
    assert usage_of(resumed)["prompt_tokens"] >= 100, "恢复后累计预算被清零了"


def test_call_records_carry_phase_outcome_and_retries(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """结构化记录要能回答"哪一步、成没成、重试了几次"。"""
    calls = {"n": 0}

    def handler(_request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:  # 第一次 503，触发传输层重试
            return httpx.Response(503, json={"error": "busy"})
        return httpx.Response(
            200,
            json={
                "choices": [{"message": {"role": "assistant", "content": json.dumps(_draft())}}],
                "usage": {"prompt_tokens": 100, "completion_tokens": 10},
            },
        )

    transport = httpx.MockTransport(handler)
    original_client = httpx.AsyncClient

    def factory(*args: Any, **kwargs: Any) -> httpx.AsyncClient:
        kwargs["transport"] = transport
        return original_client(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", factory)
    scoped = configured(settings, llm_max_attempts=2)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    reported = usage_of(view)
    assert calls["n"] == 2, "503 应触发一次重试"
    assert reported["requests"] == 2, f"503 重试必须计入请求数：{reported}"
    assert reported["missing_responses"] == 1, "失败的那次请求没有 usage，应记为缺失"
    assert reported["known"] is False, "有缺失时总量不能算已知"
    outcomes = [item["outcome"] for item in reported["calls"]]
    phases = {item["phase"] for item in reported["calls"]}
    assert "error" in outcomes and "ok" in outcomes, outcomes
    assert phases <= {"generate", "investigate", "other"}, phases


def test_model_request_cap_is_enforced(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """任务累计请求次数是硬上限，含重试。"""
    stub = ModelStub([(NOT_JSON, {"prompt_tokens": 1, "completion_tokens": 1})])
    stub.install(monkeypatch)
    scoped = configured(settings, draft_parse_attempts=5, max_task_requests=2)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 2, f"请求次数上限是硬边界，实际 {stub.calls}"
    assert view.status is TaskStatus.FAILED
    assert "请求次数" in (view.error or ""), view.error


def test_cancelled_request_is_still_accounted(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """取消不是漏记的理由：请求已经发出去了，账本必须留下这一笔。

    请求被取消后到底有没有到达模型、有没有被计费，我们并不知道——所以按"算它花过"记账，
    而不是当作没发生过。这与"不宣称 exactly-once"是同一口径。
    """
    started = asyncio.Event()

    async def handler(_request: httpx.Request) -> httpx.Response:
        started.set()
        await asyncio.Event().wait()  # 一直不返回，直到被取消
        raise AssertionError("unreachable")

    transport = httpx.MockTransport(handler)
    original_client = httpx.AsyncClient

    def factory(*args: Any, **kwargs: Any) -> httpx.AsyncClient:
        kwargs["transport"] = transport
        return original_client(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", factory)
    scoped = configured(settings)
    provider = OpenAICompatibleProvider(scoped)
    ledger = UsageLedger(task_id="t", execution_id="e", model=scoped.llm_model)

    async def scenario() -> None:
        request = DraftRequest(requirement="x", slots=complete_slots())
        with usage_scope(ledger, phase=PHASE_GENERATE):
            task = asyncio.create_task(provider.generate(request))
            await asyncio.wait_for(started.wait(), timeout=10)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task

    run(scenario())

    assert provider.requests_sent == 1
    assert ledger.requests == 1, "被取消的请求漏记了"
    assert [item.outcome for item in ledger.calls] == ["cancelled"], ledger.calls
    assert ledger.reported == 0, "取消的那次拿不到 usage，不能被算成已报"
    assert ledger.known is False, "取消的那次没有 usage，总量不能算已知"


def test_cost_is_unknown_rather_than_zero_when_usage_is_missing(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """缺 usage 时报费用必须是不确定，而不是一个"已知的 0"。

    以前只要配了单价就返回一个数：一次响应都没报 usage 时恰好是 0.0，读起来像"确定没花钱"。
    """
    stub = ModelStub([(NOT_JSON, None)])
    stub.install(monkeypatch)
    scoped = configured(
        settings, llm_price_prompt_per_1k=1.0, llm_price_completion_per_1k=1.0
    )
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    reported = usage_of(view)
    assert reported["missing_responses"] == 1
    assert reported["known"] is False
    assert reported["cost_estimate"] is None, "缺 usage 时不得报已知费用（更不得报 0）"
    assert reported["cost_known"] is False
    assert "费用" in reported["note"], reported["note"]


def test_cost_is_reported_when_every_response_reports_usage(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """对照组：每一次响应都报了 usage 时，费用照常报出并按单价折算。"""
    stub = ModelStub([(json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(
        settings, llm_price_prompt_per_1k=1.0, llm_price_completion_per_1k=1.0
    )
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    reported = usage_of(view)
    assert reported["known"] is True
    assert reported["cost_known"] is True
    assert reported["cost_estimate"] == pytest.approx(0.11, abs=0.001)


def test_cost_limit_fails_closed_when_usage_is_missing(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """配了费用上限但响应不报 usage：下一次请求无法判定费用，按失败关闭，不当作 0 继续花。"""
    stub = ModelStub([(NOT_JSON, None)])
    stub.install(monkeypatch)
    scoped = configured(
        settings,
        draft_parse_attempts=5,
        max_task_cost_estimate=1.0,
        llm_price_prompt_per_1k=1.0,
        llm_price_completion_per_1k=1.0,
    )
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 1, f"缺 usage 时费用上限无法判定，应在下一次请求前停住，实际 {stub.calls}"
    assert view.status is TaskStatus.FAILED
    assert "usage" in (view.error or ""), view.error


def test_missing_usage_does_not_discard_a_response_that_needs_no_further_request(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """费用上限是"花钱闸门"，不是"结果闸门"：已经拿到的合格响应不该被判为失败。

    缺 usage 时费用按未知报出，但既然后续不需要再发请求，就没有"再多花"的风险。
    """
    stub = ModelStub([(json.dumps(_draft()), None)])
    stub.install(monkeypatch)
    scoped = configured(
        settings,
        max_task_cost_estimate=1.0,
        llm_price_prompt_per_1k=1.0,
        llm_price_completion_per_1k=1.0,
    )
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 1
    assert view.status is TaskStatus.DRAFT_READY, (view.status, view.error)
    reported = usage_of(view)
    assert reported["cost_estimate"] is None
    assert reported["cost_known"] is False


def test_the_last_allowed_request_keeps_its_successful_response(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """预算只管"能不能发下一次"：被允许的那次成功响应必须被使用，不能被丢弃。

    以前 `_record_call` 在记账后又复查一次预算，于是 max_requests=1 时第一次（也是唯一一次）
    调用必定失败——钱已经花了，响应却被扔掉。
    """
    stub = ModelStub([(json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(settings, max_task_requests=1)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))

    view, _ = run(service.create_task(complete_request(), CONTEXT))

    assert stub.calls == 1, f"只允许一次请求，实际发了 {stub.calls} 次"
    assert view.status is TaskStatus.DRAFT_READY, (view.status, view.error)
    assert view.draft is not None
    assert usage_of(view)["requests"] == 1


def test_the_budget_refuses_the_next_request_before_sending_it(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """硬边界仍然成立：超限后**不再发出**下一次请求，账本也不再多记一笔。"""
    stub = ModelStub([(json.dumps(_draft()), {"prompt_tokens": 100, "completion_tokens": 10})])
    stub.install(monkeypatch)
    scoped = configured(settings)
    provider = OpenAICompatibleProvider(scoped)
    ledger = UsageLedger(max_requests=1, task_id="t", execution_id="e", model=scoped.llm_model)

    async def scenario() -> None:
        request = DraftRequest(requirement="x", slots=complete_slots())
        with usage_scope(ledger, phase=PHASE_GENERATE):
            await provider.generate(request)  # 被允许
            with pytest.raises(TaskBudgetExceeded):
                await provider.generate(request)  # 在发出去之前被拦下

    run(scenario())

    assert stub.calls == 1, "被拒绝的那次不应真的发出去"
    assert ledger.requests == 1
    assert provider.last_usage == {"prompt_tokens": 100, "completion_tokens": 10}, (
        "被允许的那次响应结果必须保留"
    )


def _draft() -> dict[str, Any]:
    return {
        "sql": (
            "SET lock_timeout = '3s';\n\n"
            "CREATE INDEX CONCURRENTLY idx_orders_user_created ON orders (user_id, created_at DESC);"
        ),
        "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
        "assumptions": [],
        "open_questions": [],
        "advisory_risk": "LOW",
        "advice_summary": "占位结论",
    }
