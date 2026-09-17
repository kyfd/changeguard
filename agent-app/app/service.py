"""编排层：把 HTTP 请求、工作流、存储串起来。

三条与计划一致的约束：

1. **可信上下文来自 API 层**，不是请求体，也不是工具参数。
2. **每个任务有超时**，超时按失败处理，而不是让状态永远停在 RUNNING。
3. **取消会真正停止后续工作**，包括正在等待模型调用的那一刻。
"""

from __future__ import annotations

import asyncio
import uuid
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
from app.workflow.graph import DraftWorkflow, WorkflowDeps
from app.workflow.state import event

RESUMABLE_STATUSES = {TaskStatus.NEEDS_INFO.value, TaskStatus.FAILED.value, TaskStatus.CHECK_BLOCKED.value}

# 只有真正"在途"的状态算中断；终态（DRAFT_READY / CHECK_BLOCKED / INPUT_REJECTED / FAILED / CANCELLED）
# 不在其列，重启时保持原样。
INTERRUPTED_STATUSES = {TaskStatus.RECEIVED.value, TaskStatus.RUNNING.value}


class TaskNotFound(Exception):
    """任务不存在。"""


class TaskNotResumable(Exception):
    """当前状态不允许继续。"""


class AgentService:
    """变更准备 Agent 的服务实现。"""

    def __init__(self, settings: Settings, provider: Any | None = None) -> None:
        self._settings = settings
        self._retriever = build_retriever(settings.demo_dir)
        # provider 可注入：评估集需要构造"模型输出非法/冲突"等场景。
        self._provider = provider or build_provider(settings)
        self._repository = TaskRepository(settings.task_store_path)
        self._running: dict[str, asyncio.Task[None]] = {}
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
        running = sum(1 for task in self._running.values() if not task.done())
        return {
            "status": "ok",
            "time": datetime.now(timezone.utc).isoformat(),
            "provider": self.provider_info(),
            "retriever": self.retriever_name(),
            "knowledge_chunks": self.knowledge_base_size(),
            "running_tasks": running,
        }

    # -- 任务生命周期 ------------------------------------------------------

    async def create_task(
        self, request: CreateTaskRequest, context: TrustedContext
    ) -> tuple[TaskView, TaskSlots]:
        slots = self._slots_from_request(request)
        record: dict[str, Any] = {
            "task_id": f"task_{uuid.uuid4().hex[:12]}",
            "organization_id": context.organization_id,
            "user_id": context.user_id,
            "requirement": request.requirement.strip(),
            "slots": slots.model_dump(mode="json"),
            "schema_snapshot": request.schema_snapshot or "",
            "status": TaskStatus.RECEIVED.value,
            "questions": [],
            "draft": None,
            "events": [event("created", "任务已创建")],
            "revisions": 0,
            "error": None,
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
        updated = _merge_slots(slots, request)
        record["slots"] = updated.model_dump(mode="json")
        record["status"] = TaskStatus.RECEIVED.value
        record["error"] = None
        events = list(record.get("events") or [])
        provided = [
            name
            for name in ("application", "environment", "database", "table", "query_sql", "planned_at")
            if getattr(request, name, None) is not None
        ]
        events.append(event("clarified", f"补充信息：{', '.join(provided) or '无字段变化'}"))
        record["events"] = events
        self._repository.save(record)
        await self._dispatch_or_fail(task_id)
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
        # 先让在途执行失去所有权（递增代际 + 清空 execution_id），再取消协程。
        # 顺序反过来的话，被取消的协程在退出前仍可能把自己的结果写回去，
        # 把"取消后仍是成功态"暴露给调用方。
        record["run_generation"] = int(record.get("run_generation") or 0) + 1
        record["execution_id"] = ""
        running = self._running.pop(task_id, None)
        if running and not running.done():
            running.cancel()
        record["status"] = TaskStatus.CANCELLED.value
        events = list(record.get("events") or [])
        # 取消后不再继续任何后续工作。
        events.append(event("cancelled", "任务被取消，后续步骤不再执行"))
        record["events"] = events
        self._repository.save(record)
        return self._view(record)

    async def wait_for(self, task_id: str, context: TrustedContext) -> TaskView:
        """等待后台执行结束（测试与演示用）。

        先校验归属再等待：否则等待本身就会变成一条无授权的存在性探测通道。
        """
        self._authorize(task_id, context)
        running = self._running.get(task_id)
        if running:
            try:
                await running
            except asyncio.CancelledError:
                pass
        return self._view(self._authorize(task_id, context))

    # -- 内部 --------------------------------------------------------------

    async def _dispatch_or_fail(self, task_id: str) -> None:
        """派发执行；失败时把任务标为失败。

        否则记录会永远停在 `RECEIVED`：没有任何协程在推动它，也没有超时兜底，
        调用方看到的是一个"一直在运行"的任务。
        """
        try:
            await self._dispatch(task_id)
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

    async def _dispatch(self, task_id: str) -> None:
        """按配置决定同步执行还是交给后台任务。"""
        execution_id = self._begin_execution(task_id)
        if execution_id is None:
            return
        if self._settings.execution_mode == "inline":
            await self._execute(task_id, execution_id)
            return
        self._running[task_id] = asyncio.create_task(self._execute(task_id, execution_id))

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
        return execution_id

    def _owned(self, task_id: str, execution_id: str) -> dict[str, Any] | None:
        """取记录，并确认本次执行仍持有所有权。"""
        record = self._repository.get(task_id)
        if record is None or (record.get("execution_id") or "") != execution_id:
            return None
        return record

    def _save_if_owner(self, task_id: str, execution_id: str, record: dict[str, Any]) -> None:
        """只在仍持有执行所有权时落盘。

        取消与被接管都会换掉 execution_id，于是旧执行在这里被拒绝写入。
        注意这里**不是**吞掉持久化失败：真正落盘出错时 `save` 会照常抛出。
        """
        if self._owned(task_id, execution_id) is None:
            return
        self._repository.save(record)

    def _release(self, task_id: str) -> None:
        """释放执行句柄；只有当前句柄仍是自己时才移除，避免误删新执行的句柄。"""
        if self._running.get(task_id) is asyncio.current_task():
            self._running.pop(task_id, None)

    async def _execute(self, task_id: str, execution_id: str) -> None:
        record = self._owned(task_id, execution_id)
        if record is None:
            # 已被更新的执行接管，或任务已被取消：旧执行不得再写任何状态。
            return
        try:
            record = await asyncio.wait_for(
                self._run_workflow(record),
                timeout=self._settings.task_timeout_seconds,
            )
        except asyncio.TimeoutError:
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"任务超过 {self._settings.task_timeout_seconds:.0f} 秒未完成，已终止"
            record.setdefault("events", []).append(event("timeout", record["error"]))
        except asyncio.CancelledError:
            # 取消状态由 cancel() 负责写入；被取消的执行不再落盘，
            # 否则就会把"取消后仍然是成功态"暴露给调用方。
            self._release(task_id)
            return
        except Exception as error:  # noqa: BLE001 - 任何执行异常都要转成显式失败状态
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"执行失败：{type(error).__name__}: {error}"
            record.setdefault("events", []).append(event("failed", record["error"]))
        self._release(task_id)
        self._save_if_owner(task_id, execution_id, record)

    def _mark_interrupted_tasks(self) -> list[str]:
        """启动时把上次进程遗留的在途任务显式标为中断失败。

        P0 **不做安全续跑**——那需要检查点、重新授权与输入版本校验（见 P2 规划）。
        这里刻意不宣称"已恢复"：留在 RECEIVED/RUNNING 会让健康检查与界面看起来
        还有任务在推进，而实际上没有任何协程在推动它。
        """
        interrupted: list[str] = []
        for record in self._repository.list():
            if record.get("status") not in INTERRUPTED_STATUSES:
                continue
            task_id = str(record.get("task_id") or "")
            record["status"] = TaskStatus.FAILED.value
            record["error"] = "进程重启：任务在执行中被中断，本版本不进行自动续跑"
            record["restart_policy"] = "interrupted_without_resume"
            events = list(record.get("events") or [])
            events.append(event("restart", record["error"]))
            record["events"] = events
            self._repository.save(record)
            if task_id:
                interrupted.append(task_id)
        return interrupted

    async def _run_workflow(self, record: dict[str, Any]) -> dict[str, Any]:
        toolbox_factory = lambda snapshot: Toolbox(  # noqa: E731 - 需要按任务注入快照
            settings=self._settings,
            retriever=self._retriever,
            schema_snapshot=snapshot,
        )
        workflow = DraftWorkflow(
            WorkflowDeps(
                settings=self._settings,
                provider=self._provider,
                trusted_context=TrustedContext(
                    user_id=record.get("user_id") or "",
                    organization_id=record.get("organization_id") or "",
                ),
                toolbox_factory=toolbox_factory,
            )
        )

        initial_state: dict[str, Any] = {
            "task_id": record["task_id"],
            "requirement": record.get("requirement", ""),
            "slots": record.get("slots") or {},
            "schema_snapshot": record.get("schema_snapshot") or "",
            "events": list(record.get("events") or []),
            "revisions": 0,
            "max_revisions": self._settings.max_revisions,
        }
        result = await workflow.run(initial_state)  # type: ignore[arg-type]

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
            }
        )


def _merge_slots(slots: TaskSlots, request: ClarifyRequest) -> TaskSlots:
    """只覆盖调用方实际提供的字段。"""
    data = slots.model_dump()
    for field in ("application", "environment", "database", "table", "query_sql", "planned_at", "planned_at_timezone"):
        value = getattr(request, field, None)
        if value is not None:
            data[field] = value
    return TaskSlots.model_validate(data)


__all__ = ["AgentService", "TaskNotFound", "TaskNotResumable", "DatabaseKind"]
