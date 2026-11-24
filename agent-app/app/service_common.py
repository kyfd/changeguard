"""AgentService 共享常量、异常定义与纯函数辅助。"""

from __future__ import annotations

import difflib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from app.guard import detect_injection
from app.schemas.drafts import TaskStatus
from app.workflow.state import event, input_version, material_hash

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

