"""执行生命周期：取消竞态、落盘故障与监督。

这一组测试针对 P0 收尾发现的三个缺口，全部用**事件与故障注入**驱动，不依赖
随机 sleep、真实模型或真实浏览器：

1. `cancel` 在落盘**之前**就移除了执行句柄并终止执行：一旦落盘失败，执行已经停了、
   仓储却仍是 `RECEIVED`，健康检查显示一切正常 —— 任务成为孤儿。
2. `_execute` 在最终落盘**之前**释放句柄：落盘失败时异常发生在既有异常处理之外，
   后台任务留下 `RECEIVED`，`wait_for` 无声返回陈旧状态。
3. `inline` 模式没有登记可取消的执行句柄：`cancel` 只能阻止回写，
   工作流仍会继续调用模型与工具。

断言优先使用 `create_task` / `cancel` / `get_task` / `wait_for` 等公共服务接口。
对 `_executions` 等内部结构的检查仅作辅助，用于确认"资源确实被清理"。

关于失败注入 `FlakyRepository`：它只包一层真实仓储并让 `save` 失败，不改变任何
持久化语义，因此不是"用假实现替换掉被测逻辑"。
"""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Callable

import pytest

from app.config import Settings
from app.service import AgentService
from app.tools.registry import ToolRegistry, TrustedContext
from tests.conftest import DEMO_DIR, complete_request, execution_handles, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

# 一份能通过确定性扫描的草案：避免因规则命中触发修订循环，让测试只测生命周期。
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


# ---------------------------------------------------------------------------
# 可控 provider
# ---------------------------------------------------------------------------


class GatedProvider:
    """在生成阶段阻塞，直到显式放行；并记录是否观察到取消。"""

    name = "gated"

    def __init__(self, *, honour_cancellation: bool = True, outcome: str = "draft") -> None:
        self.entered = asyncio.Event()
        self.release = asyncio.Event()
        self.cancelled = False
        self.calls = 0
        self.on_generate: Callable[[], None] | None = None
        self._honour = honour_cancellation
        self._outcome = outcome

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "gated"}

    async def generate(self, _request: Any) -> str:
        self.calls += 1
        self.entered.set()
        if self.on_generate is not None:
            # 在生成阶段注入故障：此时 create_task 的初次落盘已经过去，
            # 因此可以精确地只影响"终态落盘"。
            self.on_generate()
        if self._outcome == "raise":
            raise RuntimeError("注入的 provider 异常")
        try:
            await self.release.wait()
        except asyncio.CancelledError:
            self.cancelled = True
            if self._honour:
                raise
            # 故意吞掉取消：模拟不响应取消的外部依赖。
        return VALID_DRAFT


# ---------------------------------------------------------------------------
# 落盘故障注入
# ---------------------------------------------------------------------------


class FlakyRepository:
    """包一层真实仓储，按需让 `save` 失败。"""

    def __init__(self, inner: Any) -> None:
        self._inner = inner
        self._fail_remaining = 0
        self.save_calls = 0
        self.on_failure: Callable[[], None] | None = None

    def arm(self, failures: int, on_failure: Callable[[], None] | None = None) -> None:
        self._fail_remaining = failures
        self.on_failure = on_failure

    def save(self, record: dict[str, Any]) -> None:
        self.save_calls += 1
        if self._fail_remaining > 0:
            self._fail_remaining -= 1
            if self.on_failure is not None:
                self.on_failure()
            raise OSError("注入的落盘失败")
        self._inner.save(record)

    def __getattr__(self, name: str) -> Any:
        return getattr(self._inner, name)


# ---------------------------------------------------------------------------
# 辅助
# ---------------------------------------------------------------------------


async def _settle(service: AgentService) -> None:
    """让已登记的后台执行跑完（不关心它抛什么），并让事件循环转几圈。"""
    pending = execution_handles(service)
    if pending:
        await asyncio.gather(*pending, return_exceptions=True)
    for _ in range(3):
        await asyncio.sleep(0)


def build_service(
    tmp_path: Path,
    provider: Any,
    *,
    execution_mode: str = "background",
    timeout_seconds: float = 30.0,
) -> tuple[AgentService, FlakyRepository]:
    settings = Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode=execution_mode,
        allow_header_identity=True,
        max_revisions=0,
        task_timeout_seconds=timeout_seconds,
    )
    service = AgentService(settings, provider=provider)
    flaky = FlakyRepository(service._repository)
    service._repository = flaky
    return service, flaky


async def blocked_background_task(
    tmp_path: Path, provider: GatedProvider
) -> tuple[AgentService, FlakyRepository, str]:
    """启动一个 background 任务并把它停在生成阶段。"""
    service, flaky = build_service(tmp_path, provider, execution_mode="background")
    view, _ = await service.create_task(complete_request(), CONTEXT)
    await provider.entered.wait()
    return service, flaky, view.task_id


@pytest.fixture
def tool_calls(monkeypatch: pytest.MonkeyPatch) -> list[str]:
    """记录所有工具调用，用于验证取消后不再调度后续步骤。"""
    calls: list[str] = []
    original = ToolRegistry.call

    async def counting(self: ToolRegistry, name: str, args: Any, context: Any) -> Any:
        calls.append(name)
        return await original(self, name, args, context)

    monkeypatch.setattr(ToolRegistry, "call", counting)
    return calls


# ---------------------------------------------------------------------------
# 6.1 background 执行中取消，取消落盘一次性失败
# ---------------------------------------------------------------------------


def test_cancel_persist_failure_must_not_abandon_a_running_execution(tmp_path: Path) -> None:
    """取消落盘失败时：句柄不得丢弃、执行不得终止、状态不得假装已取消。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, flaky, task_id = await blocked_background_task(tmp_path, provider)
        handle_before = execution_handles(service)
        assert handle_before, "background 执行必须登记可取消的句柄"

        flaky.arm(1)
        failed = False
        try:
            await service.cancel(task_id, CONTEXT)
        except Exception:  # noqa: BLE001 - 期望一个显式的失败
            failed = True

        assert failed, "取消在落盘失败时必须显式失败，不能假装成功"
        # 关键业务断言：执行仍在、句柄仍在、仓储状态未被改写成 CANCELLED。
        assert not provider.cancelled, "落盘失败时不得终止仍在运行的工作流"
        assert execution_handles(service), "落盘失败时不得丢弃执行句柄"
        assert (await service.get_task(task_id, CONTEXT)).status.value == "RECEIVED"
        assert (await service.health()).get("running_tasks", 0) == 1

        # 重试取消必须成功，并且这次真正停下工作流。
        await service.cancel(task_id, CONTEXT)
        # `task.cancel()` 只是调度取消；要让工作流真正收到，必须让事件循环转一圈。
        await _settle(service)
        assert provider.cancelled, "重试成功后必须真正取消执行"
        assert (await service.get_task(task_id, CONTEXT)).status.value == "CANCELLED"

    run(scenario())


# ---------------------------------------------------------------------------
# 6.2 取消落盘成功后，旧模型/工具迟到返回
# ---------------------------------------------------------------------------


def test_late_result_after_cancel_is_never_published(tmp_path: Path) -> None:
    """不响应取消的依赖迟到返回时，结果不得覆盖取消状态。

    这是**回归护栏**：改造前后都应通过，用来防止后续改动破坏该保障。
    """

    async def scenario() -> None:
        provider = GatedProvider(honour_cancellation=False)
        service, _flaky, task_id = await blocked_background_task(tmp_path, provider)

        await service.cancel(task_id, CONTEXT)
        provider.release.set()  # 让工作流即使被取消也继续跑完
        await _settle(service)

        view = await service.get_task(task_id, CONTEXT)
        assert view.status.value == "CANCELLED", "取消状态不得被迟到结果覆盖"
        assert view.draft is None, "取消后不得发布草案"

    run(scenario())


# ---------------------------------------------------------------------------
# 6.3 最终结果落盘一次性失败
# ---------------------------------------------------------------------------


def test_one_off_final_persist_failure_reaches_a_queryable_terminal_state(tmp_path: Path) -> None:
    """一次性落盘故障恢复后，任务必须能进入明确、可查询的终态。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, flaky, task_id = await blocked_background_task(tmp_path, provider)

        flaky.arm(1)  # 只让最终那次落盘失败
        provider.release.set()
        await _settle(service)

        view = await service.get_task(task_id, CONTEXT)
        assert view.status.value == "DRAFT_READY", f"一次性故障后应进入终态，实际 {view.status.value}"
        assert view.draft is not None
        assert not execution_handles(service), "完成后必须清理执行句柄"
        assert (await service.health()).get("unpersisted_tasks", []) == []

    run(scenario())


# ---------------------------------------------------------------------------
# 6.4 最终结果落盘持续失败
# ---------------------------------------------------------------------------


def test_persistent_final_persist_failure_is_reported_not_faked(tmp_path: Path) -> None:
    """持续落盘故障：不得伪造已落盘的 FAILED，必须显式降级并可被查询到。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, flaky, task_id = await blocked_background_task(tmp_path, provider)

        flaky.arm(10_000)  # 持续失败
        provider.release.set()
        await _settle(service)

        # 行为断言优先：wait_for 不得无声返回看似仍在运行的陈旧状态。
        raised = False
        try:
            await service.wait_for(task_id, CONTEXT)
        except Exception:  # noqa: BLE001 - 期望显式失败
            raised = True
        assert raised, "最终落盘持续失败时，wait_for 不得无声返回陈旧状态"

        # 也不得伪造一个"已落盘"的 FAILED：仓储里仍是最后成功写入的状态。
        assert (await service.get_task(task_id, CONTEXT)).status.value == "RECEIVED"
        assert not execution_handles(service), "故障不应永久占用执行句柄"

        health = await service.health()
        assert health.get("status") == "degraded", "持续存储故障必须体现在健康状态上"
        assert task_id in (health.get("unpersisted_tasks") or [])

    run(scenario())


# ---------------------------------------------------------------------------
# 6.5 持久化重试期间发生取消或新执行接管
# ---------------------------------------------------------------------------


def test_takeover_during_persist_retry_does_not_forge_a_degraded_state(tmp_path: Path) -> None:
    """重试期间被新执行接管时，既不写旧结果，也不谎报存储降级。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, flaky, task_id = await blocked_background_task(tmp_path, provider)

        def takeover() -> None:
            # 重试期间新执行接管（同步进行，避免依赖调度顺序）。
            service._begin_execution(task_id)

        flaky.arm(1, on_failure=takeover)
        provider.release.set()
        await _settle(service)

        health = await service.health()
        assert health.get("status") != "degraded", "接管不是存储故障，不得谎报降级"
        assert (health.get("unpersisted_tasks") or []) == []
        # 旧执行不得把自己的结果写到新执行身上。
        record = service._repository.get(task_id)
        assert record is not None and record.get("draft") is None

    run(scenario())


# ---------------------------------------------------------------------------
# 6.6 inline 与 background 取消后，后续工具调用次数均为 0
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("execution_mode", ["background", "inline"])
def test_cancel_stops_further_tool_scheduling(
    tmp_path: Path, tool_calls: list[str], execution_mode: str
) -> None:
    """取消必须真正停止执行，而不只是拒绝回写。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, _flaky = build_service(tmp_path, provider, execution_mode=execution_mode)

        creator = asyncio.create_task(service.create_task(complete_request(), CONTEXT))
        await provider.entered.wait()
        task_id = service._repository.list()[0]["task_id"]
        calls_at_cancel = len(tool_calls)

        await service.cancel(task_id, CONTEXT)
        provider.release.set()
        await asyncio.gather(creator, return_exceptions=True)
        await _settle(service)

        assert provider.cancelled, "取消必须真正命中工作流，而不只是拒绝回写"
        assert len(tool_calls) == calls_at_cancel, (
            f"取消后不得再调度工具，实际多调用了 {tool_calls[calls_at_cancel:]}"
        )
        assert (await service.get_task(task_id, CONTEXT)).status.value == "CANCELLED"
        assert not execution_handles(service), "取消后必须清理执行句柄"

    run(scenario())


# ---------------------------------------------------------------------------
# 6.7 inline 请求自身被取消时，内部工作流不会成为孤儿
# ---------------------------------------------------------------------------


def test_cancelled_inline_request_does_not_orphan_the_workflow(
    tmp_path: Path, tool_calls: list[str]
) -> None:
    """调用方被取消（客户端断开）时，内部执行必须被终止并留下确定的终态。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, _flaky = build_service(tmp_path, provider, execution_mode="inline")

        creator = asyncio.create_task(service.create_task(complete_request(), CONTEXT))
        await provider.entered.wait()
        calls_at_abandon = len(tool_calls)

        creator.cancel()
        await asyncio.gather(creator, return_exceptions=True)
        await _settle(service)

        assert not execution_handles(service), "内部执行不得作为孤儿继续存活"
        assert len(tool_calls) == calls_at_abandon, "放弃的执行不得继续调度后续工具"

        record = service._repository.list()[0]
        assert record["status"] in {"FAILED", "CANCELLED"}, (
            f"被放弃的 inline 执行必须留下确定的终态，实际 {record['status']}"
        )

    run(scenario())


# ---------------------------------------------------------------------------
# 额外发现的边界：inline 下的存储故障，以及放弃路径不得覆盖终态
# ---------------------------------------------------------------------------


def test_inline_persist_failure_surfaces_instead_of_returning_a_stale_view(tmp_path: Path) -> None:
    """inline 模式下终态落盘持续失败时，create_task 必须显式失败。"""

    async def scenario() -> None:
        provider = GatedProvider()
        service, flaky = build_service(tmp_path, provider, execution_mode="inline")
        # 在生成阶段才注入持续故障：create_task 与 _begin_execution 的落盘已经过去。
        provider.on_generate = lambda: flaky.arm(10_000)
        provider.release.set()

        raised = False
        try:
            await service.create_task(complete_request(), CONTEXT)
        except Exception:  # noqa: BLE001 - 期望显式失败
            raised = True

        assert raised, "inline 下终态落盘持续失败时不得返回一个可能是陈旧的视图"
        assert not execution_handles(service), "失败后仍须清理执行句柄"
        health = await service.health()
        assert health.get("status") == "degraded"
        assert health.get("unpersisted_tasks")

    run(scenario())


def test_abandon_never_overwrites_an_already_persisted_terminal_state(tmp_path: Path) -> None:
    """放弃路径不得把已经落盘的终态改写成 FAILED。

    场景：调用方被取消与执行完成之间可能只差一个事件循环轮次。
    这是针对该守卫的**定向**检查；完整生命周期另有上面的用例覆盖。
    """

    async def scenario() -> None:
        provider = GatedProvider()
        provider.release.set()  # 立即完成，避免依赖调度顺序
        service, _flaky = build_service(tmp_path, provider, execution_mode="inline")
        view, _ = await service.create_task(complete_request(), CONTEXT)
        assert view.status.value == "DRAFT_READY"

        record = service._repository.get(view.task_id)
        stale = SimpleNamespace(task_id=view.task_id, execution_id=record["execution_id"])
        service._abandon_execution(stale, "迟到的放弃")  # type: ignore[arg-type]

        after = service._repository.get(view.task_id)
        assert after["status"] == "DRAFT_READY", "已落盘的终态不得被放弃路径覆盖"
        assert after.get("draft") is not None

    run(scenario())


# ---------------------------------------------------------------------------
# 6.8 正常完成、超时、异常、取消后，句柄与健康状态符合实际
# ---------------------------------------------------------------------------

def test_handle_and_health_after_normal_completion(tmp_path: Path) -> None:
    async def scenario() -> None:
        provider = GatedProvider()
        service, _flaky, task_id = await blocked_background_task(tmp_path, provider)
        provider.release.set()
        await _settle(service)

        assert (await service.get_task(task_id, CONTEXT)).status.value == "DRAFT_READY"
        assert not execution_handles(service)
        assert (await service.health())["running_tasks"] == 0

    run(scenario())


def test_handle_and_health_after_timeout(tmp_path: Path) -> None:
    async def scenario() -> None:
        provider = GatedProvider()  # 永不放行 → 由任务超时终止
        service, _flaky = build_service(tmp_path, provider, timeout_seconds=0.05)
        view, _ = await service.create_task(complete_request(), CONTEXT)
        task_id = view.task_id
        await provider.entered.wait()
        await _settle(service)

        final = await service.get_task(task_id, CONTEXT)
        assert final.status.value == "FAILED"
        assert "超过" in (final.error or "")
        assert not execution_handles(service), "超时后必须清理执行句柄"
        assert (await service.health())["running_tasks"] == 0

    run(scenario())


def test_handle_and_health_after_workflow_exception(tmp_path: Path) -> None:
    async def scenario() -> None:
        provider = GatedProvider(outcome="raise")
        service, _flaky = build_service(tmp_path, provider)
        view, _ = await service.create_task(complete_request(), CONTEXT)
        await provider.entered.wait()
        await _settle(service)

        final = await service.get_task(view.task_id, CONTEXT)
        assert final.status.value == "FAILED"
        assert not execution_handles(service), "异常后必须清理执行句柄"
        health = await service.health()
        assert health["running_tasks"] == 0
        assert health.get("status") == "ok"

    run(scenario())


def test_handle_and_health_after_cancel(tmp_path: Path) -> None:
    async def scenario() -> None:
        provider = GatedProvider()
        service, _flaky, task_id = await blocked_background_task(tmp_path, provider)
        await service.cancel(task_id, CONTEXT)
        await _settle(service)

        assert not execution_handles(service), "取消后必须清理执行句柄"
        health = await service.health()
        assert health["running_tasks"] == 0
        assert health.get("status") == "ok"

    run(scenario())
