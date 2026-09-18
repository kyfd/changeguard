"""编排层：把 HTTP 请求、工作流、存储串起来。

四条与计划一致的约束：

1. **可信上下文来自 API 层**，不是请求体，也不是工具参数。
2. **每个任务有超时**，超时按失败处理，而不是让状态永远停在 RUNNING。
3. **取消会真正停止后续工作**，包括正在等待模型调用的那一刻。
4. **执行受监督**：任何退出路径都必须留下「已落盘的终态」或「显式的降级记录」，
   两者必居其一。既不能假装成功，也不能让任务在没有监督者的情况下留在在途状态。

关于第 3、4 条的边界（不要过度宣称）：

- 取消是**协作式**的。工作流在 await 点被终止，因此不会再调度后续的工具或模型调用；
  但如果某个外部依赖不响应取消（例如同步阻塞调用），我们无法中断它正在进行的那个调用。
  能保证的是：不再调度后续步骤，且迟到结果不会被发布。
- `inline` 与 `background` 共用同一套受管理执行，区别只在等待方式。
"""

from __future__ import annotations

import asyncio
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from app.config import Settings
from app.llm.provider import build_provider
from app.retrieval.corpus import build_retriever
from app.schemas.drafts import (
    ClarifyRequest,
    CreateTaskRequest,
    DatabaseKind,
    TaskSlots,
    TaskStatus,
    TaskView,
)
from app.store.tasks import TaskRepository
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.workflow.checkpoint import has_checkpoint, open_checkpointer
from app.workflow.graph import DraftWorkflow, WorkflowDeps
from app.workflow.state import event, input_version, merge_slot_data

RESUMABLE_STATUSES = {TaskStatus.NEEDS_INFO.value, TaskStatus.FAILED.value, TaskStatus.CHECK_BLOCKED.value}

# 只有真正"在途"的状态算中断；终态（DRAFT_READY / CHECK_BLOCKED / INPUT_REJECTED / FAILED / CANCELLED）
# 不在其列，重启时保持原样。
INTERRUPTED_STATUSES = {TaskStatus.RECEIVED.value, TaskStatus.RUNNING.value}

# 终态落盘的结果。三种情形的含义不同，不能合并成一个 bool：
#   persisted —— 已落盘，任务有明确可查询的终态；
#   skipped   —— 已被取消或新执行接管，**不写才是对的**，不是故障；
#   failed    —— 用完重试仍写不进去，存储不可用，必须显式降级。
PERSIST_PERSISTED = "persisted"
PERSIST_SKIPPED = "skipped"
PERSIST_FAILED = "failed"

# 恢复模式。三者必须可区分：`interrupt` 是从节点级中断继续，`checkpoint` 是续跑未完成的节点，
# 两者都**不是**从头重跑；`restart_from_scratch` 才是重跑，且必须如实这样标注。
RESUME_INTERRUPT = "interrupt"
RESUME_CHECKPOINT = "checkpoint"


class TaskNotFound(Exception):
    """任务不存在。"""


class TaskNotResumable(Exception):
    """当前状态不允许继续。"""


class TaskCancelRejected(Exception):
    """取消失效：取消状态未能落盘，任务仍在运行。

    这是一个**可重试**的失败：调用方应当重试，而不是以为任务已经停下。
    """


class TaskStateUnavailable(Exception):
    """任务状态无法确定（例如终态未能落盘）。

    存在的意义是：不能返回一个可能已经过期的视图来冒充"当前状态"。
    """


@dataclass
class _Execution:
    """一次受管理的执行。"""

    task_id: str
    execution_id: str
    task: asyncio.Task[None]
    finished: asyncio.Event


@dataclass
class _ExecutionProblem:
    """执行留下的、值得外部知道的问题（用于健康状态与等待接口）。"""

    execution_id: str
    kind: str  # persist | exception
    detail: str
    recorded_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class AgentService:
    """变更准备 Agent 的服务实现。"""

    def __init__(self, settings: Settings, provider: Any | None = None) -> None:
        self._settings = settings
        self._retriever = build_retriever(settings.demo_dir)
        # provider 可注入：评估集需要构造"模型输出非法/冲突"等场景。
        self._provider = provider or build_provider(settings)
        self._repository = TaskRepository(settings.task_store_path)
        # 受管理的执行登记表：inline 与 background 都登记，二者因此都可被取消。
        self._executions: dict[str, _Execution] = {}
        # 未能留下可查询终态的执行（存储不可用）。健康状态据此报告降级。
        self._problems: dict[str, _ExecutionProblem] = {}
        # 启动即处理上次进程遗留的在途任务，避免状态永远停在 RUNNING。
        self.interrupted_task_ids = self._mark_interrupted_tasks()

    # -- 只读信息 ----------------------------------------------------------

    def provider_info(self) -> dict[str, Any]:
        info = self._provider.describe()
        info["llm_configured"] = self._settings.llm_configured
        return info

    def retriever_name(self) -> str:
        return self._retriever.name

    def build_tool_registry_for_introspection(self) -> Any:
        """返回工具清单（只有名称与 schema，不含业务数据）。"""
        return Toolbox(settings=self._settings, retriever=self._retriever, schema_snapshot="").build()

    def knowledge_base_size(self) -> int:
        return self._retriever.size()

    async def health(self) -> dict[str, Any]:
        running = sum(1 for execution in self._executions.values() if not execution.finished.is_set())
        unpersisted = sorted(self._problems)
        result: dict[str, Any] = {
            # 有未落盘的执行结果时不能再报 ok：那会让"存储不可用"看起来像"一切正常"。
            "status": "degraded" if unpersisted else "ok",
            "time": datetime.now(timezone.utc).isoformat(),
            "provider": self.provider_info(),
            "retriever": self.retriever_name(),
            "knowledge_chunks": self.knowledge_base_size(),
            "running_tasks": running,
            "unpersisted_tasks": unpersisted,
        }
        if unpersisted:
            result["degraded_reason"] = "存在未能落盘的执行结果，存储可能不可用"
        return result

    # -- 任务生命周期 ------------------------------------------------------

    async def create_task(
        self, request: CreateTaskRequest, context: TrustedContext
    ) -> tuple[TaskView, TaskSlots]:
        slots = self._slots_from_request(request)
        slots_payload = slots.model_dump(mode="json")
        requirement = request.requirement.strip()
        schema_snapshot = request.schema_snapshot or ""
        record: dict[str, Any] = {
            "task_id": f"task_{uuid.uuid4().hex[:12]}",
            "organization_id": context.organization_id,
            "user_id": context.user_id,
            "requirement": requirement,
            "slots": slots_payload,
            "schema_snapshot": schema_snapshot,
            "status": TaskStatus.RECEIVED.value,
            "questions": [],
            "draft": None,
            "events": [event("created", "任务已创建")],
            "revisions": 0,
            "error": None,
            # 输入与材料版本：恢复前必须与记录当前值一致，否则拒绝带着陈旧上下文继续。
            "input_version": input_version(
                requirement=requirement, slots=slots_payload, schema_snapshot=schema_snapshot
            ),
            "awaiting_input": False,
            "resume_count": 0,
        }
        self._repository.save(record)
        await self._dispatch_or_fail(record["task_id"])
        return self._view(self._require(record["task_id"])), slots

    async def clarify(self, task_id: str, request: ClarifyRequest, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        status = record.get("status")
        if status not in RESUMABLE_STATUSES:
            raise TaskNotResumable(f"当前状态 {status} 不允许补充信息")

        slots = TaskSlots.model_validate(record.get("slots") or {})
        updated = merge_slot_data(slots, request.model_dump())
        record["slots"] = updated.model_dump(mode="json")
        # 表结构快照是任务级材料而非槽位，但必须能在补充阶段补齐；
        # 只有显式提供才覆盖，避免把已有快照清空。
        if request.schema_snapshot is not None:
            record["schema_snapshot"] = request.schema_snapshot
        # 自由文本说明：以前被接收后直接丢弃。现在写进任务记录，
        # 并在下一次执行时作为「补充说明」拼进需求文本，模型确实能看到它。
        note = (request.note or "").strip()
        if note:
            notes = list(record.get("clarification_notes") or [])
            notes.append(note)
            record["clarification_notes"] = notes
        events = list(record.get("events") or [])
        provided = [
            name
            for name in (
                "application",
                "environment",
                "database",
                "table",
                "query_sql",
                "planned_at",
                "schema_snapshot",
                "note",
            )
            if getattr(request, name, None) is not None
        ]
        events.append(event("clarified", f"补充信息：{', '.join(provided) or '无字段变化'}"))
        # 输入/材料变了就作废旧草案与旧结果——不能带着陈旧上下文继续。
        _apply_input_version(record)
        record["events"] = events

        if record.get("awaiting_input"):
            # 图停在**节点级中断**上：把当前完整输入交给 interrupt()，从等待点继续，
            # 之前的节点不会重跑。
            record["awaiting_input"] = False
            record["status"] = TaskStatus.RUNNING.value
            record["error"] = None
            record["resume_mode"] = RESUME_INTERRUPT
            record["resume_count"] = int(record.get("resume_count") or 0) + 1
            self._repository.save(record)
            await self._dispatch_or_fail(
                task_id, resume={"mode": RESUME_INTERRUPT, "value": _resume_value(record)}
            )
            return self._view(self._require(task_id))

        # 没有待恢复的检查点：按既有语义重新执行（可能再次停在中断上）。
        record["status"] = TaskStatus.RECEIVED.value
        record["error"] = None
        record["resume_mode"] = None
        self._repository.save(record)
        await self._dispatch_or_fail(task_id)
        return self._view(self._require(task_id))

    async def resume(
        self, task_id: str, context: TrustedContext, request: ClarifyRequest | None = None
    ) -> TaskView:
        """从检查点恢复。

        恢复**之前**必须重新校验：归属（组织 + 创建者）、执行代际、输入与材料版本。
        任何一项不满足都抛 `TaskNotResumable`，任务保持原状——不做"尽力继续"。
        """
        record = self._authorize(task_id, context)
        status = record.get("status")
        # 终态（取消、输入被拒等）不允许借恢复回到执行路径。
        if status not in RESUMABLE_STATUSES:
            raise TaskNotResumable(f"当前状态 {status} 不允许恢复")
        if request is not None:
            slots = TaskSlots.model_validate(record.get("slots") or {})
            record["slots"] = merge_slot_data(slots, request.model_dump()).model_dump(mode="json")
            if request.schema_snapshot is not None:
                record["schema_snapshot"] = request.schema_snapshot
            note = (request.note or "").strip()
            if note:
                notes = list(record.get("clarification_notes") or [])
                notes.append(note)
                record["clarification_notes"] = notes
        _apply_input_version(record)

        checkpoint = await self._inspect_checkpoint(task_id)
        if checkpoint is None:
            raise TaskNotResumable("没有可恢复的检查点状态；请重新发起任务")

        if checkpoint["interrupt"]:
            # 停在节点级中断上：把当前完整输入交给 interrupt()。
            mode = RESUME_INTERRUPT
            value: Any = _resume_value(record)
        elif checkpoint["next"]:
            # 续跑未完成节点：输入必须与检查点一致，否则会带着陈旧的检索/检查结果继续。
            # 核对不出来（检查点没有记录版本）时**失败关闭**，不放行"尽力继续"。
            stored = checkpoint.get("input_version") or ""
            if not stored or stored != (record.get("input_version") or ""):
                raise TaskNotResumable("输入或材料已变更或无法核对，检查点结果不再适用；请重新发起任务")
            mode = RESUME_CHECKPOINT
            value = None
        else:
            raise TaskNotResumable("检查点没有待继续的步骤；请重新发起任务")

        record["awaiting_input"] = False
        record["status"] = TaskStatus.RUNNING.value
        record["error"] = None
        record["resume_mode"] = mode
        record["resume_count"] = int(record.get("resume_count") or 0) + 1
        events = list(record.get("events") or [])
        events.append(event("resumed", f"从检查点恢复（mode={mode}）"))
        record["events"] = events
        self._repository.save(record)
        await self._dispatch_or_fail(task_id, resume={"mode": mode, "value": value})
        return self._view(self._require(task_id))

    async def get_task(self, task_id: str, context: TrustedContext) -> TaskView:
        return self._view(self._authorize(task_id, context))

    async def list_tasks(self, context: TrustedContext) -> list[TaskView]:
        """只列出调用方**自己**创建的任务。

        组织与创建者都要匹配；可信上下文不完整时返回空列表而不是全部任务。
        """
        if not context.organization_id or not context.user_id:
            return []
        return [
            self._view(item)
            for item in self._repository.list()
            if (item.get("organization_id") or "").strip() == context.organization_id
            and (item.get("user_id") or "").strip() == context.user_id
        ]

    async def cancel(self, task_id: str, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        superseded_execution_id = record.get("execution_id") or ""

        # 先在**副本**上构造取消后的状态，并且先落盘。
        #
        # 顺序是关键：如果先终止执行、再落盘，一旦落盘失败就会留下一个
        # 「执行已经停了、仓储却仍是 RECEIVED」的孤儿——没有任何协程在推动它，
        # 健康检查还显示一切正常，clarify 又因为状态不允许而拒绝。
        # 所以落盘失败时必须什么都没发生，调用方拿到显式失败并可以重试。
        candidate = dict(record)
        candidate["run_generation"] = int(record.get("run_generation") or 0) + 1
        candidate["execution_id"] = ""
        candidate["status"] = TaskStatus.CANCELLED.value
        events = list(record.get("events") or [])
        events.append(event("cancelled", "任务被取消，后续步骤不再执行"))
        candidate["events"] = events

        # 落盘前确认没有被并发接管。`TaskRepository.save` 是同步的，且这里到落盘之间
        # 没有 await，因此在单进程事件循环里这一段是原子的（不会被其它协程插入）。
        latest = self._repository.get(task_id)
        if latest is not None and (latest.get("execution_id") or "") != superseded_execution_id:
            raise TaskNotResumable("执行已被新的执行接管，取消未生效，请重试")

        try:
            self._repository.save(candidate)
        except Exception as error:  # noqa: BLE001 - 任何落盘失败都必须变成显式的可重试失败
            raise TaskCancelRejected(
                f"取消未生效：状态未能落盘（{type(error).__name__}），任务仍在运行，请重试"
            ) from error

        # 落盘成功之后才终止执行并清理句柄，而且只清理**刚才那一个**执行。
        self._terminate_superseded_execution(task_id, superseded_execution_id)
        return self._view(candidate)

    async def wait_for(self, task_id: str, context: TrustedContext) -> TaskView:
        """等待后台执行结束（测试与演示用）。

        先校验归属再等待：否则等待本身就会变成一条无授权的存在性探测通道。

        执行结束后若发现它没能留下可查询的终态（存储不可用），必须显式失败，
        而不是把仓储里那份可能过期的记录当成"当前状态"返回。
        """
        self._authorize(task_id, context)
        execution = self._executions.get(task_id)
        if execution is not None:
            await execution.finished.wait()
        problem = self._problems.get(task_id)
        if problem is not None:
            raise TaskStateUnavailable(problem.detail)
        return self._view(self._authorize(task_id, context))

    # -- 内部 --------------------------------------------------------------

    async def _dispatch_or_fail(self, task_id: str, resume: dict[str, Any] | None = None) -> None:
        """派发执行；失败时把任务标为失败。

        否则记录会永远停在 `RECEIVED`：没有任何协程在推动它，也没有超时兜底，
        调用方看到的是一个"一直在运行"的任务。
        """
        try:
            await self._dispatch(task_id, resume)
        except (TaskStateUnavailable, TaskCancelRejected):
            # 存储本身已经出问题，再去写一条 FAILED 只会再失败一次并掩盖真正的原因。
            # 这类失败由调用方显式返回，健康状态另行报告降级。
            raise
        except Exception as error:
            record = self._repository.get(task_id)
            # 只有确实没跑起来（仍在途状态）才改状态，避免覆盖执行中已写入的终态。
            if record is not None and record.get("status") in INTERRUPTED_STATUSES:
                record["status"] = TaskStatus.FAILED.value
                record["error"] = f"任务未能启动：{type(error).__name__}: {error}"
                events = list(record.get("events") or [])
                events.append(event("dispatch_failed", record["error"]))
                record["events"] = events
                self._repository.save(record)
            raise

    async def _dispatch(self, task_id: str, resume: dict[str, Any] | None = None) -> None:
        """按配置决定等待完成还是立即返回。

        两种模式**共用同一套受管理执行**：都登记可取消的句柄，
        区别只在于是不是在这里等它结束。这是 inline 也能被真正取消的前提。
        """
        execution_id = self._begin_execution(task_id)
        if execution_id is None:
            return
        execution = self._start_execution(task_id, execution_id, resume)
        if self._settings.execution_mode == "inline":
            await self._await_execution(execution)

    def _start_execution(self, task_id: str, execution_id: str, resume: dict[str, Any] | None = None) -> _Execution:
        """把一次执行登记成独立的受管理任务。

        独立成 task 而不是直接 await 的好处：调用方的取消（客户端断开）与
        工作流的取消可以分开处理，且句柄始终可被 `cancel` 找到。
        """
        task = asyncio.create_task(
            self._run_managed(task_id, execution_id, resume), name=f"agent-exec:{task_id}:{execution_id[:8]}"
        )
        execution = _Execution(task_id=task_id, execution_id=execution_id, task=task, finished=asyncio.Event())
        self._executions[task_id] = execution
        # 用回调统一收口：既取走异常（否则只会以 "Task exception was never retrieved"
        # 的形式出现在日志里，没有任何机制能查询到），也负责释放句柄。
        task.add_done_callback(lambda finished: self._on_execution_done(execution, finished))
        return execution

    def _on_execution_done(self, execution: _Execution, task: asyncio.Task[None]) -> None:
        if not task.cancelled():
            error = task.exception()
            if error is not None:
                self._record_problem(
                    execution.task_id,
                    execution.execution_id,
                    "exception",
                    f"执行异常终止，状态未能确定：{type(error).__name__}: {error}",
                )
        # 释放句柄时必须校验身份，避免误删后来的新执行。
        if self._executions.get(execution.task_id) is execution:
            self._executions.pop(execution.task_id, None)
        execution.finished.set()

    async def _await_execution(self, execution: _Execution) -> None:
        """等待受管理执行结束，并把它的失败如实向上暴露。"""
        try:
            await execution.finished.wait()
        except asyncio.CancelledError:
            # 调用方自己被取消（例如 inline 请求断开）。内部执行不能变成孤儿：
            # 先终止它，再留下确定的终态，然后继续向上传播取消。
            self._terminate_execution(execution)
            self._abandon_execution(execution, "调用方取消：请求被中断，执行不再继续")
            raise
        problem = self._problems.get(execution.task_id)
        if problem is not None and problem.execution_id == execution.execution_id:
            raise TaskStateUnavailable(problem.detail)

    async def _run_managed(
        self, task_id: str, execution_id: str, resume: dict[str, Any] | None = None
    ) -> None:
        """受管理执行的顶层入口：保证任何退出路径都不留无监督的在途任务。"""
        try:
            record = await self._execute(task_id, execution_id, resume)
        except asyncio.CancelledError:
            # 取消的落盘由**发起方**负责，这里不写：
            #   - 用户 cancel()：先落盘 CANCELLED，再取消执行；
            #   - inline 调用方被取消：由 _await_execution 落盘 FAILED。
            # 两处都不在这里，避免出现第二个写入者互相覆盖。
            return
        if record is None:
            return  # 已被取消或新执行接管：不写才是对的
        outcome = await self._persist_terminal(task_id, execution_id, record)
        if outcome == PERSIST_FAILED:
            self._record_problem(
                task_id,
                execution_id,
                "persist",
                "执行已完成，但终态未能落盘：存储不可用，任务状态无法确定",
            )

    async def _persist_terminal(self, task_id: str, execution_id: str, record: dict[str, Any]) -> str:
        """带**有上限**重试的终态落盘，返回 persisted / skipped / failed。

        只对终态做重试：一次性的存储抖动不应该把一个已经跑完的任务变成孤儿，
        但也不能无限重试——重试用尽后必须显式降级，而不是假装写成功。
        """
        attempts = max(1, int(self._settings.persist_max_attempts))
        for attempt in range(attempts):
            if not self._owns(task_id, execution_id):
                return PERSIST_SKIPPED
            try:
                self._repository.save(record)
                return PERSIST_PERSISTED
            except Exception:  # noqa: BLE001 - 任何落盘失败都先按可重试处理
                if attempt + 1 >= attempts:
                    return PERSIST_FAILED
                # 退避期间若本执行被取消，CancelledError 应当继续向上传播：
                # 那时终态的所有权已经转移（用户取消或调用方放弃会各自落盘），
                # 在这里报一个"落盘失败"只会制造虚假的降级信号。
                await asyncio.sleep(self._settings.persist_retry_backoff_seconds * (attempt + 1))
        return PERSIST_FAILED

    def _record_problem(self, task_id: str, execution_id: str, kind: str, detail: str) -> None:
        self._problems[task_id] = _ExecutionProblem(execution_id=execution_id, kind=kind, detail=detail)

    def _terminate_superseded_execution(self, task_id: str, execution_id: str) -> None:
        """终止**刚被取代的那一个**执行。

        必须比对 execution_id：如果在这中间已经有新执行接管，误杀它会让一个
        正常推进的任务凭空停住。
        """
        execution = self._executions.get(task_id)
        if execution is None or execution.execution_id != execution_id:
            return
        self._terminate_execution(execution)

    def _terminate_execution(self, execution: _Execution) -> None:
        if not execution.task.done():
            execution.task.cancel()

    def _abandon_execution(self, execution: _Execution, reason: str) -> None:
        """为被放弃的执行留下确定终态。

        这里是**同步**写入（`TaskRepository.save` 是同步的）：它会在协程已被取消时执行，
        此时任何 await 都可能立刻再次抛出 CancelledError。
        仍然校验执行身份——若已被用户取消或新执行接管，就不写。
        """
        record = self._owned(execution.task_id, execution.execution_id)
        if record is None:
            return
        # 已经有确定的终态时不得覆盖：调用方被取消与执行完成之间可能只差一个事件循环轮次，
        # 把已经落盘的 DRAFT_READY 改写成 FAILED 是比"不写"更糟的结果。
        if (record.get("status") or "") not in INTERRUPTED_STATUSES:
            return
        record["status"] = TaskStatus.FAILED.value
        record["error"] = reason
        events = list(record.get("events") or [])
        events.append(event("abandoned", reason))
        record["events"] = events
        try:
            self._repository.save(record)
        except Exception as error:  # noqa: BLE001 - 连放弃都写不进去时必须显式降级，不能沉默
            self._record_problem(
                execution.task_id,
                execution.execution_id,
                "persist",
                f"放弃的执行未能落盘：{type(error).__name__}: {error}",
            )

    def _begin_execution(self, task_id: str) -> str | None:
        """开启一次新执行：递增代际并分配 execution_id。

        代际的作用是**让上一个执行失去写权限**。取消与重跑都会走到这里，
        因此旧协程不可能把自己的结果覆盖到新状态上。
        """
        record = self._repository.get(task_id)
        if record is None:
            return None
        execution_id = uuid.uuid4().hex
        record["run_generation"] = int(record.get("run_generation") or 0) + 1
        record["execution_id"] = execution_id
        self._repository.save(record)
        # 新执行意味着重新开始：清掉上一个执行留下的降级记录。
        self._problems.pop(task_id, None)
        return execution_id

    def _owns(self, task_id: str, execution_id: str) -> bool:
        record = self._repository.get(task_id)
        return record is not None and (record.get("execution_id") or "") == execution_id

    def _owned(self, task_id: str, execution_id: str) -> dict[str, Any] | None:
        """取记录，并确认本次执行仍持有所有权。"""
        record = self._repository.get(task_id)
        if record is None or (record.get("execution_id") or "") != execution_id:
            return None
        return record

    def _save_if_owner(self, task_id: str, execution_id: str, record: dict[str, Any]) -> None:
        """单次、带所有权校验的落盘。

        取消与被接管都会换掉 execution_id，于是旧执行在这里被拒绝写入。
        注意这里**不是**吞掉持久化失败：真正落盘出错时 `save` 会照常抛出。
        需要"失败后重试"的场景用 `_persist_terminal`。
        """
        if self._owned(task_id, execution_id) is None:
            return
        self._repository.save(record)

    async def _execute(
        self, task_id: str, execution_id: str, resume: dict[str, Any] | None = None
    ) -> dict[str, Any] | None:
        """运行工作流并把结果整理成待落盘的记录。返回 None 表示不应落盘。

        这里**不**吞掉 CancelledError，也不负责释放句柄：取消的语义由发起方决定，
        资源的释放由 `_on_execution_done` 统一负责。这样"执行结束"与"结果已落盘"
        就不会像以前那样互相抢顺序。
        """
        record = self._owned(task_id, execution_id)
        if record is None:
            # 已被更新的执行接管，或任务已被取消：旧执行不得再写任何状态。
            return None
        try:
            record = await asyncio.wait_for(
                self._run_workflow(record, resume),
                timeout=self._settings.task_timeout_seconds,
            )
        except asyncio.TimeoutError:
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"任务超过 {self._settings.task_timeout_seconds:.0f} 秒未完成，已终止"
            record.setdefault("events", []).append(event("timeout", record["error"]))
        except asyncio.CancelledError:
            raise
        except Exception as error:  # noqa: BLE001 - 任何执行异常都要转成显式失败状态
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"执行失败：{type(error).__name__}: {error}"
            record.setdefault("events", []).append(event("failed", record["error"]))
        return record

    def _mark_interrupted_tasks(self) -> list[str]:
        """启动时处理上次进程遗留的在途任务。

        与 P0 的区别：**有检查点的任务标为可恢复**，不再一律"中断不续跑"。
        但这里**不自动续跑**——恢复必须由创建者显式发起，并在恢复前重新校验
        归属、代际与输入版本（见 `resume`）。留在 RECEIVED/RUNNING 会让健康检查与界面
        看起来还有任务在推进，而实际上没有任何协程在推动它，因此仍显式落一个终态。
        """
        interrupted: list[str] = []
        for record in self._repository.list():
            if record.get("status") not in INTERRUPTED_STATUSES:
                continue
            task_id = str(record.get("task_id") or "")
            resumable = bool(task_id) and has_checkpoint(self._settings, task_id)
            record["status"] = TaskStatus.FAILED.value
            record["awaiting_input"] = False
            if resumable:
                record["error"] = "进程重启：任务在执行中被中断，存在可恢复的检查点，需由创建者显式恢复"
                record["restart_policy"] = "checkpoint_available"
            else:
                record["error"] = "进程重启：任务在执行中被中断，且没有可用的检查点，无法续跑"
                record["restart_policy"] = "interrupted_without_resume"
            events = list(record.get("events") or [])
            events.append(event("restart", record["error"]))
            record["events"] = events
            self._repository.save(record)
            if task_id:
                interrupted.append(task_id)
        return interrupted

    async def _run_workflow(
        self, record: dict[str, Any], resume: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        """运行（或恢复）工作流。检查点按 task_id 作为线程。

        - `resume is None`：全新执行，先清掉该线程的旧检查点，避免意外"接着上次跑"；
        - `mode=interrupt`：从节点级中断继续，之前的节点不会重跑；
        - `mode=checkpoint`：续跑最后未完成的节点。
        """
        task_id = record["task_id"]
        initial_state: dict[str, Any] = {
            "task_id": task_id,
            "requirement": _effective_requirement(record),
            "slots": record.get("slots") or {},
            "schema_snapshot": record.get("schema_snapshot") or "",
            "input_version": record.get("input_version") or "",
            "events": list(record.get("events") or []),
            "revisions": 0,
            "max_revisions": self._settings.max_revisions,
        }

        async with open_checkpointer(self._settings) as saver:
            workflow = self._build_workflow(record, saver)
            if resume is None:
                await _delete_thread(saver, task_id)
                record["resume_mode"] = None
                result = await workflow.run(initial_state, thread_id=task_id)  # type: ignore[arg-type]
            elif resume.get("mode") == RESUME_INTERRUPT:
                result = await workflow.resume_interrupt(thread_id=task_id, value=resume.get("value") or {})
            else:
                result = await workflow.continue_pending(thread_id=task_id)

        interrupts = result.get("__interrupt__") if isinstance(result, dict) else None
        if interrupts:
            # 图停在**节点级中断**上：状态是 NEEDS_INFO，但检查点可从此节点续跑。
            payload = _first_interrupt_value(interrupts)
            record["status"] = TaskStatus.NEEDS_INFO.value
            record["questions"] = payload.get("questions") or []
            record["error"] = None
            record["awaiting_input"] = True
            events = list(result.get("events") or record.get("events") or [])
            events.append(event("awaiting_input", "图停在节点级中断上，等待用户补充信息后从该节点继续"))
            record["events"] = events
            return record

        record["awaiting_input"] = False
        record.update(
            {
                "status": result.get("status", TaskStatus.FAILED.value),
                "questions": result.get("questions") or [],
                "draft": result.get("draft"),
                "events": result.get("events") or record.get("events") or [],
                "revisions": int(result.get("revisions") or 0),
                "error": result.get("error"),
                "evidence_note": result.get("evidence_note"),
            }
        )
        return record

    def _build_workflow(self, record: dict[str, Any], checkpointer: Any) -> DraftWorkflow:
        toolbox_factory = lambda snapshot: Toolbox(  # noqa: E731 - 需要按任务注入快照
            settings=self._settings,
            retriever=self._retriever,
            schema_snapshot=snapshot,
        )
        return DraftWorkflow(
            WorkflowDeps(
                settings=self._settings,
                provider=self._provider,
                trusted_context=TrustedContext(
                    user_id=record.get("user_id") or "",
                    organization_id=record.get("organization_id") or "",
                ),
                toolbox_factory=toolbox_factory,
                checkpointer=checkpointer,
            )
        )

    async def _inspect_checkpoint(self, task_id: str) -> dict[str, Any] | None:
        """读取检查点：是否有待恢复的工作、是否停在中断上、记录在案的输入版本。"""
        async with open_checkpointer(self._settings) as saver:
            workflow = self._build_workflow(self._require(task_id), saver)
            state = await workflow.snapshot(thread_id=task_id)
        values = getattr(state, "values", None) or {}
        next_nodes = list(getattr(state, "next", ()) or ())
        if not values and not next_nodes:
            return None  # 该线程没有检查点
        pending_interrupt = any(
            getattr(task, "interrupts", ()) for task in (getattr(state, "tasks", ()) or ())
        )
        return {
            "next": next_nodes,
            "interrupt": bool(pending_interrupt),
            "input_version": str(values.get("input_version") or ""),
        }

    def _require(self, task_id: str) -> dict[str, Any]:
        """按 ID 取记录，不校验归属。仅供已授权的内部路径使用。"""
        record = self._repository.get(task_id)
        if record is None:
            raise TaskNotFound(task_id)
        return record

    def _authorize(self, task_id: str, context: TrustedContext) -> dict[str, Any]:
        """按组织与创建者校验任务归属，通过则返回记录。

        失败一律抛 `TaskNotFound`：**不区分"任务不存在"与"不属于你"**。
        否则调用方可以用状态码差异探测其他组织是否存在某个任务。

        默认策略是**仅创建者可操作**。仓库目前没有任何组织共享或管理员代管的规范，
        因此不隐式授予同组织其他用户的读写权；将来若引入共享策略，
        必须显式实现并在此处放开，而不是靠"同组织就算通过"的模糊默认。
        """
        if not context.organization_id or not context.user_id:
            # 可信上下文不完整时失败关闭，不允许退化成"只看记录存在性"。
            raise TaskNotFound(task_id)
        record = self._require(task_id)
        owner_organization = (record.get("organization_id") or "").strip()
        owner_user = (record.get("user_id") or "").strip()
        # 旧记录缺少组织或创建者信息时同样失败关闭，不对所有人开放。
        if not owner_organization or not owner_user:
            raise TaskNotFound(task_id)
        if owner_organization != context.organization_id or owner_user != context.user_id:
            raise TaskNotFound(task_id)
        return record

    @staticmethod
    def _slots_from_request(request: CreateTaskRequest) -> TaskSlots:
        return TaskSlots(
            application=request.application,
            environment=request.environment,
            database=request.database,
            table=request.table,
            query_sql=request.query_sql,
            planned_at=request.planned_at,
            planned_at_timezone=request.planned_at_timezone,
        )

    @staticmethod
    def _view(record: dict[str, Any]) -> TaskView:
        return TaskView.model_validate(
            {
                "task_id": record["task_id"],
                "status": record.get("status", TaskStatus.FAILED.value),
                "requirement": record.get("requirement", ""),
                "slots": record.get("slots") or {},
                "questions": record.get("questions") or [],
                "draft": record.get("draft"),
                "events": record.get("events") or [],
                "revisions": int(record.get("revisions") or 0),
                "error": record.get("error"),
                "planned_at_missing": "planned_at" in (TaskSlots.model_validate(record.get("slots") or {}).missing()),
                "awaiting_input": bool(record.get("awaiting_input")),
                "resume_mode": record.get("resume_mode"),
                "restart_policy": record.get("restart_policy"),
            }
        )


def _effective_requirement(record: dict[str, Any]) -> str:
    """把用户后续提供的补充说明并入需求文本，并标注来源，避免与原始需求混为一谈。"""
    notes = [str(item).strip() for item in (record.get("clarification_notes") or []) if str(item).strip()]
    requirement = record.get("requirement", "")
    if notes:
        requirement = requirement + "\n\n补充说明（用户后续提供）：\n" + "\n".join(f"- {item}" for item in notes)
    return requirement


def _apply_input_version(record: dict[str, Any]) -> None:
    """重算输入/材料版本；一旦变化，旧草案与旧检查结果即失效。"""
    version = input_version(
        requirement=_effective_requirement(record),
        slots=record.get("slots") or {},
        schema_snapshot=record.get("schema_snapshot") or "",
    )
    if version != record.get("input_version"):
        record["draft"] = None
        record["questions"] = []
        record["input_version"] = version


def _resume_value(record: dict[str, Any]) -> dict[str, Any]:
    """交给 `interrupt()` 的恢复值：当前**完整**输入（而非增量），使重复恢复幂等。"""
    return {
        "slots": dict(record.get("slots") or {}),
        "schema_snapshot": record.get("schema_snapshot") or "",
        "requirement": _effective_requirement(record),
        "input_version": record.get("input_version") or "",
    }


def _first_interrupt_value(interrupts: Any) -> dict[str, Any]:
    """从 `__interrupt__` 里取出节点中断的 payload（只取第一个）。"""
    try:
        first = interrupts[0]
    except (TypeError, IndexError, KeyError):
        return {}
    value = getattr(first, "value", None)
    return value if isinstance(value, dict) else {}


async def _delete_thread(saver: Any, thread_id: str) -> None:
    """清掉某线程的检查点（全新执行前调用）。"""
    delete = getattr(saver, "adelete_thread", None)
    if delete is not None:
        await delete(thread_id)


__all__ = ["AgentService", "TaskNotFound", "TaskNotResumable", "DatabaseKind"]
