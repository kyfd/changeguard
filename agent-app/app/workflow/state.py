"""工作流状态与槽位追问。

状态全部是可序列化的普通结构，便于持久化、重放与测试。
**没有"模型内部推理过程"这一项**——不保存思维链。
"""

from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone
from typing import Any, Mapping, TypedDict

from app.schemas.drafts import ClarificationQuestion, TaskSlots

SLOT_FIELDS = (
    "application",
    "environment",
    "database",
    "table",
    "query_sql",
    "planned_at",
    "planned_at_timezone",
)


def merge_slot_data(slots: TaskSlots, data: Mapping[str, Any]) -> TaskSlots:
    """只覆盖实际提供的槽位字段，其余保持不变。"""
    merged = slots.model_dump()
    for field in SLOT_FIELDS:
        if field in data and data[field] is not None:
            merged[field] = data[field]
    return TaskSlots.model_validate(merged)


def input_version(*, requirement: str, slots: Mapping[str, Any], schema_snapshot: str) -> str:
    """需求 + 槽位 + 表结构快照的稳定摘要。

    这是"输入与材料版本"的判据：它一旦改变，旧的检查点结果与旧的人工确认就都不再适用，
    不能带着陈旧上下文恢复（见 P2 §4/§5）。
    """
    payload = json.dumps(
        {"requirement": requirement, "slots": dict(slots), "schema_snapshot": schema_snapshot or ""},
        sort_keys=True,
        ensure_ascii=False,
        default=str,
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def material_hash(sql: str, rollback_sql: str) -> str:
    """材料内容摘要（变更 SQL + 回滚 SQL）。

    人工确认记录绑定的是**这份内容**，而不是"某个时间点之后的一切"：
    草案一旦被重新生成且内容不同，旧确认就应当失效。
    """
    payload = f"{sql or ''}\x00{rollback_sql or ''}"
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


# 追问文案：包含「为什么问」，避免用户不知道要补什么。
QUESTION_TEMPLATES: dict[str, ClarificationQuestion] = {
    "application": ClarificationQuestion(
        field="application",
        question="这次变更属于哪个应用？",
        reason="应用决定权限范围、依赖关系与发布窗口，必须明确。",
        examples=["order-service"],
    ),
    "environment": ClarificationQuestion(
        field="environment",
        question="目标环境是哪个（生产 / 预发 / 测试）？",
        reason="不同环境的风险判定与检查强度不同。",
        examples=["生产", "预发"],
    ),
    "database": ClarificationQuestion(
        field="database",
        question="目标数据库类型是什么？",
        reason="语法与并发选项随数据库不同，不能凭空推断。",
        examples=["postgresql", "mysql"],
    ),
    "table": ClarificationQuestion(
        field="table",
        question="涉及哪张表？",
        reason="需要对照表结构快照确认现有索引与写入特征。",
        examples=["orders"],
    ),
    "planned_at": ClarificationQuestion(
        field="planned_at",
        question="计划执行时间是什么？（请给出带时区的明确时刻）",
        reason="规范要求明确时刻，不接受「周五晚上」这类模糊表述。",
        examples=["2026-09-18T21:30:00+08:00"],
    ),
}


def questions_for(missing: list[str]) -> list[ClarificationQuestion]:
    return [QUESTION_TEMPLATES[field] for field in missing if field in QUESTION_TEMPLATES]


def build_questions(
    missing: list[str],
    extra: list[str] | None = None,
    suggestions: dict[str, str] | None = None,
) -> list[ClarificationQuestion]:
    questions = questions_for(missing)
    if suggestions:
        # 建议值只挂在追问上供用户核对，不代表这些槽位已被填上。
        questions = [
            item.model_copy(
                update={"suggested": suggestions[item.field], "suggested_from": "需求原文"}
            )
            if item.field in suggestions
            else item
            for item in questions
        ]
    for note in extra or []:
        questions.append(
            ClarificationQuestion(
                field="open_question",
                question=note,
                reason="草案生成的未决问题，需要人工确认。",
            )
        )
    return questions


def event(kind: str, detail: str = "") -> dict[str, Any]:
    return {"at": datetime.now(timezone.utc).isoformat(), "kind": kind, "detail": detail}


class WorkflowState(TypedDict, total=False):
    """工作流状态。"""

    task_id: str
    requirement: str
    slots: dict[str, Any]
    schema_snapshot: str

    # 输入与材料版本（需求 + 槽位 + 快照的摘要）。恢复前必须与任务记录当前值比对：
    # 不一致说明用户改过输入/材料，旧检查点结果不再适用。随检查点一起保存。
    input_version: str

    questions: list[dict[str, Any]]
    evidence: list[dict[str, Any]]
    evidence_note: str | None

    # 调查结论的结构化状态（策略、决策者、停止原因、预算消耗、缺失的必需证据、
    # 是否阻断）。路由依据它决定是继续生成草案还是终止，而不是只看事件日志。
    investigation: dict[str, Any]

    draft_text: str | None
    draft: dict[str, Any] | None
    draft_signature: str | None

    check: dict[str, Any] | None
    feedback: list[str]
    previous_feedback: list[str]

    revisions: int
    max_revisions: int
    no_progress: bool

    status: str
    error: str | None
    events: list[dict[str, Any]]
    cancelled: bool


def slots_from_state(state: WorkflowState) -> TaskSlots:
    return TaskSlots.model_validate(state.get("slots") or {})
