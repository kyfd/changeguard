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
        if not (self.application or "").strip():
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


class TaskView(BaseModel):
    """对外暴露的任务视图。"""

    task_id: str
    status: TaskStatus
    requirement: str
    slots: TaskSlots
    questions: list[ClarificationQuestion] = Field(default_factory=list)
    draft: Draft | None = None
    events: list[TaskEvent] = Field(default_factory=list)
    revisions: int = 0
    error: str | None = None
    planned_at_missing: bool = False


class CreateTaskRequest(BaseModel):
    """创建任务的输入。只允许用户显式提供，不允许由模型自行补全后写回。"""

    requirement: str = Field(min_length=1, max_length=4000)
    application: str | None = None
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
    environment: str | None = None
    database: DatabaseKind | None = None
    table: str | None = None
    query_sql: str | None = None
    planned_at: datetime | None = None
    planned_at_timezone: str | None = None
    note: str | None = None


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
