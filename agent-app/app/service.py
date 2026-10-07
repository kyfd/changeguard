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

# 恢复的执行语义。**我们不宣称 exactly-once**：
#
# LangGraph 的节点级检查点保证的是"从哪个节点继续"，不是"那个节点没有执行过"。恢复一个
# 中断的节点，等于**重新执行该节点**——`interrupt()` 之前的代码会再跑一遍，而如果进程是在
# 节点执行到一半时消失的，那个节点根本没有完成过。两种情况都可能是 at-least-once。
#
# 外部模型请求尤其如此：请求可能在崩溃前就已经发出去了（账本会记下它），我们无法撤回，
# 也不假装它没发生过。因此恢复时必须把这一不确定性**显式**写进记录与事件，而不是
# 让"已从检查点恢复"读起来像"那次调用不存在"。
RECOVERY_AT_LEAST_ONCE = "at_least_once"
RECOVERY_UNCERTAINTY_NOTE = (
    "恢复不宣称 exactly-once：被中断的节点可能已经开始执行，外部模型请求可能已经发出；"
    "重新执行该节点属于 at-least-once。已发生的消耗以调用账本（usage）为准，不当作没有发生过。"
)


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


class TaskNotConfirmable(Exception):
    """当前没有可人工确认的材料（例如尚未生成草案）。"""


class KnowledgeNotFound(Exception):
    """知识不存在或不属于调用方组织（不区分，避免探测）。"""


class KnowledgeInvalid(Exception):
    """知识导入参数不合法。"""


class KnowledgeStateUnavailable(Exception):
    """知识无法落盘：失败关闭，不返回未持久化的成功。"""


class KnowledgeForbidden(Exception):
    """调用方对该应用没有授权（或无法核对授权）。"""


class ApplicationNotAuthorized(Exception):
    """调用方对所声明的应用没有授权（或无法核对授权）。

    应用 ID 只是"要访问谁"的声明；是否允许由治理服务按成员与应用授权判定，
    无法核对时失败关闭。HTTP 层映射为 403。
    """


class TaskInputInvalid(Exception):
    """任务输入的业务校验失败（例如快照二选一冲突、所选快照已失效）。

    请求格式合法但业务规则不满足，错误消息必须给出可执行的下一步提示。
    HTTP 层映射为 422。
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

    async def _select_snapshot(
        self, knowledge_id: str, context: TrustedContext, task_application_id: str
    ) -> tuple[dict[str, Any], str]:
        """校验并解析一份可供任务选用的知识库结构快照，返回 (元数据, 正文)。

        校验（失败关闭，逐项给出可执行提示）：
        - 存在且属于调用方组织（不存在与跨组织同样按"不存在"处理，不提供探测）；
        - kind 必须是 schema、status 必须是生效中；
        - 快照绑定应用时，任务必须绑定同一应用（先选应用再选快照）；
        - 快照的应用授权在任务绑定环节已通过治理服务核对（同一 ID，不重复请求）。
        正文按不可信数据处理：只进入生成输入，不改变工具权限或治理规则。
        """
        record = self._require_knowledge(knowledge_id, context)
        if str(record.get("kind") or "") != "schema":
            raise TaskInputInvalid("所选知识不是结构快照（kind=schema），请从结构快照中选择")
        if str(record.get("status") or "") != "active":
            raise TaskInputInvalid("所选结构快照已失效，请选择生效中的快照或改用手填快照")
        knowledge_application = str(record.get("application_id") or "")
        task_application = (task_application_id or "").strip()
        if knowledge_application and knowledge_application != task_application:
            if not task_application:
                raise TaskInputInvalid("该结构快照属于特定应用：请先为任务选择应用，再选用快照")
            raise TaskInputInvalid("所选结构快照属于其他应用，请选择当前应用下的快照")
        return (
            {
                "knowledge_id": str(record.get("knowledge_id") or ""),
                "title": str(record.get("title") or ""),
                "version": str(record.get("version") or ""),
                "content_hash": str(record.get("content_hash") or ""),
                "application_id": knowledge_application,
                "selected_at": datetime.now(timezone.utc).isoformat(),
                "selected_by": context.user_id,
            },
            str(record.get("body") or ""),
        )

    def _validate_selected_snapshot(self, record: dict[str, Any]) -> None:
        """再次执行前复核已选用的知识快照仍然有效（存在、生效、内容未变）。

        快照失效不是"尽力继续"的理由：生成输入将包含这份结构，失效快照会让生成
        建立在过期事实上。失败时给出可执行的下一步（重新选择或改手填）。
        """
        meta = record.get("selected_snapshot")
        if not meta:
            return
        knowledge_id = str(meta.get("knowledge_id") or "")
        body = self._knowledge.get(knowledge_id)
        if (
            body is None
            or str(body.get("status") or "") != "active"
            or str(body.get("content_hash") or "") != str(meta.get("content_hash") or "")
        ):
            raise TaskInputInvalid(
                f"所选知识库结构快照已失效或内容已更新（knowledge_id={knowledge_id}）。"
                "请重新选择生效中的快照，或改用手填快照后再继续"
            )

    async def _apply_clarify_snapshot(
        self,
        record: dict[str, Any],
        request: ClarifyRequest,
        context: TrustedContext,
        application_id: str,
    ) -> None:
        """补充/恢复阶段的快照生命周期：切换、清除或替换知识快照与手填快照。

        冲突一律显式拒绝，绝不静默覆盖或拼接：
        - `snapshot_knowledge_id` 非空：切换为知识快照。已有手填快照时必须同时把
          schema_snapshot 显式置空，表示"确认替换"；
        - `snapshot_knowledge_id` 为空串：清除选用；正文来自知识快照时一并清空；
        - `schema_snapshot` 非空：覆盖手填正文；当前正文来自知识快照时先清除选用；
        - `schema_snapshot` 空串：清空手填正文。
        """
        snapshot_knowledge_id = (request.snapshot_knowledge_id or "").strip()
        schema_snapshot = request.schema_snapshot
        current_kind = str(record.get("snapshot_source_kind") or "")
        events = list(record.get("events") or [])

        if snapshot_knowledge_id:
            if schema_snapshot is not None and schema_snapshot.strip():
                raise TaskInputInvalid(
                    "结构快照只能二选一：不能同时提供 snapshot_knowledge_id 和 schema_snapshot"
                )
            if (
                current_kind == "manual"
                and str(record.get("schema_snapshot") or "").strip()
                and schema_snapshot is None
            ):
                raise TaskInputInvalid(
                    "当前已有一份手填快照：如确认改用知识库快照，请在提交 snapshot_knowledge_id 的同时"
                    "把 schema_snapshot 显式置空（表示确认替换）；或继续使用手填快照"
                )
            meta, body = await self._select_snapshot(snapshot_knowledge_id, context, application_id)
            record["selected_snapshot"] = meta
            record["snapshot_source_kind"] = "knowledge"
            record["schema_snapshot"] = body
            events.append(
                event(
                    "snapshot_selected",
                    f"选用知识库结构快照 {meta.get('title') or ''}"
                    f"（版本 {meta.get('version') or ''}，knowledge_id={meta.get('knowledge_id')}）",
                )
            )
            record["events"] = events
            return

        if request.snapshot_knowledge_id is not None:
            # 空串：显式清除选用；不能再同时提供手填正文（分两步，避免歧义）。
            if schema_snapshot is not None and schema_snapshot.strip():
                raise TaskInputInvalid(
                    "已请求清除知识快照选用，不能同时提供手填 schema_snapshot；请分两步提交"
                )
            if current_kind == "knowledge":
                record["schema_snapshot"] = ""
                record["snapshot_source_kind"] = ""
            record["selected_snapshot"] = None
            events.append(event("snapshot_cleared", "已清除知识库快照选用"))
            record["events"] = events
            return

        if schema_snapshot is None:
            return
        if current_kind == "knowledge":
            raise TaskInputInvalid(
                "当前正文来自知识库快照：请先清除选用（snapshot_knowledge_id 提交空串），再提交手填快照"
            )
        record["schema_snapshot"] = schema_snapshot
        record["snapshot_source_kind"] = "manual" if schema_snapshot.strip() else ""
        if not schema_snapshot.strip():
            record["selected_snapshot"] = None
        record["events"] = events

    def _reload_for_mutation(self, task_id: str) -> dict[str, Any]:
        """在等待外部 I/O 之后重新读取记录，用于"基于最新状态合并写入"。

        落盘一律写回这份最新记录（而不是等待之前读到的那份），否则并发产生的归档、
        关联、审计等字段会被旧值覆盖。
        """
        record = self._repository.get(task_id)
        if record is None:
            raise TaskNotFound(task_id)
        return record

    # -- M3：项目知识 ------------------------------------------------------

    async def import_knowledge(self, request: KnowledgeImportRequest, context: TrustedContext) -> KnowledgeView:
        """导入一份项目知识（规范 / 历史案例 / 结构快照）。

        组织来自可信上下文；文档内容按**不可信数据**处理——只做提示注入筛查并如实
        记录，既不据此提升权限，也不据此放行或拒绝一份可能正当的安全文档。
        """
        if not context.organization_id or not context.user_id:
            raise KnowledgeInvalid("缺少可信身份，拒绝导入")
        title = request.title.strip()
        body = request.body
        if not title:
            raise KnowledgeInvalid("标题不能为空")
        if not body.strip():
            raise KnowledgeInvalid("正文不能为空")
        # 应用 ID 由调用方声明，必须由治理后端核对授权；不能把声明当权限。
        application_id = request.application_id.strip()
        if application_id and not await self._application_authorized(context, application_id):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对），不能导入应用专属知识")
        hits = sorted(set(detect_injection(f"{title}\n{body}")))
        record = import_record(
            knowledge_id=f"kb_{uuid.uuid4().hex[:12]}",
            organization_id=context.organization_id,
            imported_by=context.user_id,
            kind=request.kind,
            title=title,
            body=body,
            version=request.version.strip(),
            source=request.source.strip(),
            application_id=application_id,
            status=request.status,
            injection_hits=hits,
        )
        try:
            self._knowledge.save(record)
        except OSError as error:
            raise KnowledgeStateUnavailable("知识未落盘，请重试") from error
        return self._knowledge_view(record)

    async def list_knowledge(
        self,
        context: TrustedContext,
        *,
        kind: str | None = None,
        status: str | None = None,
        application_id: str | None = None,
    ) -> list[KnowledgeView]:
        """列出**本组织**导入的知识。其他组织的数据完全不出现。

        指定 `application_id` 时必须先由治理后端核对授权：筛选参数只是请求条件，
        不是权限依据。未授权（或无法核对）直接拒绝，而不是返回该应用的知识。
        """
        if not context.organization_id:
            return []
        if application_id and not await self._application_authorized(context, application_id):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对）")
        result: list[KnowledgeView] = []
        for record in self._knowledge.list():
            if str(record.get("organization_id") or "") != context.organization_id:
                continue
            record_application = str(record.get("application_id") or "")
            if application_id:
                # 已核对过该应用授权：返回组织通用 + 该应用的知识。
                if record_application not in ("", application_id):
                    continue
            elif record_application:
                # 未指定应用时不返回应用专属知识——无法逐条核对授权。失败关闭。
                continue
            if kind and str(record.get("kind") or "") != kind:
                continue
            if status and str(record.get("status") or "active") != status:
                continue
            result.append(self._knowledge_view(record))
        result.sort(key=lambda item: item.knowledge_id, reverse=True)
        return result

    async def get_knowledge(self, knowledge_id: str, context: TrustedContext) -> KnowledgeDetail:
        record = self._require_knowledge(knowledge_id, context)
        await self._ensure_knowledge_application(record, context)
        return self._knowledge_detail(record)

    async def deprecate_knowledge(self, knowledge_id: str, context: TrustedContext) -> KnowledgeView:
        """把一份知识标记为失效（保留记录，不物理删除）。失效后不再进入检索。"""
        record = self._require_knowledge(knowledge_id, context)
        await self._ensure_knowledge_application(record, context)
        if str(record.get("status") or "active") == "deprecated":
            return self._knowledge_view(record)
        record["status"] = "deprecated"
        record["deprecated_at"] = datetime.now(timezone.utc).isoformat()
        try:
            self._knowledge.save(record)
        except OSError as error:
            raise KnowledgeStateUnavailable("知识状态未落盘，请重试") from error
        return self._knowledge_view(record)

    async def search_knowledge(
        self,
        context: TrustedContext,
        query: str,
        *,
        kind: str | None = None,
        application_id: str = "",
        limit: int = 8,
    ) -> list[KnowledgeSearchHit]:
        """在服务端权限过滤之后检索项目知识。

        只会在"本组织 + 应用匹配 + 生效中"的片段里排序，因此跨组织、未授权应用或已失效的
        知识不可能出现在结果里。
        """
        organization_id = context.organization_id
        if not organization_id or not (query or "").strip():
            return []
        # 请求参数不是权限依据：应用范围必须先由治理后端核对授权。
        if application_id and not await self._application_authorized(context, application_id):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对）")
        chunks = visible_chunks(self._knowledge.list(), organization_id, application_id)
        if not chunks:
            return []
        retriever = HybridRetriever()
        retriever.add(chunks)
        prefixes = (KIND_PREFIX[kind],) if kind in KIND_PREFIX else None
        selected = retriever.search(
            query, limit=max(1, min(int(limit), 20)), prefixes=prefixes, organizations=(organization_id,)
        )
        hits: list[KnowledgeSearchHit] = []
        for item in selected:
            knowledge_id = item.chunk.doc_id.split("/", 1)[-1]
            hits.append(
                KnowledgeSearchHit(
                    knowledge_id=knowledge_id,
                    doc_id=item.chunk.doc_id,
                    title=item.chunk.title,
                    section=item.chunk.section,
                    version=item.chunk.version,
                    status=item.chunk.status,
                    snippet=item.chunk.as_snippet(),
                    score=item.score,
                )
            )
        return hits

    def _require_knowledge(self, knowledge_id: str, context: TrustedContext) -> dict[str, Any]:
        if not context.organization_id or not context.user_id:
            raise KnowledgeNotFound(knowledge_id)
        record = self._knowledge.get(knowledge_id)
        # 不存在与不属于本组织同样返回 404：不提供跨组织存在性探测。
        if record is None or str(record.get("organization_id") or "") != context.organization_id:
            raise KnowledgeNotFound(knowledge_id)
        return record

    async def _ensure_knowledge_application(self, record: dict[str, Any], context: TrustedContext) -> None:
        """应用专属知识：必须由治理后端确认调用方对该应用有授权。"""
        application = str(record.get("application_id") or "")
        if application and not await self._application_authorized(context, application):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对），不能访问应用专属知识")

    @staticmethod
    def _knowledge_view(record: dict[str, Any]) -> KnowledgeView:
        snippets = chunks_for(record)
        return KnowledgeView(
            knowledge_id=str(record.get("knowledge_id") or ""),
            organization_id=str(record.get("organization_id") or ""),
            application_id=str(record.get("application_id") or ""),
            kind=str(record.get("kind") or "norms"),
            title=str(record.get("title") or ""),
            version=str(record.get("version") or ""),
            source=str(record.get("source") or ""),
            status=str(record.get("status") or "active"),
            content_hash=str(record.get("content_hash") or ""),
            imported_by=str(record.get("imported_by") or ""),
            imported_at=record.get("imported_at"),
            deprecated_at=record.get("deprecated_at"),
            injection_hits=[str(item) for item in (record.get("injection_hits") or [])],
            snippet_count=len(snippets),
        )

    def _knowledge_detail(self, record: dict[str, Any]) -> KnowledgeDetail:
        view = self._knowledge_view(record)
        snippets = chunks_for(record)
        return KnowledgeDetail(
            **view.model_dump(),
            snippets=[
                KnowledgeSnippet(
                    evidence_id=chunk.evidence_id,
                    doc_id=chunk.doc_id,
                    section=chunk.section,
                    version=chunk.version,
                    status=chunk.status,
                    snippet=chunk.as_snippet(),
                )
                for chunk in snippets
            ],
        )

    # -- M3：独立评测中心 --------------------------------------------------

    async def create_eval_job(self, request: EvalJobRequest, context: TrustedContext) -> EvalJobView:
        """发起一次评测作业并在**隔离目录**中执行。

        真实模型（live）必须显式启用且配置凭据，否则作业记为 `not_run`——
        未运行就是未运行，不用离线结果冒充真实模型质量。
        """
        if not context.organization_id or not context.user_id:
            raise EvalInvalid("缺少可信身份，拒绝发起评测")
        record = new_job_record(
            job_id=f"eval_{uuid.uuid4().hex[:12]}",
            organization_id=context.organization_id,
            created_by=context.user_id,
            provider=request.provider,
            strategy=request.strategy,
            split=request.split,
            limit=request.limit,
        )
        if request.provider == "live" and not self._settings.live_model_ready:
            record["status"] = "not_run"
            record["finished_at"] = datetime.now(timezone.utc).isoformat()
            record["notes"] = [LIVE_NOT_RUN_NOTE, LIVE_ENABLE_HINT]
            self._save_eval(record)
            return EvalJobView.model_validate(job_view(record))

        self._save_eval(record)
        workdir = self._settings.eval_dir / str(record["job_id"])
        try:
            report = await run_evaluation(
                self._settings,
                provider=request.provider,
                strategy=request.strategy,
                split=request.split,
                limit=request.limit,
                workdir=workdir,
            )
        except Exception as error:  # noqa: BLE001 - 任何失败都必须留下可见的作业记录，不伪装成功
            record["status"] = "failed"
            record["finished_at"] = datetime.now(timezone.utc).isoformat()
            record["error"] = f"{type(error).__name__}: {error}"
            self._save_eval(record)
            return EvalJobView.model_validate(job_view(record))

        record["status"] = "completed"
        record["finished_at"] = datetime.now(timezone.utc).isoformat()
        record["dataset"] = report["dataset"]
        record["summary"] = report["summary"]
        record["cases"] = report["cases"]
        record["limitations"] = report.get("limitations") or []
        record["failure_classes"] = aggregate_failures(report["cases"])
        self._save_eval(record)
        return EvalJobView.model_validate(job_view(record))

    async def list_eval_jobs(self, context: TrustedContext) -> list[EvalJobView]:
        if not context.organization_id:
            return []
        records = [
            record for record in self._evals.list()
            if str(record.get("organization_id") or "") == context.organization_id
        ]
        records.sort(key=lambda item: str(item.get("created_at") or ""), reverse=True)
        return [EvalJobView.model_validate(job_view(record)) for record in records]

    async def get_eval_job(self, job_id: str, context: TrustedContext) -> EvalJobView:
        return EvalJobView.model_validate(job_view(self._require_eval(job_id, context)))

    async def eval_report(self, job_id: str, context: TrustedContext) -> EvalReport:
        record = self._require_eval(job_id, context)
        return EvalReport(
            job=EvalJobView.model_validate(job_view(record)),
            cases=list(record.get("cases") or []),
            limitations=list(record.get("limitations") or []),
        )

    async def compare_eval_jobs(self, base_id: str, target_id: str, context: TrustedContext) -> EvalComparison:
        base = self._require_eval(base_id, context)
        target = self._require_eval(target_id, context)
        return EvalComparison(base_job_id=base_id, target_job_id=target_id, **compare_jobs(base, target))

    def _save_eval(self, record: dict[str, Any]) -> None:
        try:
            self._evals.save(record)
        except OSError as error:
            raise EvalStateUnavailable("评测作业未落盘，请重试") from error

    def _require_eval(self, job_id: str, context: TrustedContext) -> dict[str, Any]:
        if not context.organization_id or not context.user_id:
            raise EvalNotFound(job_id)
        record = self._evals.get(job_id)
        if record is None or str(record.get("organization_id") or "") != context.organization_id:
            raise EvalNotFound(job_id)
        return record

    async def cancel(self, task_id: str, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        self._ensure_active(record)
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

    def _persist_ledger(self, task_id: str, execution_id: str, payload: dict[str, Any]) -> None:
        """把调用账本落盘。

        只覆盖 `usage` 字段，避免把并发写入的其它状态（例如取消）盖回去；
        执行已被取代或取消时**直接抛错**，让调用停止——不允许产生没有账目的消耗。
        """
        record = self._repository.get(task_id)
        if record is None:
            raise LedgerPersistError(f"任务 {task_id} 不存在，调用账本无法落盘")
        if (record.get("execution_id") or "") != execution_id:
            raise LedgerPersistError("执行已被取消或被新执行接管：账本停止记录，不再继续调用模型")
        record["usage"] = payload
        self._repository.save(record)

    def _has_live_execution(self, task_id: str) -> bool:
        """该任务是否已有一次**尚未结束**的执行。"""
        execution = self._executions.get(task_id)
        return execution is not None and not execution.finished.is_set()

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

        **外部模型请求不保证 exactly-once，这里也不这么宣称**：一次模型调用可能在进程崩溃
        或请求被取消之前就已经发出去了，我们无法撤销它。能保证的是两点——
        (1) 每次真的发出过的请求都会按次记进调用账本（并在恢复/重启后续算），
        (2) 任务不会因为"看起来恢复过"就被当成没消耗过资源。因此恢复路径会显式写入
        `recovery_semantics=at_least_once`（见 `resume` / `clarify`），并把不确定性留在事件里。
        """
        task_id = record["task_id"]
        # dispatch 前已持久化的事件轨迹：检查点恢复时图返回的事件**不含** dispatch
        # 之前的增量（clarify 写入的 snapshot_selected/cleared 等），直接覆盖会丢审计；
        # 结束时用 _merge_event_lists 按最长公共前缀合并（审计只增不减）。
        pre_events = list(record.get("events") or [])
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

        # 任务级账本：provider 是共享的，消耗必须记在**每个任务自己的**账本上，
        # 否则并发任务会互相串账；预算上限也在这一层生效。
        execution_id = record.get("execution_id") or ""
        ledger = UsageLedger(
            max_total_tokens=int(getattr(self._settings, "max_task_tokens", 0) or 0),
            max_prompt_tokens=int(getattr(self._settings, "max_task_prompt_tokens", 0) or 0),
            max_cost_estimate=float(getattr(self._settings, "max_task_cost_estimate", 0.0) or 0.0),
            prompt_price_per_1k=float(getattr(self._settings, "llm_price_prompt_per_1k", 0.0) or 0.0),
            completion_price_per_1k=float(getattr(self._settings, "llm_price_completion_per_1k", 0.0) or 0.0),
            max_requests=int(getattr(self._settings, "max_task_requests", 0) or 0),
            # 未提供 usage 的请求按一个保守值计入预算判定；没配置就用单次输出上界。
            unknown_charge_tokens=int(getattr(self._settings, "unknown_usage_charge_tokens", 0) or 0)
            or int(self._settings.llm_max_tokens),
            task_id=task_id,
            execution_id=execution_id,
            model=self._settings.llm_model,
        )
        # 恢复、重试与重启都要**续算**：任务累计预算不得被清零。
        ledger.seed_from_prior(record.get("usage"))
        # 每次记账后落盘：恢复才有据可续，也确保不出现"没有账目的模型调用"。
        ledger.on_update = lambda payload: self._persist_ledger(task_id, execution_id, payload)

        try:
            async with open_checkpointer(self._settings) as saver:
                workflow = self._build_workflow(record, saver)
                with usage_scope(ledger, phase=PHASE_GENERATE):
                    if resume is None:
                        await _delete_thread(saver, task_id)
                        record["resume_mode"] = None
                        result = await workflow.run(initial_state, thread_id=task_id)  # type: ignore[arg-type]
                    elif resume.get("mode") == RESUME_INTERRUPT:
                        result = await workflow.resume_interrupt(thread_id=task_id, value=resume.get("value") or {})
                    else:
                        result = await workflow.continue_pending(thread_id=task_id)
        finally:
            # 成功、失败、被取消都要把账本落到本次执行的记录上：
            # 失败的执行同样消耗了模型调用，不能因此丢失账目（否则恢复会重置预算）。
            record["usage"] = ledger.as_dict()

        interrupts = result.get("__interrupt__") if isinstance(result, dict) else None
        if interrupts:
            # 图停在**节点级中断**上：状态是 NEEDS_INFO，但检查点可从此节点续跑。
            payload = _first_interrupt_value(interrupts)
            record["status"] = TaskStatus.NEEDS_INFO.value
            record["questions"] = payload.get("questions") or []
            record["error"] = None
            record["awaiting_input"] = True
            events = _merge_event_lists(pre_events, result.get("events") or record.get("events") or [])
            events.append(event("awaiting_input", "图停在节点级中断上，等待用户补充信息后从该节点继续"))
            record["events"] = events
            _annotate_recovery(record)
            return record

        record["awaiting_input"] = False
        investigation = result.get("investigation") or {}
        record.update(
            {
                "status": result.get("status", TaskStatus.FAILED.value),
                "questions": result.get("questions") or [],
                "draft": result.get("draft"),
                "events": _merge_event_lists(pre_events, result.get("events") or record.get("events") or []),
                "revisions": int(result.get("revisions") or 0),
                "error": result.get("error"),
                "evidence_note": result.get("evidence_note"),
                # 调查轨迹与预算原样落库，供工作台展示"实际执行了什么、为什么停下"。
                "investigation": investigation or record.get("investigation"),
                "strategy": investigation.get("strategy") or record.get("strategy"),
            }
        )
        # 草案（重新）生成后保留一份版本快照；内容未变时不重复追加。
        _record_draft_version(record, origin="agent", actor="agent", reason="工作流生成", created_at=datetime.now(timezone.utc))
        # 草案重新生成后，与当前内容不一致的旧确认立即失效（保留痕跡）。
        _invalidate_stale_confirmations(record, "草案已重新生成，旧确认失效")
        _annotate_recovery(record)
        return record

    def _retriever_for(self, organization_id: str, application_id: str = "") -> Any:
        """按 (组织, 应用) 构建检索器：公开合成语料 + 该组织可见的导入知识。

        权限过滤在**服务端**这一层完成：只有通过 `is_visible`（组织匹配、应用匹配、
        处于生效状态）的知识才会被加入检索器，模型无法用参数扩大范围。
        """
        retriever = build_retriever(self._settings.demo_dir)
        extra = visible_chunks(self._knowledge.list(), organization_id, application_id)
        if extra:
            retriever.add(extra)
        return retriever

    def _build_workflow(self, record: dict[str, Any], checkpointer: Any) -> DraftWorkflow:
        # 检索只用**已核对通过**的应用；请求里声明的应用不能直接决定可见范围。
        retriever = self._retriever_for(
            str(record.get("organization_id") or ""), str(record.get("authorized_application") or "")
        )
        toolbox_factory = lambda snapshot: Toolbox(  # noqa: E731 - 需要按任务注入快照
            settings=self._settings,
            retriever=retriever,
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


def _screen_new_input(*texts: str) -> None:
    """对**新增输入**做与入口一致的注入筛查。

    检查点使旧节点不必重跑，但新输入（补充说明、替换的快照）仍必须校验：
    否则"通过检查点恢复"就成了绕过入口筛查的旁路。

    正则与 untrusted 标签只是**辅助**手段，挡的是明显的注入模式，
    不构成完整的安全保证——模型输出本身依旧按不可信数据处理。
    """
    hits: list[str] = []
    for text in texts:
        if text and str(text).strip():
            hits.extend(detect_injection(str(text)))
    if hits:
        raise TaskNotResumable("新增内容命中提示注入检测（" + "、".join(sorted(set(hits))) + "），已拒绝继续")


def _effective_requirement(record: dict[str, Any]) -> str:
    """把用户后续提供的补充说明并入需求文本，并标注来源，避免与原始需求混为一谈。"""
    notes = [str(item).strip() for item in (record.get("clarification_notes") or []) if str(item).strip()]
    requirement = record.get("requirement", "")
    if notes:
        requirement = requirement + "\n\n补充说明（用户后续提供）：\n" + "\n".join(f"- {item}" for item in notes)
    return requirement


def _apply_input_version(record: dict[str, Any]) -> None:
    """重算输入/材料版本；一旦变化，旧草案、旧检查结果与旧确认即失效。"""
    version = input_version(
        requirement=_effective_requirement(record),
        slots=record.get("slots") or {},
        schema_snapshot=record.get("schema_snapshot") or "",
    )
    if version != record.get("input_version"):
        # 输入/材料已变：旧草案、旧调查轨迹与旧检查结果都不再对应当前输入，必须失效——
        # 只清草案而留着旧轨迹，会让界面把上一次的调查结论当成这一次的。
        record["draft"] = None
        record["questions"] = []
        record["investigation"] = None
        record["evidence_note"] = None
        record["input_version"] = version
        # 调用账本**不清零**：预算是任务的累计消耗，改输入不等于没花过钱。
        # 旧的人工确认同样失效（材料内容/版本已变）。
        _invalidate_stale_confirmations(record, "输入或材料已变更，旧确认失效")


def _annotate_recovery(record: dict[str, Any]) -> None:
    """把"恢复 = at-least-once"的不确定性落到最终记录的事件里。

    必须在工作流返回**之后**补写：图的事件流来自检查点里保存的旧状态，会把恢复时在记录上
    追加的那一条覆盖掉。这里按 kind 去重，因此重复调用是幂等的。
    """
    events = list(record.get("events") or [])
    for item in record.get("management_events") or []:
        if item not in events:
            events.append(item)
    record["events"] = sorted(events, key=lambda item: item.get("at", ""))
    if record.get("recovery_semantics") != RECOVERY_AT_LEAST_ONCE:
        return
    events = list(record.get("events") or [])
    if any(item.get("kind") == "recovery_at_least_once" for item in events):
        return
    events.append(event("recovery_at_least_once", RECOVERY_UNCERTAINTY_NOTE))
    record["events"] = events


def _current_material_hash(record: dict[str, Any]) -> str:
    """当前材料（草案 SQL + 回滚）的内容摘要；没有草案时为空串。"""
    draft = record.get("draft") or {}
    if not draft:
        return ""
    return material_hash(str(draft.get("sql") or ""), str(draft.get("rollback_sql") or ""))


def _unified_diff(before: str, after: str, label: str) -> str:
    """统一 diff（服务端计算，不依赖浏览器）。内容相同则返回空串。"""
    lines = difflib.unified_diff(
        (before or "").splitlines(),
        (after or "").splitlines(),
        fromfile=f"{label}:before",
        tofile=f"{label}:after",
        lineterm="",
    )
    return "\n".join(lines)


def _material_summary(draft: dict[str, Any]) -> str:
    """人可读的材料摘要（语句类型、回滚、长度），仅用于版本列表展示。"""
    sql = str(draft.get("sql") or "")
    rollback = str(draft.get("rollback_sql") or "")
    statements = [part.strip() for part in sql.split(";") if part.strip()]
    verbs: list[str] = []
    for statement in statements:
        head = statement.split(None, 1)[0].upper() if statement.split() else ""
        if head and head not in verbs:
            verbs.append(head)
    parts = [f"{len(statements)} 条变更语句" + (f"（{'/'.join(verbs)}）" if verbs else "")]
    parts.append("含回滚" if rollback.strip() else "缺少回滚")
    parts.append(f"变更 {len(sql)} 字符")
    return "；".join(parts)


def _draft_version_entry(
    draft: dict[str, Any],
    *,
    origin: str,
    actor: str,
    reason: str,
    created_at: datetime | None = None,
    snapshot_source: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """把一份草案物化成不可变版本快照。"""
    check = draft.get("deterministic_check") or {}
    return {
        "version": int(draft.get("version") or 1),
        "created_at": (created_at or datetime.now(timezone.utc)).isoformat(),
        "origin": origin,
        "actor": actor,
        "reason": reason or "",
        "sql": draft.get("sql") or "",
        "rollback_sql": draft.get("rollback_sql") or "",
        "content_hash": material_hash(str(draft.get("sql") or ""), str(draft.get("rollback_sql") or "")),
        "summary": _material_summary(draft),
        "check_status": str(check.get("status") or "NOT_RUN"),
        "check_source": str(check.get("source") or "local_scan"),
        "check_error": check.get("error"),
        "check_blocking_count": int(check.get("blocking_count") or 0),
        "evidence_ids": [str(item.get("evidence_id")) for item in (draft.get("evidence") or [])],
        "revision_notes": list(draft.get("revision_notes") or []),
        # 快照溯源：该版本草案依据的知识快照（元数据+内容摘要），历史版本不随后续变更漂移。
        "snapshot_source": snapshot_source,
    }


def _record_draft_version(
    record: dict[str, Any],
    *,
    origin: str,
    actor: str,
    reason: str = "",
    draft: dict[str, Any] | None = None,
    created_at: datetime | None = None,
) -> bool:
    """追加一份草案版本快照；返回是否真的新增。

    - 工作流生成（origin="agent"）在内容与上一版**完全相同**时不新增，避免把恢复重跑
      记成"新材料版本"；
    - 人工编辑（origin="user_edit"）总会新增（版本号由编辑路径递增）。
    """
    resolved = draft if draft is not None else (record.get("draft") or {})
    if not resolved:
        return False
    entry = _draft_version_entry(
        resolved,
        origin=origin,
        actor=actor,
        reason=reason,
        created_at=created_at,
        snapshot_source=record.get("selected_snapshot"),
    )
    versions = list(record.get("draft_versions") or [])
    if origin == "agent" and versions and versions[-1].get("content_hash") == entry["content_hash"]:
        return False
    versions.append(entry)
    record["draft_versions"] = versions
    return True


def _invalidate_stale_confirmations(record: dict[str, Any], reason: str) -> bool:
    """把与当前材料（版本 + 内容哈希）不一致的确认标记为失效（保留痕跡）。"""
    version = str(record.get("input_version") or "")
    digest = _current_material_hash(record)
    changed = False
    for item in record.get("confirmations") or []:
        if item.get("invalidated_at"):
            continue
        if item.get("material_version") != version or item.get("material_hash") != digest:
            item["invalidated_at"] = datetime.now(timezone.utc).isoformat()
            item["invalidate_reason"] = reason
            changed = True
    return changed


def _same_event(left: dict[str, Any], right: dict[str, Any]) -> bool:
    """事件相等：at/kind/detail 逐值比较（at 带微秒时间戳，碰撞概率可忽略）。"""
    return (
        left.get("at") == right.get("at")
        and left.get("kind") == right.get("kind")
        and left.get("detail") == right.get("detail")
    )


def _merge_event_lists(pre: list[dict[str, Any]], produced: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """合并 dispatch 前的事件轨迹与图执行返回的事件轨迹，审计只增不减。

    - 全新执行：图从 initial_state 出发，produced 完整继承 pre，前缀逐项相等，
      直接采用 produced（pre + produced[common:] 恰好还原 produced，不重复）。
    - 检查点恢复：produced 只含检查点里的旧轨迹与恢复后的增量，**不含** dispatch 前
      写入的增量（clarified / snapshot_selected / snapshot_cleared 等）→ 取最长公共
      前缀后拼接，避免用旧轨迹覆盖掉补充信息阶段新写入的审计事件。
    """
    if not produced:
        return list(pre)
    common = 0
    for old, new in zip(pre, produced):
        if not _same_event(old, new):
            break
        common += 1
    return list(pre) + list(produced[common:])


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


__all__ = ["AgentService", "TaskNotFound", "TaskNotResumable", "TaskNotConfirmable", "DatabaseKind"]
