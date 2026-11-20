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
import difflib
import hashlib
import json
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

import httpx

from app.buildinfo import current as current_buildinfo
from app.budget import PHASE_GENERATE, LedgerPersistError, UsageLedger, usage_scope
from app.config import Settings
from app.evalcenter import (
    LIVE_ENABLE_HINT,
    LIVE_NOT_RUN_NOTE,
    EvalInvalid,
    EvalNotFound,
    EvalRepository,
    EvalStateUnavailable,
    aggregate_failures,
    compare_jobs,
    job_view,
    new_job_record,
    run_evaluation,
)
from app.guard import detect_injection
from app.knowledge import KnowledgeRepository, chunks_for, import_record, visible_chunks
from app.llm.provider import build_provider
from app.retrieval.corpus import build_retriever
from app.retrieval.keyword import HybridRetriever
from app.schemas.evals import EvalComparison, EvalJobRequest, EvalJobView, EvalReport
from app.schemas.knowledge import (
    KIND_PREFIX,
    KnowledgeDetail,
    KnowledgeImportRequest,
    KnowledgeSearchHit,
    KnowledgeSnippet,
    KnowledgeView,
)
from app.schemas.drafts import (
    ChangeLink,
    ClarifyRequest,
    ConfirmRequest,
    CreateTaskRequest,
    DatabaseKind,
    DeleteTaskPreview,
    DeterministicCheck,
    DraftEditRequest,
    DraftVersion,
    DraftVersionDiff,
    LinkChangeRequest,
    SelectedSnapshot,
    TaskSlots,
    TaskStatus,
    TaskTrace,
    TaskView,
)
from app.store.tasks import TaskRepository
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.trace import build_trace
from app.workflow.checkpoint import has_checkpoint, open_checkpointer
from app.workflow.graph import DraftWorkflow, WorkflowDeps
from app.workflow.state import event, input_version, material_hash, merge_slot_data

from app.service_common import (
    INTERRUPTED_STATUSES,
    PERSIST_FAILED,
    PERSIST_PERSISTED,
    PERSIST_SKIPPED,
    RECOVERY_AT_LEAST_ONCE,
    RECOVERY_UNCERTAINTY_NOTE,
    RESUMABLE_STATUSES,
    RESUME_CHECKPOINT,
    RESUME_INTERRUPT,
    ApplicationNotAuthorized,
    KnowledgeForbidden,
    KnowledgeInvalid,
    KnowledgeNotFound,
    KnowledgeStateUnavailable,
    TaskCancelRejected,
    TaskInputInvalid,
    TaskNotConfirmable,
    TaskNotFound,
    TaskNotResumable,
    TaskStateUnavailable,
    _Execution,
    _ExecutionProblem,
    _annotate_recovery,
    _apply_input_version,
    _current_material_hash,
    _delete_thread,
    _draft_version_entry,
    _effective_requirement,
    _first_interrupt_value,
    _invalidate_stale_confirmations,
    _material_summary,
    _merge_event_lists,
    _record_draft_version,
    _resume_value,
    _same_event,
    _screen_new_input,
    _unified_diff,
)
from app.service_evals import EvalServiceMixin
from app.service_execution import ExecutionSupervisorMixin
from app.service_knowledge import KnowledgeServiceMixin


class AgentService(KnowledgeServiceMixin, EvalServiceMixin, ExecutionSupervisorMixin):
    """变更准备 Agent 的服务实现。"""
    def __init__(self, settings: Settings, provider: Any | None = None) -> None:
        self._settings = settings
        self._retriever = build_retriever(settings.demo_dir)
        # provider 可注入：评估集需要构造"模型输出非法/冲突"等场景。
        self._provider = provider or build_provider(settings)
        self._repository = TaskRepository(settings.task_store_path)
        self._knowledge = KnowledgeRepository(settings.knowledge_file or "data/agent-knowledge.json")
        self._evals = EvalRepository(str(settings.eval_dir / "eval-jobs.json"))
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
            # 构建身份：只报告本进程自己的标识，不含任何业务数据。
            "build": current_buildinfo().as_dict(),
        }
        if unpersisted:
            result["degraded_reason"] = "存在未能落盘的执行结果，存储可能不可用"
        return result

    # -- 任务生命周期 ------------------------------------------------------

    async def create_task(
        self, request: CreateTaskRequest, context: TrustedContext
    ) -> tuple[TaskView, TaskSlots]:
        slots = self._slots_from_request(request)
        requirement = request.requirement.strip()
        # 快照二选一：知识库快照与手填快照不能同时提供（不静默覆盖、不拼接）。
        snapshot_knowledge_id = (request.snapshot_knowledge_id or "").strip()
        manual_snapshot = (request.schema_snapshot or "").strip()
        if snapshot_knowledge_id and manual_snapshot:
            raise TaskInputInvalid(
                "结构快照只能二选一：请只提供知识库快照（snapshot_knowledge_id）或手填快照（schema_snapshot）之一"
            )
        # 应用绑定只认 canonical ID：提供时由治理服务核对授权，通过后回填展示名称；
        # 只给名称（历史习惯）不再建立绑定——绝不按名称模糊匹配。
        application_id = (request.application_id or "").strip()
        if application_id:
            verified = await self._verify_application(context, application_id)
            if verified is None:
                raise ApplicationNotAuthorized(
                    "无法确认你对所选应用的授权（未授权或治理服务暂不可达），任务未创建"
                )
            slots.application_id = application_id
            slots.application = verified.get("name") or ""
            authorized_application = application_id
            application_binding = "authorized"
        else:
            # 未提供 canonical ID：不建立应用绑定（名称仅作展示），检索只看组织通用知识。
            authorized_application = ""
            application_binding = "legacy" if (slots.application or "").strip() else "none"
        # 结构快照：选用知识快照时校验（组织、类型、状态、应用匹配、授权）后把正文物化
        # 为任务材料；此后由任务记录持有这份内容，知识库后续失效不影响已生成版本的追溯。
        selected_snapshot: dict[str, Any] | None = None
        snapshot_source_kind = ""
        schema_snapshot = ""
        if snapshot_knowledge_id:
            selected_snapshot, schema_snapshot = await self._select_snapshot(
                snapshot_knowledge_id, context, application_id
            )
            snapshot_source_kind = "knowledge"
        elif manual_snapshot:
            schema_snapshot = request.schema_snapshot or ""
            snapshot_source_kind = "manual"
        slots_payload = slots.model_dump(mode="json")
        events = [event("created", "任务已创建")]
        if selected_snapshot is not None:
            events.append(
                event(
                    "snapshot_selected",
                    f"选用知识库结构快照 {selected_snapshot.get('title') or ''}"
                    f"（版本 {selected_snapshot.get('version') or ''}，"
                    f"knowledge_id={selected_snapshot.get('knowledge_id')}）",
                )
            )
        record: dict[str, Any] = {
            "task_id": f"task_{uuid.uuid4().hex[:12]}",
            "source": self._settings.task_source,
            "created_at": datetime.now(timezone.utc).isoformat(),
            "organization_id": context.organization_id,
            "user_id": context.user_id,
            "requirement": requirement,
            "slots": slots_payload,
            "schema_snapshot": schema_snapshot,
            # 已核对通过的应用（canonical ID）；检索只使用这个值，不回退到请求里声明的名称。
            "authorized_application": authorized_application,
            # 应用绑定状态：authorized / unauthorized / legacy / none。
            "application_binding": application_binding,
            # 明确选用的知识库结构快照（元数据 + 内容摘要；正文物化到 schema_snapshot）。
            "selected_snapshot": selected_snapshot,
            # 当前快照材料来源：knowledge（知识库选用）/ manual（手填）/ ""（无快照）。
            "snapshot_source_kind": snapshot_source_kind,
            "status": TaskStatus.RECEIVED.value,
            "questions": [],
            "draft": None,
            "events": events,
            "revisions": 0,
            "error": None,
            # 输入与材料版本：恢复前必须与记录当前值一致，否则拒绝带着陈旧上下文继续。
            "input_version": input_version(
                requirement=requirement, slots=slots_payload, schema_snapshot=schema_snapshot
            ),
            "awaiting_input": False,
            "resume_count": 0,
            # 实际执行策略：工作台要如实展示，而不是让用户以为永远是固定流程。
            "strategy": self._settings.investigation_strategy,
        }
        self._repository.save(record)
        await self._dispatch_or_fail(record["task_id"])
        return self._view(self._require(record["task_id"])), slots

    async def clarify(self, task_id: str, request: ClarifyRequest, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        self._ensure_active(record)
        status = record.get("status")
        if status not in RESUMABLE_STATUSES:
            raise TaskNotResumable(f"当前状态 {status} 不允许补充信息")

        # 应用授权**每次都重新核对**（历史授权不是长期凭据）。核对是一次外部调用（await），
        # 之后必须基于**最新**记录重新校验并写入，避免等待期间并发的归档 / 关联被旧记录覆盖。
        current_slots = TaskSlots.model_validate(record.get("slots") or {})
        resolved_application = await self._resolve_request_application(context, request, current_slots)
        record = self._reload_for_mutation(task_id)
        self._ensure_active(record)
        if record.get("status") != status:
            raise TaskNotResumable("任务状态已变化，本次补充未生效，请重试")

        slots = TaskSlots.model_validate(record.get("slots") or {})
        updated = merge_slot_data(slots, request.model_dump())
        if request.application_id is not None:
            # 以服务端核对结果为准回填 canonical ID 与展示名称（不信任客户端提交的名称）。
            updated.application_id = resolved_application[0]
            updated.application = resolved_application[1] or updated.application
        record["slots"] = updated.model_dump(mode="json")
        # 只记录本次核对结果；未授权时为空串，检索不会使用应用专属知识。
        record["authorized_application"] = resolved_application[0]
        record["application_binding"] = resolved_application[2]
        # 新增输入要过与入口一致的筛查：检查点让旧节点不必重跑，
        # 但**不能**因为走了检查点就跳过对新输入的校验。
        _screen_new_input(request.note or "", request.schema_snapshot or "")
        # 表结构快照与知识快照选择：冲突显式拒绝，不静默覆盖或拼接（规则见方法注释）。
        await self._apply_clarify_snapshot(record, request, context, resolved_application[0])
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
                "application_id",
                "environment",
                "database",
                "table",
                "query_sql",
                "planned_at",
                "snapshot_knowledge_id",
                "schema_snapshot",
                "note",
            )
            if getattr(request, name, None) is not None
        ]
        events.append(event("clarified", f"补充信息：{', '.join(provided) or '无字段变化'}"))
        # 已选用的知识快照在重新执行前重新校验；失效/被删/内容更新则阻断并提示重新选择。
        self._validate_selected_snapshot(record)
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
            # 从等待点继续同样会**重新执行该节点**（`interrupt()` 之前的代码会再跑一遍），
            # 因此同样属于 at-least-once，必须如实标注而不是假装那次执行没发生过。
            record["recovery_semantics"] = RECOVERY_AT_LEAST_ONCE
            record["resume_count"] = int(record.get("resume_count") or 0) + 1
            events = list(record.get("events") or [])
            events.append(event("recovery_at_least_once", RECOVERY_UNCERTAINTY_NOTE))
            record["events"] = events
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
        self._ensure_active(record)
        status = record.get("status")
        # 终态（取消、输入被拒等）不允许借恢复回到执行路径。
        if status not in RESUMABLE_STATUSES:
            raise TaskNotResumable(f"当前状态 {status} 不允许恢复")
        # 已有执行在跑：直接拒绝，绝不在同一个 thread_id 上再起一次图。
        if self._has_live_execution(task_id):
            raise TaskNotResumable("该任务已有执行在进行中，本次恢复未生效，请等待或先取消")
        current_slots = TaskSlots.model_validate(record.get("slots") or {})
        resolved_application = await self._resolve_request_application(context, request, current_slots)
        if request is not None:
            _screen_new_input(request.note or "", request.schema_snapshot or "")
            slots = TaskSlots.model_validate(record.get("slots") or {})
            updated = merge_slot_data(slots, request.model_dump())
            if request.application_id is not None:
                # 以服务端核对结果为准回填 canonical ID 与展示名称。
                updated.application_id = resolved_application[0]
                updated.application = resolved_application[1] or updated.application
            record["slots"] = updated.model_dump(mode="json")
            # 快照选择/切换规则与补充信息一致：冲突显式拒绝，不静默覆盖或拼接。
            await self._apply_clarify_snapshot(record, request, context, resolved_application[0])
            note = (request.note or "").strip()
            if note:
                notes = list(record.get("clarification_notes") or [])
                notes.append(note)
                record["clarification_notes"] = notes
        authorized_application, application_binding = resolved_application[0], resolved_application[2]
        # 已选用的知识快照在恢复执行前重新校验；失效/被删/内容更新则阻断并提示重新选择。
        self._validate_selected_snapshot(record)
        _apply_input_version(record)

        # 下面读检查点是一个 await：期间可能有另一个请求抢先派发。记下此刻的"代际 + 状态"，
        # 在真正占用执行之前**同步地**再确认一次（理由见下）。
        expected_execution = record.get("execution_id") or ""
        expected_status = record.get("status") or ""

        checkpoint = await self._inspect_checkpoint(task_id)
        if checkpoint is None:
            raise TaskNotResumable("没有可恢复的检查点状态；请重新发起任务")

        if checkpoint["interrupt"]:
            # 停在节点级中断上：把当前完整输入交给 interrupt()。
            mode = RESUME_INTERRUPT
            value: Any = None  # 在重新校验之后基于**最新**记录计算，见下。
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

        # 重新确认 → 落盘 → 占用执行：这一段**不能有 await**。
        # 否则并发的两次恢复都会看到"还没人占"，于是双双派发，在同一个 thread_id 上并发跑图——
        # 检查点不是并发安全的共享状态，那不是"最后写入者胜"，而是互相交错写入。
        latest = self._require(task_id)
        self._ensure_active(latest)
        if (latest.get("execution_id") or "") != expected_execution or (latest.get("status") or "") != expected_status:
            raise TaskNotResumable("任务已被其他操作接管，本次恢复未生效，请重试")
        if self._has_live_execution(task_id):
            raise TaskNotResumable("该任务已有执行在进行中，本次恢复未生效，请等待或先取消")

        # 在**最新**记录上合并写入：`record` 是外部等待之前读到的，直接保存会把这段时间里
        # 并发产生的归档 / 关联 / 审计覆盖回旧值。这里只把本次请求的**输入变更**合并过去，
        # 其余字段以最新记录为准。下面到 save 之间没有 await，单进程内是原子的。
        if request is not None:
            latest["slots"] = record.get("slots") or latest.get("slots")
            if "schema_snapshot" in record:
                latest["schema_snapshot"] = record["schema_snapshot"]
            if "clarification_notes" in record:
                latest["clarification_notes"] = record["clarification_notes"]
            if "selected_snapshot" in record:
                latest["selected_snapshot"] = record["selected_snapshot"]
            if "snapshot_source_kind" in record:
                latest["snapshot_source_kind"] = record["snapshot_source_kind"]
        # 合并后的输入若与最新记录不同，旧的草案/检查结果同样要作废。
        _apply_input_version(latest)
        if mode == RESUME_INTERRUPT:
            value = _resume_value(latest)
        latest["authorized_application"] = authorized_application
        latest["application_binding"] = application_binding
        latest["awaiting_input"] = False
        latest["status"] = TaskStatus.RUNNING.value
        latest["error"] = None
        latest["resume_mode"] = mode
        # 不宣称 exactly-once：`interrupt` 与 `checkpoint` 两种模式都会**重新执行**节点，
        # 而 `checkpoint` 模式的那个节点在中断前可能已经开始、甚至已经把模型请求发出去了。
        # 与其假装那次调用不存在，不如把不确定性写进记录（消耗以账本为准）。
        latest["recovery_semantics"] = RECOVERY_AT_LEAST_ONCE
        latest["resume_count"] = int(latest.get("resume_count") or 0) + 1
        events = list(latest.get("events") or [])
        events.append(event("resumed", f"从检查点恢复（mode={mode}）"))
        events.append(event("recovery_at_least_once", RECOVERY_UNCERTAINTY_NOTE))
        latest["events"] = events
        self._repository.save(latest)
        await self._dispatch_or_fail(task_id, resume={"mode": mode, "value": value})
        return self._view(self._require(task_id))

    async def confirm(
        self, task_id: str, context: TrustedContext, request: ConfirmRequest | None = None
    ) -> TaskView:
        """记录一次**人工材料确认**。

        三条边界：

        - 只有创建者能确认，且必须**已有材料**（草案）时才能确认；
        - 同一人 + 同一材料版本 + 同一内容哈希的重复确认是**幂等**的，不新增记录、不重复写事件；
        - 材料重新生成（内容哈希变化）或输入/材料版本变化时，旧确认**失效**并保留痕跡。

        它**不是**治理审批，也**不是**执行许可：确认不改变任务状态，也不授予任何执行权利；
        审批与通行证签发仍只由 Go 治理服务负责。
        """
        record = self._authorize(task_id, context)
        if record.get("archived_at") or record.get("deleted_at"):
            raise TaskNotConfirmable("请先恢复任务，再确认材料")
        status = record.get("status")
        if status not in {TaskStatus.DRAFT_READY.value, TaskStatus.CHECK_BLOCKED.value}:
            raise TaskNotConfirmable(f"当前状态 {status} 没有可确认的材料")
        draft = record.get("draft") or {}
        digest = material_hash(str(draft.get("sql") or ""), str(draft.get("rollback_sql") or ""))
        if not draft or not digest:
            raise TaskNotConfirmable("没有可确认的材料：草案为空")
        version = str(record.get("input_version") or "")

        # 所见即所确认：调用方声明的材料哈希必须与当前材料一致，否则要求刷新。
        claimed = (request.material_hash if request else None) or ""
        if not claimed.strip() or claimed.strip() != digest:
            raise TaskNotConfirmable("材料已更新，当前页面看到的内容不是最新版本；请刷新后重新确认")

        # 幂等：同一人 + 同一版本 + 同一内容已经确认过，就直接返回，不新增记录。
        for item in record.get("confirmations") or []:
            if (
                not item.get("invalidated_at")
                and item.get("confirmed_by") == context.user_id
                and item.get("material_version") == version
                and item.get("material_hash") == digest
            ):
                return self._view(record)

        _invalidate_stale_confirmations(record, "材料版本或内容已变化，旧确认失效")
        confirmations = list(record.get("confirmations") or [])
        confirmations.append(
            {
                "confirmation_id": f"confirm_{uuid.uuid4().hex[:12]}",
                "confirmed_by": context.user_id,
                "confirmed_organization": context.organization_id,
                "confirmed_at": datetime.now(timezone.utc).isoformat(),
                "material_version": version,
                "material_hash": digest,
                "note": ((request.note if request else None) or "").strip(),
                "invalidated_at": None,
                "invalidate_reason": None,
            }
        )
        record["confirmations"] = confirmations
        events = list(record.get("events") or [])
        events.append(event("confirmed", "材料已由创建者人工确认（不构成治理审批，也不代表可在生产执行）"))
        record["events"] = events
        self._repository.save(record)
        return self._view(record)

    async def get_task(self, task_id: str, context: TrustedContext) -> TaskView:
        return self._view(self._authorize(task_id, context))

    async def list_tasks(
        self, context: TrustedContext, *, q: str = "", status: str | None = None,
        source: str = "default", workspace: str = "active",
    ) -> list[TaskView]:
        """只列出调用方**自己**创建的任务。

        组织与创建者都要匹配；可信上下文不完整时返回空列表而不是全部任务。
        """
        if not context.organization_id or not context.user_id:
            return []
        if source not in {"default", "all", "production", "evaluation", "demo", "legacy"}:
            raise ValueError("未知任务来源")
        if workspace not in {"active", "archived", "trash", "all"}:
            raise ValueError("未知任务工作区")
        if status is not None:
            TaskStatus(status)
        query = q.strip().casefold()
        if len(query) > 200:
            raise ValueError("搜索最多 200 字")

        def matches(item: dict[str, Any]) -> bool:
            origin = item.get("source") or "legacy"
            if source == "default" and origin not in {"production", "legacy"}:
                return False
            if source not in {"default", "all"} and origin != source:
                return False
            scope = "trash" if item.get("deleted_at") else "archived" if item.get("archived_at") else "active"
            if workspace != "all" and workspace != scope:
                return False
            if status and item.get("status") != status:
                return False
            fields = (item.get("task_id"), item.get("requirement"), (item.get("slots") or {}).get("application"))
            return not query or any(query in str(value or "").casefold() for value in fields)

        return [
            self._view(item)
            for item in self._repository.list()
            if (item.get("organization_id") or "").strip() == context.organization_id
            and (item.get("user_id") or "").strip() == context.user_id
            and matches(item)
        ]

    @staticmethod
    def _ensure_active(record: dict[str, Any]) -> None:
        if record.get("archived_at") or record.get("deleted_at"):
            raise TaskNotResumable("任务已归档或在回收站，请先恢复")

    def _ensure_idle(self, record: dict[str, Any]) -> None:
        if record.get("status") in INTERRUPTED_STATUSES or self._has_live_execution(record["task_id"]):
            raise TaskNotResumable("任务仍在执行，请先取消或等待结束")

    def _save_management(self, record: dict[str, Any], kind: str, context: TrustedContext) -> TaskView:
        record["updated_at"] = datetime.now(timezone.utc).isoformat()
        entry = event(kind, f"创建者 {context.user_id} 执行任务管理操作；不改变审批与执行权限")
        # 检查点可能携带旧 events；管理审计独立留存，恢复图后再合并，不能被旧快照抹掉。
        record["management_events"] = [*(record.get("management_events") or []), entry]
        record["events"] = [*(record.get("events") or []), entry]
        try:
            self._repository.save(record)
        except OSError as error:
            raise TaskStateUnavailable("任务管理操作未落盘，请重试") from error
        return self._view(record)

    async def archive_task(self, task_id: str, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        self._ensure_idle(record)
        if record.get("deleted_at"):
            raise TaskNotResumable("任务在回收站，请先恢复")
        if record.get("archived_at"):
            return self._view(record)
        record["archived_at"] = datetime.now(timezone.utc).isoformat()
        return self._save_management(record, "archived", context)

    async def restore_task(self, task_id: str, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        self._ensure_idle(record)
        if record.get("deleted_at"):
            record["deleted_at"] = None
            # 回收站恢复到归档区，不能暗中恢复模型执行。
            record["archived_at"] = record.get("archived_at") or datetime.now(timezone.utc).isoformat()
        elif record.get("archived_at"):
            record["archived_at"] = None
        else:
            return self._view(record)
        return self._save_management(record, "restored", context)

    async def delete_preview(self, task_id: str, context: TrustedContext) -> DeleteTaskPreview:
        record = self._authorize(task_id, context)
        return self._deletion_preview(record)

    def _deletion_preview(self, record: dict[str, Any]) -> DeleteTaskPreview:
        blockers = []
        if record.get("deleted_at"):
            blockers.append("任务已在回收站")
        if not record.get("archived_at"):
            blockers.append("请先归档任务")
        if record.get("status") not in {"FAILED", "CANCELLED", "INPUT_REJECTED"} or self._has_live_execution(record["task_id"]):
            blockers.append("仅允许清理已停止的失败、取消或输入拒绝任务")
        # 现有系统尚无权威跨服务关联索引：有过材料/确认的记录保守地只允许归档。
        if record.get("draft") or record.get("confirmations") or record.get("change_links") or any(
            item.get("kind") in {"generate_draft", "finalize", "confirmed", "change_linked"}
            for item in record.get("events") or []
        ) or any(
            record.get(key) for key in ("change_id", "change_request_id", "linked_change_id", "submitted_change_id")
        ):
            blockers.append("包含草案、确认或正式变更关联，保留记录，仅支持归档")
        version = hashlib.sha256(json.dumps(record, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()
        return DeleteTaskPreview(task_id=record["task_id"], allowed=not blockers, blockers=blockers, record_version=version)

    async def delete_task(self, task_id: str, context: TrustedContext, record_version: str) -> TaskView:
        record = self._authorize(task_id, context)
        preview = self._deletion_preview(record)
        if not preview.allowed:
            raise TaskNotResumable("；".join(preview.blockers))
        if preview.record_version != record_version:
            raise TaskNotResumable("任务已变化，请重新预览后确认")
        # 校验与原子落盘之间没有 await；单实例事件循环不能插入另一生命周期操作。
        record["deleted_at"] = datetime.now(timezone.utc).isoformat()
        return self._save_management(record, "moved_to_trash", context)

    # -- M2：执行轨迹与草案版本 -------------------------------------------

    async def task_trace(self, task_id: str, context: TrustedContext) -> TaskTrace:
        """执行轨迹投影。

        只展示**已落盘的**事件、工具观察、模型调用账本与检查结论：步骤、状态、
        实测耗时、引用、token、费用与失败原因。未记录的一律标为未知（不填 0），
        且不含模型内部思维链。
        """
        return build_trace(self._authorize(task_id, context))

    async def draft_versions(self, task_id: str, context: TrustedContext) -> list[DraftVersion]:
        record = self._authorize(task_id, context)
        return [DraftVersion.model_validate(item) for item in record.get("draft_versions") or []]

    async def draft_version_diff(
        self, task_id: str, context: TrustedContext, from_version: int | None, to_version: int | None
    ) -> DraftVersionDiff:
        record = self._authorize(task_id, context)
        versions = [DraftVersion.model_validate(item) for item in record.get("draft_versions") or []]
        if not versions:
            raise TaskNotResumable("该任务还没有草案版本快照")
        target_version = int(to_version) if to_version is not None else versions[-1].version
        target = next((item for item in versions if item.version == target_version), None)
        if target is None:
            raise TaskNotResumable(f"草案版本 v{target_version} 不存在")
        if from_version is None:
            # 默认对照上一版；没有上一版时与空内容对照（展示整份材料的引入）。
            previous = next((item for item in reversed(versions) if item.version < target_version), None)
        else:
            previous = next((item for item in versions if item.version == int(from_version)), None)
            if previous is None:
                raise TaskNotResumable(f"草案版本 v{from_version} 不存在")
        return DraftVersionDiff(
            task_id=task_id,
            from_version=previous.version if previous else None,
            to_version=target.version,
            sql_diff=_unified_diff(previous.sql if previous else "", target.sql, "sql"),
            rollback_diff=_unified_diff(previous.rollback_sql if previous else "", target.rollback_sql, "rollback"),
        )

    async def edit_draft(self, task_id: str, context: TrustedContext, request: DraftEditRequest) -> TaskView:
        """服务端版本化编辑草案。

        边界（失败关闭）：

        - 归属（组织 + 创建者）由 `_authorize` 校验；归档 / 回收站任务只读；
        - 只有 `DRAFT_READY` / `CHECK_BLOCKED` 且没有在途执行时可编辑；
        - `expected_version` 必须与当前草案版本一致，否则拒绝（陈旧页面 / 并发覆盖）；
        - 编辑后**重新执行确定性检查**，新内容绝不沿用旧检查结果；检查工具失败即失败关闭
          （状态落到 `CHECK_BLOCKED`，不会变成 `DRAFT_READY`）；
        - 内容变化使旧人工确认**失效**，但审计、旧确认与旧版本快照全部保留。
        """
        record = self._authorize(task_id, context)
        self._ensure_active(record)
        if record.get("status") not in {TaskStatus.DRAFT_READY.value, TaskStatus.CHECK_BLOCKED.value}:
            raise TaskNotResumable(f"当前状态 {record.get('status')} 不允许编辑材料")
        if self._has_live_execution(task_id):
            raise TaskNotResumable("任务仍有执行在进行中，请先取消或等待结束")
        draft = record.get("draft") or {}
        if not draft:
            raise TaskNotResumable("当前任务没有可编辑的草案")
        current_version = int(draft.get("version") or 1)
        if int(request.expected_version) != current_version:
            raise TaskNotResumable(
                f"草案已被更新（当前 v{current_version}），您看到的是旧版本；请刷新后重试"
            )
        # 自由文本说明按新输入筛查；SQL 是材料，不作为提示词注入处理。
        _screen_new_input(request.reason or "")

        check = await self._scan_material(record, request.sql, request.rollback_sql)

        # 确定性扫描是一次 await：期间可能发生并发操作（归档、关联、另一次编辑）。
        # 必须基于**最新**记录重新校验生命周期与版本，并把结果合并写回这份最新记录；
        # 写回等待之前读到的那份会把并发产生的归档/关联/审计覆盖掉。
        latest = self._reload_for_mutation(task_id)
        self._ensure_active(latest)
        if self._has_live_execution(task_id):
            raise TaskNotResumable("任务已有新的执行在进行中，本次编辑未生效；请等待或先取消")
        latest_draft = latest.get("draft") or {}
        if int(latest_draft.get("version") or 1) != current_version:
            raise TaskNotResumable("草案已被其他操作更新，本次编辑未生效；请刷新后重试")
        if latest.get("status") not in {TaskStatus.DRAFT_READY.value, TaskStatus.CHECK_BLOCKED.value}:
            raise TaskNotResumable("任务状态已变化，本次编辑未生效；请刷新后重试")

        new_version = current_version + 1
        new_draft = dict(latest_draft)
        new_draft.update(
            {
                "version": new_version,
                "sql": request.sql,
                "rollback_sql": request.rollback_sql,
                "deterministic_check": check.model_dump(mode="json"),
                "revision_notes": [*(latest_draft.get("revision_notes") or []), f"人工编辑 v{new_version}"],
            }
        )
        # 以下到 save 之间没有 await，单进程事件循环内是原子的。
        latest["draft"] = new_draft
        # 检查未通过（含扫描工具失败）时失败关闭：状态是 CHECK_BLOCKED，不是 DRAFT_READY。
        latest["status"] = (
            TaskStatus.CHECK_BLOCKED.value if check.status in {"BLOCKED", "FAILED"} else TaskStatus.DRAFT_READY.value
        )
        latest["updated_at"] = datetime.now(timezone.utc).isoformat()
        # 材料内容已变：旧确认失效（保留痕跡）；新检查结果是针对**新内容**计算的。
        _invalidate_stale_confirmations(latest, "材料已被人工编辑，旧确认失效")
        _record_draft_version(
            latest, origin="user_edit", actor=context.user_id, reason=request.reason or "", draft=new_draft
        )
        events = list(latest.get("events") or [])
        events.append(
            event("draft_edited", f"创建者 {context.user_id} 服务端编辑材料至 v{new_version}；检查 {check.status}")
        )
        latest["events"] = events
        try:
            self._repository.save(latest)
        except OSError as error:
            # 失败关闭且状态不变：内存不得领先磁盘（TaskRepository 先落盘后提交内存）。
            raise TaskStateUnavailable("材料编辑未落盘，请重试") from error
        return self._view(latest)

    async def link_change(self, task_id: str, context: TrustedContext, request: LinkChangeRequest) -> TaskView:
        """把本任务关联到一个**已存在**的正式变更单（人工动作，服务端校验）。

        三条边界：

        - 归档 / 回收站任务只读：不允许借本接口绕过生命周期限制；
        - 关联前必须通过治理后端只读接口确认该变更存在且属于本组织，**失败关闭**；
        - 同一 `change_request_id`（或同一幂等键）重复提交是幂等的，不新增关联、不重复写事件。

        关联**不是**授权凭据：它不代表变更单已获批，也不授予任何生产执行权利。
        """
        record = self._authorize(task_id, context)
        self._ensure_active(record)
        if self._has_live_execution(task_id):
            raise TaskNotResumable("任务仍在执行，请稍后再关联正式变更单")
        change_id = (request.change_request_id or "").strip()
        if not change_id:
            raise TaskNotResumable("change_request_id 不能为空")
        idempotency_key = (request.idempotency_key or change_id).strip()

        def duplicate(item: dict[str, Any]) -> bool:
            return item.get("change_request_id") == change_id or (
                bool(item.get("idempotency_key")) and item.get("idempotency_key") == idempotency_key
            )

        # 先做一次廉价去重，避免为一次必然幂等的请求多打一次治理后端。
        if any(duplicate(item) for item in record.get("change_links") or []):
            return self._view(record)
        if not await self._change_exists(change_id, context):
            raise TaskNotResumable("无法确认该正式变更单存在且属于本组织，未建立关联")

        # 核对是一次 await：期间可能被归档 / 被其它操作写入。基于**最新**记录重新校验并合并写入，
        # 否则会把并发产生的归档、其它关联与审计覆盖掉。
        latest = self._reload_for_mutation(task_id)
        self._ensure_active(latest)
        if self._has_live_execution(task_id):
            raise TaskNotResumable("任务已有新的执行在进行中，本次关联未生效")
        links = list(latest.get("change_links") or [])
        if any(duplicate(item) for item in links):
            return self._view(latest)
        links.append(
            {
                "change_request_id": change_id,
                "organization_id": context.organization_id,
                "linked_by": context.user_id,
                "linked_at": datetime.now(timezone.utc).isoformat(),
                "origin": "agent_task",
                "idempotency_key": idempotency_key,
            }
        )
        # 以下到 save 之间没有 await，单进程事件循环内是原子的。
        latest["change_links"] = links
        latest["updated_at"] = datetime.now(timezone.utc).isoformat()
        events = list(latest.get("events") or [])
        events.append(event("change_linked", f"创建者 {context.user_id} 关联正式变更单 {change_id}（关联不代表批准）"))
        latest["events"] = events
        try:
            self._repository.save(latest)
        except OSError as error:
            raise TaskStateUnavailable("变更关联未落盘，请重试") from error
        return self._view(latest)

    async def _scan_material(self, record: dict[str, Any], sql: str, rollback_sql: str) -> DeterministicCheck:
        """对给定材料执行确定性扫描。工具失败按 FAILED 返回（失败关闭，绝不当作通过）。"""
        registry = Toolbox(
            settings=self._settings,
            retriever=self._retriever,
            schema_snapshot=record.get("schema_snapshot") or "",
        ).build()
        result = await registry.call(
            "scan_sql",
            {"sql": sql, "rollback_sql": rollback_sql},
            TrustedContext(
                user_id=record.get("user_id") or "",
                organization_id=record.get("organization_id") or "",
            ),
        )
        if not result.ok:
            return DeterministicCheck(status="FAILED", source="scan_sql", error=result.error or "确定性扫描未执行")
        return DeterministicCheck.model_validate(result.data)

    async def _change_exists(self, change_id: str, context: TrustedContext) -> bool:
        """通过治理后端内部只读接口确认变更存在且属于本组织。缺密钥即失败关闭。"""
        token = self._settings.upstream_token.strip()
        if not token:
            return False
        url = f"{self._settings.governance_base_url}/api/agent-tools/changes/{change_id}"
        headers = dict(context.as_headers())
        headers["X-Agent-Upstream-Token"] = token
        try:
            async with httpx.AsyncClient(timeout=self._settings.governance_timeout_seconds) as client:
                response = await client.get(url, params={"projection": "context"}, headers=headers)
        except Exception:  # noqa: BLE001 - 治理后端不可达时失败关闭，不建立未经验证的关联
            return False
        return response.status_code == 200

    async def _application_authorized(self, context: TrustedContext, application_id: str) -> bool:
        """核验调用方对某个应用是否真的获得授权（治理后端，失败关闭）。

        **请求参数不是权限依据**：应用 ID 由调用方给出，只能作为"要访问谁"的声明；
        是否允许由治理服务按成员与应用授权判定。留空（组织通用知识）不需要应用授权。
        无法核对（缺密钥、后端不可达、非 200）一律按**未授权**处理。
        """
        application = (application_id or "").strip()
        if not application:
            return True
        token = self._settings.upstream_token.strip()
        if not token:
            return False
        url = f"{self._settings.governance_base_url}/api/agent-tools/applications/{application}"
        headers = dict(context.as_headers())
        headers["X-Agent-Upstream-Token"] = token
        try:
            async with httpx.AsyncClient(timeout=self._settings.governance_timeout_seconds) as client:
                response = await client.get(url, headers=headers)
        except Exception:  # noqa: BLE001 - 核验不了就当作没有授权
            return False
        return response.status_code == 200

    async def _authorized_application(self, context: TrustedContext, application_id: str) -> str:
        """返回**已核对通过**的应用 ID；未授权或无法核对时返回空串（失败关闭）。"""
        application = (application_id or "").strip()
        if not application:
            return ""
        return application if await self._application_authorized(context, application) else ""


    async def _verify_application(self, context: TrustedContext, application_id: str) -> dict[str, Any] | None:
        """向治理服务核对应用授权，通过时返回 {"id","name"}；否则 None（失败关闭）。

        应用 ID 由调用方给出，只是"要访问谁"的声明；是否允许由治理服务按成员与
        应用授权判定。展示名称也以治理服务返回为准，不信任客户端提交的名称。
        """
        application = (application_id or "").strip()
        token = self._settings.upstream_token.strip()
        if not application or not token:
            return None
        url = f"{self._settings.governance_base_url}/api/agent-tools/applications/{application}"
        headers = dict(context.as_headers())
        headers["X-Agent-Upstream-Token"] = token
        try:
            async with httpx.AsyncClient(timeout=self._settings.governance_timeout_seconds) as client:
                response = await client.get(url, headers=headers)
        except Exception:  # noqa: BLE001 - 核验不了就当作没有授权
            return None
        if response.status_code != 200:
            return None
        try:
            payload = response.json()
        except ValueError:
            return None
        return {"id": application, "name": str(payload.get("name") or "")}

    async def _resolve_request_application(
        self, context: TrustedContext, request: ClarifyRequest | None, current: TaskSlots
    ) -> tuple[str, str, str]:
        """解析补充/恢复请求带来的应用绑定变更，返回 (authorized_application, name, binding)。

        - 请求显式提供 application_id：核对授权，失败抛 `ApplicationNotAuthorized`（不落盘）；
        - 未提供：按任务当前绑定的 canonical ID 重新核对——撤权或不可达同样阻断，
          绝不带着上一次的授权结果继续；
        - 任务从未绑定 canonical ID：legacy（仅名称）/ none，不做应用授权，
          检索只看组织通用知识。
        """
        requested = request.application_id if request is not None else None
        if requested is not None:
            requested = requested.strip()
            if not requested:
                raise TaskInputInvalid(
                    "application_id 不能为空串：要更换应用请提供有效应用 ID，或省略该字段保持不变"
                )
            verified = await self._verify_application(context, requested)
            if verified is None:
                raise ApplicationNotAuthorized(
                    "无法确认你对所选应用的授权（未授权或治理服务暂不可达），本次操作未生效"
                )
            return requested, verified.get("name") or "", "authorized"
        binding = (current.application_id or "").strip()
        if not binding:
            return "", "", ("legacy" if (current.application or "").strip() else "none")
        verified = await self._verify_application(context, binding)
        if verified is None:
            raise ApplicationNotAuthorized(
                "当前绑定应用的授权核对失败（可能已被撤权或治理服务暂不可达）。"
                "请重新选择你有权限的应用，或稍后重试"
            )
        return binding, verified.get("name") or "", "authorized"


    def _reload_for_mutation(self, task_id: str) -> dict[str, Any]:
        """在等待外部 I/O 之后重新读取记录，用于"基于最新状态合并写入"。

        落盘一律写回这份最新记录（而不是等待之前读到的那份），否则并发产生的归档、
        关联、审计等字段会被旧值覆盖。
        """
        record = self._repository.get(task_id)
        if record is None:
            raise TaskNotFound(task_id)
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
            application_id=request.application_id or "",
            application=request.application,
            environment=request.environment,
            database=request.database,
            table=request.table,
            query_sql=request.query_sql,
            planned_at=request.planned_at,
            planned_at_timezone=request.planned_at_timezone,
        )

    def _view(self, record: dict[str, Any]) -> TaskView:
        slots_model = TaskSlots.model_validate(record.get("slots") or {})
        binding = str(record.get("application_binding") or "")
        if not binding:
            # 旧记录没有显式绑定状态：按已核对应用与槽位回退推导（只影响展示，不改存储）。
            authorized = str(record.get("authorized_application") or "")
            if authorized and authorized == slots_model.application_id:
                binding = "authorized"
            elif slots_model.application_id:
                binding = "unauthorized"
            elif (slots_model.application or "").strip():
                binding = "legacy"
            else:
                binding = "none"
        return TaskView.model_validate(
            {
                "task_id": record["task_id"],
                "source": record.get("source") or "legacy",
                "created_at": record.get("created_at"),
                "updated_at": (record.get("events") or [{}])[-1].get("at") or record.get("updated_at"),
                "archived_at": record.get("archived_at"),
                "deleted_at": record.get("deleted_at"),
                "status": record.get("status", TaskStatus.FAILED.value),
                "requirement": record.get("requirement", ""),
                "slots": record.get("slots") or {},
                "questions": record.get("questions") or [],
                "draft": record.get("draft"),
                "events": record.get("events") or [],
                "revisions": int(record.get("revisions") or 0),
                "error": record.get("error"),
                "planned_at_missing": "planned_at" in slots_model.missing(),
                "awaiting_input": bool(record.get("awaiting_input")),
                "resume_mode": record.get("resume_mode"),
                # 恢复的执行语义：外部模型请求不保证 exactly-once，恢复过就是 at-least-once。
                "recovery_semantics": record.get("recovery_semantics"),
                "restart_policy": record.get("restart_policy"),
                "material_hash": _current_material_hash(record) or None,
                "confirmations": record.get("confirmations") or [],
                "draft_version_count": len(record.get("draft_versions") or []),
                "change_links": record.get("change_links") or [],
                "strategy": record.get("strategy") or self._settings.investigation_strategy,
                "investigation": record.get("investigation"),
                "usage": record.get("usage"),
                # 应用绑定与身份：canonical ID、绑定状态、选用的知识快照溯源。
                "application_binding": binding,
                "authorized_application": record.get("authorized_application") or "",
                "selected_snapshot": record.get("selected_snapshot"),
                "snapshot_source_kind": str(record.get("snapshot_source_kind") or ""),
            }
        )

__all__ = ["AgentService", "TaskNotFound", "TaskNotResumable", "TaskNotConfirmable", "DatabaseKind"]
