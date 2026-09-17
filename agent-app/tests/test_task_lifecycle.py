"""执行所有权、取消竞态与持久化诚实性。

锁定改造前的四个缺陷：

- `_execute` 的 `finally` **无条件落盘**，且取消与重跑没有执行所有权概念，
  于是旧协程可以把自己的结果覆盖到新状态上；
- `TaskRepository.save` **先改内存再落盘**，落盘失败时内存里留下一份磁盘上不存在的状态；
- `_load` 把 JSON 解析失败与读取失败**静默当成空库**，已有任务凭空消失，
  且下一次落盘会把损坏内容永久覆盖；
- 派发失败时任务永远停在 `RECEIVED`，没有任何协程或超时在推动它。
"""

from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from typing import Any

import pytest

from app.config import Settings
from app.main import create_app
from app.service import AgentService
from app.store.tasks import TaskRepository, TaskRepositoryCorrupted
from app.tools.registry import TrustedContext
from tests.conftest import DEMO_DIR, PLANNED_AT, bare_request, complete_request, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")


def build_settings(tmp_path: Path, **overrides: Any) -> Settings:
    base: dict[str, Any] = {
        "agent_demo_dir": str(DEMO_DIR),
        "task_store_path": str(tmp_path / "agent-tasks.json"),
        "execution_mode": "inline",
        "allow_header_identity": True,
        "max_revisions": 0,
    }
    base.update(overrides)
    return Settings(**base)


class BlockingProvider:
    """进入生成阶段后阻塞，用于把执行停在中间态。"""

    name = "blocking"

    def __init__(self) -> None:
        self.entered = asyncio.Event()
        self.release = asyncio.Event()

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "blocking"}

    async def generate(self, _request: Any) -> str:
        self.entered.set()
        await self.release.wait()
        return json.dumps(
            {
                "sql": "CREATE INDEX CONCURRENTLY idx_orders_user ON orders (user_id);",
                "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user;",
                "assumptions": [],
                "open_questions": [],
                "advisory_risk": "LOW",
                "advice_summary": "占位结论",
            },
            ensure_ascii=False,
        )


# ---------------------------------------------------------------------------
# 仓储：损坏必须失败关闭
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "content",
    [
        "{ 这不是 JSON",
        "",
        "[]",
        '{"tasks": "not-an-array"}',
        '{"tasks": [1, 2]}',
    ],
)
def test_corrupt_store_fails_closed(tmp_path: Path, content: str) -> None:
    path = tmp_path / "agent-tasks.json"
    path.write_text(content, encoding="utf-8")

    with pytest.raises(TaskRepositoryCorrupted):
        TaskRepository(str(path))


def test_corrupt_store_is_not_overwritten(tmp_path: Path) -> None:
    """加载失败时必须保持原文件不变，不能把损坏内容覆盖成空库。"""
    path = tmp_path / "agent-tasks.json"
    path.write_text("{ 这不是 JSON", encoding="utf-8")

    with pytest.raises(TaskRepositoryCorrupted):
        TaskRepository(str(path))

    assert path.read_text(encoding="utf-8") == "{ 这不是 JSON"


def test_corrupt_store_prevents_service_startup(tmp_path: Path) -> None:
    """损坏状态下服务必须起不来，而不是以空库对外提供服务。"""
    settings = build_settings(tmp_path)
    Path(settings.task_store_path).write_text("{ 坏文件", encoding="utf-8")

    with pytest.raises(TaskRepositoryCorrupted):
        create_app(settings)


def test_missing_store_file_is_not_an_error(tmp_path: Path) -> None:
    """文件不存在是全新部署，属于正常状态。"""
    repository = TaskRepository(str(tmp_path / "not-created-yet.json"))
    assert repository.list() == []


def test_blank_store_path_is_rejected(tmp_path: Path) -> None:
    """以前这里是一条恒为假的死代码，等价于"静默不落盘"。"""
    with pytest.raises(ValueError):
        TaskRepository("")
    with pytest.raises(ValueError):
        TaskRepository("   ")


# ---------------------------------------------------------------------------
# 仓储：先落盘、成功后才提交内存
# ---------------------------------------------------------------------------


def _fail_persist(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(_records: dict[str, dict[str, Any]]) -> None:
        raise OSError("磁盘写入失败")

    monkeypatch.setattr(TaskRepository, "_persist_records", staticmethod(boom))


def test_failed_persist_keeps_previous_memory_state(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    repository = TaskRepository(str(tmp_path / "agent-tasks.json"))
    repository.save({"task_id": "task_1", "status": "RECEIVED"})

    _fail_persist(monkeypatch)
    with pytest.raises(OSError):
        repository.save({"task_id": "task_1", "status": "DRAFT_READY"})

    # 内存必须停留在磁盘上的那份状态，不能领先于磁盘。
    assert repository.get("task_1")["status"] == "RECEIVED"


def test_failed_persist_does_not_publish_a_new_record(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    repository = TaskRepository(str(tmp_path / "agent-tasks.json"))

    _fail_persist(monkeypatch)
    with pytest.raises(OSError):
        repository.save({"task_id": "task_2", "status": "RECEIVED"})

    assert repository.get("task_2") is None
    assert repository.list() == []


def test_failed_persist_leaves_no_temporary_file(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """落盘中途失败不得留下半截临时文件。"""
    repository = TaskRepository(str(tmp_path / "agent-tasks.json"))

    def boom_replace(*_args: Any, **_kwargs: Any) -> None:
        raise OSError("替换失败")

    monkeypatch.setattr(os, "replace", boom_replace)
    with pytest.raises(OSError):
        repository.save({"task_id": "task_3", "status": "RECEIVED"})

    assert [item.name for item in tmp_path.iterdir() if item.suffix == ".tmp"] == []
    assert repository.get("task_3") is None


def test_save_does_not_alias_the_caller_record(tmp_path: Path) -> None:
    """调用方后续修改自己那份字典，不得改变已持久化的状态。"""
    repository = TaskRepository(str(tmp_path / "agent-tasks.json"))
    payload = {"task_id": "task_4", "status": "RECEIVED"}
    repository.save(payload)

    payload["status"] = "DRAFT_READY"
    assert repository.get("task_4")["status"] == "RECEIVED"


# ---------------------------------------------------------------------------
# 执行所有权
# ---------------------------------------------------------------------------


async def _start_blocked_execution(
    tmp_path: Path, provider: BlockingProvider
) -> tuple[AgentService, str]:
    settings = build_settings(tmp_path, execution_mode="background")
    service = AgentService(settings, provider=provider)
    view, _ = await service.create_task(complete_request(), CONTEXT)
    await provider.entered.wait()
    return service, view.task_id


def test_stale_execution_cannot_overwrite_a_newer_one(tmp_path: Path) -> None:
    """持有旧 execution_id 的执行不得再写入任何状态。"""
    service = AgentService(build_settings(tmp_path))
    # 用信息不全的需求：终态是 NEEDS_INFO，与旧执行试图写入的 DRAFT_READY 不同，
    # 这样"被拒绝"才可观测。
    view, _ = run(service.create_task(bare_request(), CONTEXT))
    task_id = view.task_id
    assert service._repository.get(task_id)["status"] == "NEEDS_INFO"

    stale_execution = service._begin_execution(task_id)
    stale_record = service._owned(task_id, stale_execution)
    assert stale_record is not None
    stale_record["status"] = "DRAFT_READY"
    stale_record["draft"] = {"sql": "SELECT 1"}

    # 新执行接管（例如补充信息后重新派发）。
    current_execution = service._begin_execution(task_id)
    service._save_if_owner(task_id, stale_execution, stale_record)

    record = service._repository.get(task_id)
    assert record["execution_id"] == current_execution
    assert record["status"] == "NEEDS_INFO", "旧执行不得覆盖新执行的状态"
    assert record.get("draft") is None


def test_owner_can_still_write(tmp_path: Path) -> None:
    """所有权检查不能把正常写入一起挡掉。"""
    service = AgentService(build_settings(tmp_path))
    view, _ = run(service.create_task(bare_request(), CONTEXT))
    task_id = view.task_id

    execution_id = service._begin_execution(task_id)
    record = service._owned(task_id, execution_id)
    assert record is not None
    record["status"] = "DRAFT_READY"
    record["draft"] = {"sql": "SELECT 1"}
    service._save_if_owner(task_id, execution_id, record)

    stored = service._repository.get(task_id)
    assert stored["status"] == "DRAFT_READY"
    assert stored["draft"] == {"sql": "SELECT 1"}


def test_cancel_prevents_the_cancelled_execution_from_publishing(tmp_path: Path) -> None:
    """取消后旧执行不得把自己的结果写回。"""

    async def scenario() -> dict[str, Any]:
        provider = BlockingProvider()
        service, task_id = await _start_blocked_execution(tmp_path, provider)
        handle = service._running[task_id]

        await service.cancel(task_id, CONTEXT)
        # 放开阻塞：即使旧执行还能跑到结束，它也已经失去所有权。
        provider.release.set()
        await asyncio.gather(handle, return_exceptions=True)
        await asyncio.sleep(0)

        return service._repository.get(task_id)

    record = run(scenario())
    assert record["status"] == "CANCELLED"
    assert record.get("draft") is None, "被取消的执行不得发布草案"
    assert record["execution_id"] == ""


def test_redispatch_fences_the_previous_execution(tmp_path: Path) -> None:
    """重新派发会开启新执行（等价于补充信息后重跑），旧执行的写入被拒绝。"""

    async def scenario() -> tuple[dict[str, Any], str]:
        provider = BlockingProvider()
        service, task_id = await _start_blocked_execution(tmp_path, provider)
        stale_handle = service._running[task_id]
        stale_execution = service._repository.get(task_id)["execution_id"]

        # 再派发一次：新执行立刻接管所有权，旧执行被栅栏挡住。
        provider.release.clear()
        await service._dispatch(task_id)
        current_execution = service._repository.get(task_id)["execution_id"]
        assert current_execution != stale_execution

        provider.release.set()
        await asyncio.gather(stale_handle, return_exceptions=True)
        await asyncio.sleep(0)
        return service._repository.get(task_id), current_execution

    record, current_execution = run(scenario())
    assert record["execution_id"] == current_execution, "旧执行不得覆盖新执行的所有权"


def test_health_reports_no_running_task_after_cancel(tmp_path: Path) -> None:
    async def scenario() -> dict[str, Any]:
        provider = BlockingProvider()
        service, task_id = await _start_blocked_execution(tmp_path, provider)
        assert (await service.health())["running_tasks"] == 1

        await service.cancel(task_id, CONTEXT)
        provider.release.set()
        await asyncio.sleep(0)
        return await service.health()

    health = run(scenario())
    assert health["running_tasks"] == 0, "取消后不应继续报告有任务在运行"


# ---------------------------------------------------------------------------
# 派发失败与重启策略
# ---------------------------------------------------------------------------


def test_dispatch_failure_does_not_leave_a_received_orphan(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    service = AgentService(build_settings(tmp_path))

    async def boom(_task_id: str) -> None:
        raise RuntimeError("队列不可用")

    monkeypatch.setattr(service, "_dispatch", boom)

    with pytest.raises(RuntimeError):
        run(service.create_task(complete_request(), CONTEXT))

    records = service._repository.list()
    assert len(records) == 1
    assert records[0]["status"] == "FAILED", "不得留下永远停在 RECEIVED 的残骸"
    assert "队列不可用" in records[0]["error"]
    assert any(item["kind"] == "dispatch_failed" for item in records[0]["events"])


def test_restart_marks_in_flight_tasks_as_interrupted(tmp_path: Path) -> None:
    settings = build_settings(tmp_path)
    first = AgentService(settings)
    first._repository.save(
        {
            "task_id": "task_inflight",
            "organization_id": "org_demo",
            "user_id": "alice",
            "requirement": "占位需求",
            "status": "RUNNING",
            "events": [],
        }
    )

    second = AgentService(settings)

    assert second.interrupted_task_ids == ["task_inflight"]
    record = second._repository.get("task_inflight")
    assert record["status"] == "FAILED"
    # 明确标注"未续跑"，不能宣称已恢复。
    assert record["restart_policy"] == "interrupted_without_resume"
    assert any(item["kind"] == "restart" for item in record["events"])


@pytest.mark.parametrize("status", ["DRAFT_READY", "CHECK_BLOCKED", "INPUT_REJECTED", "FAILED", "CANCELLED"])
def test_restart_leaves_terminal_tasks_untouched(tmp_path: Path, status: str) -> None:
    settings = build_settings(tmp_path)
    first = AgentService(settings)
    first._repository.save(
        {
            "task_id": "task_terminal",
            "organization_id": "org_demo",
            "user_id": "alice",
            "requirement": "占位需求",
            "status": status,
            "events": [],
        }
    )

    second = AgentService(settings)

    assert second.interrupted_task_ids == []
    assert second._repository.get("task_terminal")["status"] == status


def test_inline_and_background_agree_on_the_outcome(tmp_path: Path) -> None:
    """两条执行路径对同一个任务必须给出相同终态。

    inline 直接 await 执行，background 走 `asyncio.create_task`；
    改造前取消与异常处理在两条路径上并不一致。
    """

    async def run_inline() -> str:
        service = AgentService(build_settings(tmp_path / "inline"))
        view, _ = await service.create_task(complete_request(), CONTEXT)
        return (await service.get_task(view.task_id, CONTEXT)).status.value

    async def run_background() -> str:
        service = AgentService(build_settings(tmp_path / "background", execution_mode="background"))
        view, _ = await service.create_task(complete_request(), CONTEXT)
        return (await service.wait_for(view.task_id, CONTEXT)).status.value

    (tmp_path / "inline").mkdir()
    (tmp_path / "background").mkdir()
    assert run(run_inline()) == run(run_background()) == "DRAFT_READY"


def test_planned_at_survives_the_round_trip(tmp_path: Path) -> None:
    """槽位经落盘再读回不得丢失（时间带时区）。"""
    service = AgentService(build_settings(tmp_path))
    view, _ = run(service.create_task(complete_request(), CONTEXT))
    stored = service._repository.get(view.task_id)
    assert stored["slots"]["planned_at"]
    assert PLANNED_AT.isoformat() in stored["slots"]["planned_at"]
