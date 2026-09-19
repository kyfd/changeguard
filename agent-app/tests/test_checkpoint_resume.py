"""节点级中断与从检查点恢复。

这里锁定的三件事，缺一件就不是"续跑"：

1. 缺信息时图**停在节点上**（`interrupt`），而不是走到 END；
2. 用户补充后**从该节点继续**，中断之前的节点不会重跑；
3. 恢复**之前**重新校验归属、代际与输入/材料版本，校验不过就拒绝，不做"尽力继续"。

"确实是从检查点继续"用可核对的事实证明：`screen_input` 事件在整条生命周期里只出现一次，
以及恢复记录里带有 `resume_mode`。
"""

from __future__ import annotations

from pathlib import Path

import pytest

from app.config import Settings
from app.schemas.drafts import ClarifyRequest, TaskStatus
from app.service import AgentService, TaskNotFound, TaskNotResumable
from app.tools.registry import TrustedContext
from app.workflow.graph import DraftWorkflow
from tests.conftest import DEMO_DIR, bare_request, complete_request, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")


def full_clarification() -> ClarifyRequest:
    """把 bare_request 缺的字段一次性补齐。"""
    req = complete_request()
    return ClarifyRequest(
        application=req.application,
        environment=req.environment,
        database=req.database,
        table=req.table,
        query_sql=req.query_sql,
        planned_at=req.planned_at,
        planned_at_timezone=req.planned_at_timezone,
        schema_snapshot=req.schema_snapshot,
    )


def kinds(view) -> list[str]:
    return [item.kind for item in view.events]


# ---------------------------------------------------------------------------
# 节点级中断
# ---------------------------------------------------------------------------


def test_missing_info_pauses_on_a_node_level_interrupt(settings: Settings) -> None:
    """缺信息时图停在节点上：状态是 NEEDS_INFO，但带有可续跑的中断标记。"""
    service = AgentService(settings)

    view, _ = run(service.create_task(bare_request(), CONTEXT))

    assert view.status is TaskStatus.NEEDS_INFO
    assert view.awaiting_input is True, "必须标记图停在节点级中断上"
    assert view.questions, "必须把缺失项转成追问"
    assert kinds(view).count("screen_input") == 1, "首次执行只应经过一次入口筛查"


def test_clarify_continues_from_the_interrupt_without_rerunning_the_first_node(settings: Settings) -> None:
    """补充信息后从等待点继续；中断前的节点不得重跑。"""
    service = AgentService(settings)
    paused, _ = run(service.create_task(bare_request(), CONTEXT))
    assert paused.status is TaskStatus.NEEDS_INFO

    continued = run(service.clarify(paused.task_id, full_clarification(), CONTEXT))

    assert continued.status is TaskStatus.DRAFT_READY, continued.error
    assert continued.draft is not None
    assert kinds(continued).count("screen_input") == 1, "恢复不得从头重跑：screen_input 只应执行一次"
    assert "resumed" in kinds(continued)
    assert continued.resume_mode == "interrupt"


# ---------------------------------------------------------------------------
# 受支持的失败重试（续跑未完成的节点）
# ---------------------------------------------------------------------------


def test_supported_failure_retries_from_the_pending_node(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """节点失败后，检查点停在那个节点上；恢复续跑它，而不是重头再来。"""
    service = AgentService(settings)
    calls = {"n": 0}
    original = DraftWorkflow._generate_draft

    async def flaky(self, state):  # noqa: ANN001
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("模拟执行中途中断")
        return await original(self, state)

    monkeypatch.setattr(DraftWorkflow, "_generate_draft", flaky)

    failed, _ = run(service.create_task(complete_request(), CONTEXT))
    assert failed.status is TaskStatus.FAILED, failed.status

    # 不再注入失败：恢复时应续跑那个失败的节点，且**只**跑它一次。
    resumed = run(service.resume(failed.task_id, CONTEXT))

    assert resumed.status is TaskStatus.DRAFT_READY, resumed.error
    assert resumed.resume_mode == "checkpoint", "续跑未完成节点必须如实标注为 checkpoint"
    # 中断前的节点不得重跑：入口筛查与检索都只应执行一次。
    assert kinds(resumed).count("screen_input") == 1, "续跑不得从头重跑"
    assert kinds(resumed).count("retrieve_evidence") == 1, "中断前的检索节点不得重跑"
    assert calls["n"] >= 2, "失败的生成节点应在恢复时重新执行"


# ---------------------------------------------------------------------------
# 恢复的执行语义：不宣称 exactly-once
# ---------------------------------------------------------------------------


def test_resuming_a_pending_node_is_labelled_at_least_once(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """续跑"已开始但未完成"的节点，必须在记录与事件里如实标为不确定。

    检查点保证的是"从哪个节点继续"，不是"那个节点没有执行过"。被中断的节点可能已经把
    外部模型请求发出去了，重跑它就是 at-least-once——不能因为"恢复了"就假装那次调用不存在。
    """
    service = AgentService(settings)
    calls = {"n": 0}
    original = DraftWorkflow._generate_draft

    async def fail_once(self, state):  # noqa: ANN001
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("模拟执行中途中断")
        return await original(self, state)

    monkeypatch.setattr(DraftWorkflow, "_generate_draft", fail_once)

    failed, _ = run(service.create_task(complete_request(), CONTEXT))
    assert failed.status is TaskStatus.FAILED
    assert failed.recovery_semantics is None, "尚未恢复时不应预先声明恢复语义"

    resumed = run(service.resume(failed.task_id, CONTEXT))

    assert resumed.status is TaskStatus.DRAFT_READY, resumed.error
    assert resumed.resume_mode == "checkpoint"
    assert resumed.recovery_semantics == "at_least_once", "恢复必须如实标注 at-least-once"
    events = {item.kind: item.detail for item in resumed.events}
    assert "recovery_at_least_once" in events, "不确定性必须留下事件痕迹，而不是只改字段"
    assert "exactly-once" in events["recovery_at_least_once"]


def test_resuming_from_an_interrupt_is_labelled_at_least_once(settings: Settings) -> None:
    """从节点级中断继续同样会重新执行该节点，因此同样是 at-least-once。"""
    service = AgentService(settings)
    paused, _ = run(service.create_task(bare_request(), CONTEXT))
    assert paused.status is TaskStatus.NEEDS_INFO
    assert paused.recovery_semantics is None

    continued = run(service.clarify(paused.task_id, full_clarification(), CONTEXT))

    assert continued.status is TaskStatus.DRAFT_READY, continued.error
    assert continued.resume_mode == "interrupt"
    assert continued.recovery_semantics == "at_least_once"
    assert "recovery_at_least_once" in [item.kind for item in continued.events]


# ---------------------------------------------------------------------------
# 恢复前的重新校验
# ---------------------------------------------------------------------------


def test_resume_requires_the_creator_and_the_same_organization(settings: Settings) -> None:
    service = AgentService(settings)
    paused, _ = run(service.create_task(bare_request(), CONTEXT))
    assert paused.status is TaskStatus.NEEDS_INFO

    with pytest.raises(TaskNotFound):
        run(service.resume(paused.task_id, TrustedContext(user_id="bob", organization_id="org_demo")))
    with pytest.raises(TaskNotFound):
        run(service.resume(paused.task_id, TrustedContext(user_id="alice", organization_id="org_other")))


def test_resume_refuses_when_nothing_is_pending(settings: Settings) -> None:
    """已到终态、检查点没有待继续步骤时不得"尽力继续"。"""
    service = AgentService(settings)
    done, _ = run(service.create_task(complete_request(), CONTEXT))
    assert done.status is TaskStatus.DRAFT_READY

    with pytest.raises(TaskNotResumable):
        run(service.resume(done.task_id, CONTEXT))


def test_resume_refuses_a_checkpoint_from_an_older_input_version(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """输入/材料变了，旧检查点结果不再适用——必须拒绝，而不是带着陈旧上下文继续。"""
    service = AgentService(settings)
    original = DraftWorkflow._generate_draft

    async def fail_once(self, state):  # noqa: ANN001
        raise RuntimeError("模拟执行中途中断")

    monkeypatch.setattr(DraftWorkflow, "_generate_draft", fail_once)
    failed, _ = run(service.create_task(complete_request(), CONTEXT))
    assert failed.status is TaskStatus.FAILED
    monkeypatch.setattr(DraftWorkflow, "_generate_draft", original)

    # 模拟用户改了输入/材料：真正改动槽位，使重算出的版本与检查点里的不一致。
    record = service._repository.get(failed.task_id)
    record["slots"] = {**(record.get("slots") or {}), "table": "another_table"}
    service._repository.save(record)

    with pytest.raises(TaskNotResumable) as error:
        run(service.resume(failed.task_id, CONTEXT))
    assert "输入" in str(error.value) or "材料" in str(error.value)


# ---------------------------------------------------------------------------
# 进程重启时的处置
# ---------------------------------------------------------------------------


def test_restart_marks_checkpointed_tasks_as_resumable(settings: Settings) -> None:
    """有检查点的在途任务标为可恢复（需创建者显式恢复），而不是谎称已跑完。"""
    service = AgentService(settings)
    view, _ = run(service.create_task(complete_request(), CONTEXT))
    record = service._repository.get(view.task_id)
    record["status"] = TaskStatus.RUNNING.value
    service._repository.save(record)

    restarted = AgentService(settings)
    after = restarted._repository.get(view.task_id)

    assert after["status"] == TaskStatus.FAILED.value
    assert after["restart_policy"] == "checkpoint_available"


def test_restart_without_a_checkpoint_is_marked_unresumable(settings: Settings) -> None:
    """没有检查点的在途任务如实标注"中断且无法续跑"。"""
    service = AgentService(settings)
    service._repository.save(
        {
            "task_id": "task_no_checkpoint",
            "organization_id": CONTEXT.organization_id,
            "user_id": CONTEXT.user_id,
            "requirement": "给订单表准备索引变更。",
            "slots": {},
            "status": TaskStatus.RUNNING.value,
            "events": [],
        }
    )

    restarted = AgentService(settings)
    after = restarted._repository.get("task_no_checkpoint")

    assert after["status"] == TaskStatus.FAILED.value
    assert after["restart_policy"] == "interrupted_without_resume"


# ---------------------------------------------------------------------------
# 不破坏既有语义（回归）
# ---------------------------------------------------------------------------


def test_cancelled_task_cannot_be_resumed(settings: Settings) -> None:
    """取消是终态，不允许借恢复回到执行路径。"""
    service = AgentService(settings)
    paused, _ = run(service.create_task(bare_request(), CONTEXT))
    cancelled = run(service.cancel(paused.task_id, CONTEXT))
    assert cancelled.status is TaskStatus.CANCELLED

    with pytest.raises(TaskNotResumable):
        run(service.resume(paused.task_id, CONTEXT))


# ---------------------------------------------------------------------------
# 检查点路径：不得共用仓库里的那一个文件
# ---------------------------------------------------------------------------


def test_checkpoint_file_defaults_next_to_the_task_store(tmp_path: Path) -> None:
    """未显式配置时检查点与任务存储同目录；显式配置优先。"""
    derived = Settings(task_store_path=str(tmp_path / "agent-tasks.json"))
    assert derived.checkpoint_file == str(tmp_path / "agent-checkpoints.sqlite")

    explicit = Settings(
        task_store_path=str(tmp_path / "agent-tasks.json"),
        checkpoint_path=str(tmp_path / "custom.sqlite"),
    )
    assert explicit.checkpoint_file == str(tmp_path / "custom.sqlite")


def test_service_writes_its_checkpoint_next_to_the_task_store(tmp_path: Path) -> None:
    """换了任务存储路径，检查点也必须跟着走，不再共用默认路径上的同一个文件。

    共用同一个 SQLite 文件会让用例之间互相污染状态，并在并发打开时产生锁冲突。
    """
    scoped = Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "tasks.json"),
        execution_mode="inline",
        max_revisions=0,
    )
    service = AgentService(scoped)

    run(service.create_task(complete_request(), CONTEXT))

    assert (tmp_path / "agent-checkpoints.sqlite").exists(), "检查点应写在任务存储旁边"
