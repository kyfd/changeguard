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
        await self._dispatch(record["task_id"])
        return self._view(self._require(record["task_id"])), slots

    async def clarify(self, task_id: str, request: ClarifyRequest) -> TaskView:
        record = self._require(task_id)
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
        await self._dispatch(task_id)
        return self._view(self._require(task_id))

    async def get_task(self, task_id: str) -> TaskView:
        return self._view(self._require(task_id))

    async def list_tasks(self) -> list[TaskView]:
        return [self._view(item) for item in self._repository.list()]

    async def cancel(self, task_id: str) -> TaskView:
        record = self._require(task_id)
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

    async def wait_for(self, task_id: str) -> TaskView:
        """等待后台执行结束（测试与演示用）。"""
        running = self._running.get(task_id)
        if running:
            try:
                await running
            except asyncio.CancelledError:
                pass
        return self._view(self._require(task_id))

    # -- 内部 --------------------------------------------------------------

    async def _dispatch(self, task_id: str) -> None:
        """按配置决定同步执行还是交给后台任务。"""
        if self._settings.execution_mode == "inline":
            await self._execute(task_id)
            return
        self._start(task_id)

    def _start(self, task_id: str) -> None:
        existing = self._running.get(task_id)
        if existing and not existing.done():
            existing.cancel()
        self._running[task_id] = asyncio.create_task(self._execute(task_id))

    async def _execute(self, task_id: str) -> None:
        record = self._repository.get(task_id)
        if record is None:
            return
        try:
            final = await asyncio.wait_for(
                self._run_workflow(record),
                timeout=self._settings.task_timeout_seconds,
            )
        except asyncio.TimeoutError:
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"任务超过 {self._settings.task_timeout_seconds:.0f} 秒未完成，已终止"
            record.setdefault("events", []).append(event("timeout", record["error"]))
        except asyncio.CancelledError:
            # 取消由 cancel() 负责写状态，这里只保证不再继续。
            record["status"] = TaskStatus.CANCELLED.value
            record.setdefault("events", []).append(event("cancelled", "执行被中断"))
            raise
        else:
            record = final
        finally:
            self._repository.save(record)

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
        record = self._repository.get(task_id)
        if record is None:
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
