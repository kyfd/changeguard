"""结构化契约。

这份文件是整个 Agent 的"数据形状"，也是面试里最值得讲的部分：
**模型只能往这里填内容，不能改变字段的含义，也不能覆盖确定性检查结果。**
"""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any, Literal

from pydantic import BaseModel, Field, field_validator

MAX_SQL_CHARS = 20000


class DatabaseKind(str, Enum):
    """目标数据库类型。未知时保持 unknown，不允许模型猜。"""

    POSTGRESQL = "postgresql"
    MYSQL = "mysql"
    UNKNOWN = "unknown"


class TaskStatus(str, Enum):
    RECEIVED = "RECEIVED"
    NEEDS_INFO = "NEEDS_INFO"
    RUNNING = "RUNNING"
    DRAFT_READY = "DRAFT_READY"
    CHECK_BLOCKED = "CHECK_BLOCKED"
    INPUT_REJECTED = "INPUT_REJECTED"
    FAILED = "FAILED"
    CANCELLED = "CANCELLED"


def is_terminal(status: TaskStatus) -> bool:
    return status in {
        TaskStatus.DRAFT_READY,
        TaskStatus.CHECK_BLOCKED,
        TaskStatus.INPUT_REJECTED,
        TaskStatus.FAILED,
        TaskStatus.CANCELLED,
    }


class TaskSlots(BaseModel):
    """生成草案所必需/可选的信息槽位。

    缺失判定只看这里，不看模型"觉得自己懂了"。
    """

    # 应用的 canonical ID（来自治理服务）。展示名称继续放在 application；
    # 授权核对、关联与检索只认这个 ID，绝不按名称模糊匹配。
    application_id: str | None = None
    # 应用展示名称（仅用于界面展示与历史兼容；授权核对与检索只认 application_id）。
    application: str | None = None
    environment: str | None = None
    database: DatabaseKind | None = None
    table: str | None = None
    query_sql: str | None = None
    planned_at: datetime | None = None
    planned_at_timezone: str | None = None

    def missing(self) -> list[str]:
        """返回仍然缺失的必填槽位名。"""
        missing: list[str] = []
        # 历史兼容：名称-only 的任务标记 legacy 后允许继续走流程（授权核对与
        # 检索只认 application_id，不用应用专属知识）；两者都缺才算缺失。
        if not (self.application_id or "").strip() and not (self.application or "").strip():
            missing.append("application")
        if not (self.environment or "").strip():
            missing.append("environment")
        if self.database is None or self.database == DatabaseKind.UNKNOWN:
            missing.append("database")
        if not (self.table or "").strip():
            missing.append("table")
        if not (self.planned_at or None):
            missing.append("planned_at")
        return missing


class ClarificationQuestion(BaseModel):
    """追问。包含"为什么问"，避免用户不知道要补什么。"""

    field: str
    question: str
    reason: str
    examples: list[str] = Field(default_factory=list)
    # 从需求原文确定性抽取出的候选值：仅作预填建议。
    # 未经用户提交不会进入 slots，也不参与任何缺失判定或检查结论。
    suggested: str | None = None
    suggested_from: str | None = None


class EvidenceRef(BaseModel):
    """引用证据。必须能追溯到实际文档片段，禁止编造。"""

    evidence_id: str
    doc_id: str
    title: str
    section: str | None = None
    version: str | None = None
    snippet: str
    source: str
    status: Literal["active", "deprecated", "unknown"] = "unknown"
    score: float = 0.0
    # 适用范围原文（例如"PostgreSQL 生产库"）。缺失表示文档没写，即**无法核对适用性**。
    applicability: str = ""


class Assumption(BaseModel):
    """假设与待确认项。必须与"已确认事实"分开。"""

    statement: str
    confirmed: bool = False
    needs_confirmation: bool = True


class AiRiskAdvice(BaseModel):
    """模型的风险建议。

    **不参与任何放行判定**，与 ChangeGuard 中 `AdvisoryRisk` 的语义一致。
    """

    advisory_risk: Literal["LOW", "MEDIUM", "HIGH", "UNKNOWN"] = "UNKNOWN"
    summary: str = ""
    reasons: list[str] = Field(default_factory=list)


class CheckItem(BaseModel):
    code: str
    severity: str = "MEDIUM"
    blocking: bool = False
    title: str = ""
    suggestion: str = ""


class DeterministicCheck(BaseModel):
    """确定性检查结果。**模型不得覆盖、不得清空。**"""

    status: Literal["NOT_RUN", "PASSED", "BLOCKED", "FAILED"] = "NOT_RUN"
    source: str = "local_scan"
    checked_at: datetime | None = None
    items: list[CheckItem] = Field(default_factory=list)
    blocking_count: int = 0
    error: str | None = None


class Draft(BaseModel):
    """变更草案。字段与计划中"草案至少包含"的清单一一对应。"""

    version: int = 1
    requirement: str
    application: str
    environment: str
    database: DatabaseKind = DatabaseKind.UNKNOWN
    planned_at: datetime | None = None
    planned_at_timezone: str | None = None

    sql: str = ""
    rollback_sql: str = ""

    assumptions: list[Assumption] = Field(default_factory=list)
    open_questions: list[str] = Field(default_factory=list)

    evidence: list[EvidenceRef] = Field(default_factory=list)
    ai_advice: AiRiskAdvice = Field(default_factory=AiRiskAdvice)
    deterministic_check: DeterministicCheck = Field(default_factory=DeterministicCheck)

    revision_notes: list[str] = Field(default_factory=list)

    @field_validator("sql", "rollback_sql")
    @classmethod
    def _limit_sql(cls, value: str) -> str:
        if len(value) > MAX_SQL_CHARS:
            raise ValueError(f"SQL 超过 {MAX_SQL_CHARS} 字符上限")
        return value

    def rendered_sql(self) -> str:
        """用于静态扫描的完整文本（含回滚）。"""
        parts = [self.sql]
        if self.rollback_sql.strip():
            parts.append(self.rollback_sql)
        return "\n".join(parts)


class TaskEvent(BaseModel):
    """任务事件。用于展示进度与复盘，不含模型内部推理过程。"""

    at: datetime
    kind: str
    detail: str = ""


class DraftVersion(BaseModel):
    """草案版本快照。

    每次草案生成或服务端编辑都追加一份**不可变**快照，使"谁在什么时候、因为什么
    把材料改成了什么"可复核。它是审计记录，不参与放行判定；物化删除时也一并保留。

    未知值保持 None / 显式为 unknown，不用 0 或空字符串冒充"已记录"。
    """

    version: int
    created_at: datetime
    # agent（工作流生成）/ user_edit（服务端版本化编辑）
    origin: Literal["agent", "user_edit", "initial"] = "agent"
    actor: str = "agent"
    reason: str = ""
    sql: str = ""
    rollback_sql: str = ""
    # 内容摘要（SQL + 回滚的 SHA-256），与人工确认绑定的 material_hash 同一算法。
    content_hash: str = ""
    # 内容摘要（人可读：语句类型、是否含回滚、长度），仅用于列表展示。
    summary: str = ""
    check_status: str = "NOT_RUN"
    check_source: str = "local_scan"
    check_error: str | None = None
    check_blocking_count: int = 0
    evidence_ids: list[str] = Field(default_factory=list)
    revision_notes: list[str] = Field(default_factory=list)
    # 该版本生成时实际使用的结构快照来源（knowledge_id/title/version/content_hash/
    # selected_at/selected_by）。None 表示未使用知识库快照（手填快照或无快照）。
    # 历史版本保留当时的来源记录：快照随后失效/更新不影响既有版本的追溯。
    snapshot_source: dict[str, Any] | None = None


class TraceStep(BaseModel):
    """执行轨迹的一步。

    **不含模型内部思维链**：只展示事件、工具观察、模型调用元数据与检查结论。
    `duration_ms` 为 None 表示**未记录耗时（未知）**，不是 0 毫秒。
    token / 费用同理：未知即 None，绝不填 0 冒充"确定没有消耗"。
    """

    index: int
    kind: Literal["event", "tool", "model_call", "check", "draft"]
    name: str
    status: str = "unknown"
    at: datetime | None = None
    duration_ms: int | None = None
    detail: str = ""
    error: str | None = None
    tool: str | None = None
    phase: str | None = None
    model: str | None = None
    evidence_ids: list[str] = Field(default_factory=list)
    prompt_tokens: int | None = None
    completion_tokens: int | None = None
    usage_known: bool | None = None
    cost_estimate: float | None = None
    cost_known: bool = False


class TaskTrace(BaseModel):
    """任务执行轨迹视图。

    从**已落盘的事件、工具观察与调用账本**投影而来：可核对的是"真的发生了什么"，
    没有记录的一律标为未知。它不包含、也不推断模型内部推理过程。
    """

    task_id: str
    execution_id: str | None = None
    generated_at: datetime
    steps: list[TraceStep] = Field(default_factory=list)
    usage: dict[str, Any] | None = None
    notes: list[str] = Field(default_factory=list)
    # 明确列出因缺少记录而无法确定的字段，避免"看起来完整"。
    unknown: list[str] = Field(default_factory=list)
    # 恒为 False：轨迹从不包含模型内部思维链。
    includes_model_reasoning: bool = False


class DraftVersionDiff(BaseModel):
    """两个草案版本之间的差异（服务端用统一 diff 计算）。"""

    task_id: str
    from_version: int | None
    to_version: int
    sql_diff: str = ""
    rollback_diff: str = ""


class Confirmation(BaseModel):
    """材料的人工确认记录。

    **材料确认 ≠ 治理审批 ≠ 执行许可**：它只表示"某个人看过这一版材料"，
    不参与任何放行判定，也不授权在生产执行。确认人、时间与所确认的材料内容
    （版本 + 内容哈希）都必须落库，便于事后复核；草案一旦重新生成且内容不同，
    旧确认即失效（保留痕跡，不物理删除）。
    """

    confirmation_id: str
    confirmed_by: str
    confirmed_organization: str
    confirmed_at: datetime
    material_version: str
    material_hash: str
    note: str = ""
    invalidated_at: datetime | None = None
    invalidate_reason: str | None = None

    @property
    def active(self) -> bool:
        return self.invalidated_at is None


class ChangeLink(BaseModel):
    """Agent 任务与**正式变更单**的关联记录。

    关联的权威在治理服务：这里只记录"某个已存在的正式变更单由本任务的人工动作
    关联而来"。它**不是**授权凭据，也不代表变更单已被批准或可在生产执行。
    """

    change_request_id: str
    organization_id: str
    linked_by: str
    linked_at: datetime
    # 关联的来源标记（恒为 agent_task：仅由工作台的人工动作建立）。
    origin: Literal["agent_task"] = "agent_task"
    # 建立关联时使用的幂等键，便于事后复核"重复点击没有产生重复记录"。
    idempotency_key: str | None = None


class SelectedSnapshot(BaseModel):
    """任务明确选用的知识库结构快照记录。

    只保存选用时的快照身份与内容摘要，便于追溯"生成时用的到底是哪份结构"；
    快照正文不在这里保存——生成时按 knowledge_id 从知识库读取，并核对
    content_hash 一致后才进入生成输入。快照后续失效或更新不影响已生成版本
    的来源记录，也不改变任何治理或检查规则。
    """

    knowledge_id: str
    title: str
    version: str
    content_hash: str
    # 选用时任务绑定的应用 ID，防止跨应用误用快照。
    application_id: str = ""
    selected_at: datetime
    selected_by: str = ""


class TaskView(BaseModel):
    """对外暴露的任务视图。"""

    task_id: str
    source: Literal["production", "evaluation", "demo", "legacy"] = "legacy"
    created_at: datetime | None = None
    updated_at: datetime | None = None
    archived_at: datetime | None = None
    deleted_at: datetime | None = None
    status: TaskStatus
    requirement: str
    slots: TaskSlots
    questions: list[ClarificationQuestion] = Field(default_factory=list)
    draft: Draft | None = None
    events: list[TaskEvent] = Field(default_factory=list)
    revisions: int = 0
    error: str | None = None
    planned_at_missing: bool = False
    # 图是否停在节点级中断上等待用户补充（区别于"走到 END 的 NEEDS_INFO"）。
    awaiting_input: bool = False
    # 本次执行是恢复还是全新执行：interrupt（从中断点续跑）/ checkpoint（续跑未完成节点）/
    # restart_from_scratch（受支持的重跑，明确不是续跑）/ None（首次执行）。
    resume_mode: str | None = None
    # 恢复的执行语义。外部模型请求不保证 exactly-once：恢复会**重新执行**被中断的节点，
    # 那次调用可能已经发出去了，因此这里是 `at_least_once`，不是"重放一次相同的执行"。
    recovery_semantics: str | None = None
    # 进程重启时的处置策略，便于界面与验收复核。
    restart_policy: str | None = None
    # 当前材料摘要与人工确认记录（确认 ≠ 审批 ≠ 执行许可）。
    material_hash: str | None = None
    confirmations: list[Confirmation] = Field(default_factory=list)
    # 草案版本快照数量（完整快照走 /tasks/{id}/draft/versions，避免列表响应膨胀）。
    draft_version_count: int = 0
    # Agent 任务与正式变更单的服务端权威关联（由人工动作触发、服务端校验后写入）。
    change_links: list["ChangeLink"] = Field(default_factory=list)
    # 执行策略与调查轨迹（决策者、停止原因、预算、工具结果摘要），供工作台展示。
    strategy: str | None = None
    investigation: dict[str, Any] | None = None
    # usage 与预算：provider 未提供时是 unknown，不填 0。
    usage: dict[str, Any] | None = None
    # 应用绑定状态：authorized（已绑定且授权核对通过）/ unauthorized（上次核对失败）/
    # legacy（仅有名称的历史任务，等待用户显式重新选择）/ none（未绑定应用）。
    # 判定权威在服务端，前端据此提示重新选择，绝不按名称模糊匹配。
    application_binding: str = "none"
    # 授权核对通过时服务端回填的展示名称；未核对通过时保持空串。
    authorized_application: str = ""
    # 明确选用的知识库结构快照（校验通过后才写入）。手填 schema_snapshot 与之互斥。
    selected_snapshot: SelectedSnapshot | None = None
    # 当前表结构快照的来源类型：knowledge（知识库选用）/ manual（手填）/ ""（未提供）。
    # 供工作台展示与替换决策；判定权威在服务端记录，前端只展示。
    snapshot_source_kind: str = ""


class CreateTaskRequest(BaseModel):
    """创建任务的输入。只允许用户显式提供，不允许由模型自行补全后写回。"""

    requirement: str = Field(min_length=1, max_length=4000)
    application: str | None = None
    # 应用的 canonical ID：提供时由治理服务核对授权；留空表示不绑定应用（检索只看组织通用知识）。
    application_id: str | None = None
    # 明确选用知识库中的一份结构快照（kind=schema、生效中）。与手填 schema_snapshot 互斥：
    # 两者同时提供会被服务端拒绝，由用户二选一，不静默覆盖或拼接。
    snapshot_knowledge_id: str | None = None
    environment: str | None = None
    database: DatabaseKind | None = None
    table: str | None = None
    query_sql: str | None = None
    planned_at: datetime | None = None
    planned_at_timezone: str | None = None
    schema_snapshot: str | None = Field(
        default=None,
        description="表结构快照文本。首版使用上传/导入的快照，不接生产库实时探索。",
    )


class ClarifyRequest(BaseModel):
    """补充信息。字段都可选，只覆盖提供的那部分。"""

    application: str | None = None
    # 重新选择应用：提供 canonical ID。核对授权与绑定一律以 ID 为准。
    application_id: str | None = None
    # 重新选择结构快照：提供 ID 切换为知识快照，空串清除选用，缺省保持不变。
    snapshot_knowledge_id: str | None = None
    environment: str | None = None
    database: DatabaseKind | None = None
    table: str | None = None
    query_sql: str | None = None
    planned_at: datetime | None = None
    planned_at_timezone: str | None = None
    # 表结构快照不是槽位而是任务级材料，但必须能在这里补齐——
    # 否则"缺少快照"就成了一条无法通过补充信息走通的死路。
    schema_snapshot: str | None = Field(
        default=None,
        description="补充或替换表结构快照。仍只使用导入的快照，不接生产库实时探索。",
    )
    # 自由文本说明。以前这个字段被接收后直接丢弃，用户看不到任何效果；
    # 现在会写入任务记录，并在下一次执行时作为「补充说明」拼进需求文本。
    note: str | None = None


class ConfirmRequest(BaseModel):
    """人工确认材料。只允许附加说明，不能改变材料或放行判定。"""

    note: str | None = Field(default=None, max_length=2000)
    # 调用方**所看到的**材料内容哈希。提供时服务端必须核对一致，否则拒绝并要求刷新——
    # 否则一个停留在旧页面的用户会把"已经改过的材料"确认掉。
    material_hash: str | None = Field(default=None, max_length=128)


class DeleteTaskRequest(BaseModel):
    """绑定删除预览看到的完整记录，状态变化后必须重新预览。"""

    record_version: str = Field(pattern=r"^[0-9a-f]{64}$")


class DeleteTaskPreview(BaseModel):
    task_id: str
    allowed: bool
    blockers: list[str]
    record_version: str
    effect: Literal["recycle_bin"] = "recycle_bin"
    retained: list[str] = Field(default_factory=lambda: ["audit_events", "checkpoints", "usage"])


class DraftEditRequest(BaseModel):
    """服务端版本化编辑草案。

    `expected_version` 是调用方**所看到的**草案版本。服务端必须核对一致，否则拒绝并要求
    刷新——否则一个停留在旧页面的用户会把已经改过的材料覆盖掉（丢失并发修改）。
    """

    expected_version: int = Field(ge=1)
    sql: str = Field(min_length=1, max_length=MAX_SQL_CHARS)
    rollback_sql: str = Field(default="", max_length=MAX_SQL_CHARS)
    reason: str = Field(default="", max_length=2000)


class LinkChangeRequest(BaseModel):
    """把本任务与一个**已存在**的正式变更单关联（人工动作）。"""

    change_request_id: str = Field(min_length=1, max_length=128)
    # 幂等键：重复提交同一键不会产生重复关联；缺省时按 change_request_id 去重。
    idempotency_key: str | None = Field(default=None, max_length=128)


class ToolResult(BaseModel):
    """统一工具返回结构：成功/失败、数据、证据标识、版本或时间、可公开错误。"""

    ok: bool
    tool: str
    data: dict[str, Any] = Field(default_factory=dict)
    evidence_ids: list[str] = Field(default_factory=list)
    data_version: str | None = None
    observed_at: datetime | None = None
    error: str | None = None
    duration_ms: int = 0
