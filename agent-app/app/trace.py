"""执行轨迹投影。

把**已经落盘的**事件、工具观察与调用账本投影成一条可核对的步骤视图：

- 步骤来自真实记录，不推断、不编造；工具成功/失败、模型调用的 outcome 与耗时、
  引用片段、token 与检查结论都取自已持久化的值。
- **不含模型内部思维链**：这里没有、也不会合成"模型在想什么"。
- 未知即未知：未记录的耗时 / token / 费用是 `None`，绝不用 0 或空串冒充"已记录"，
  并在 `unknown` 里显式列出不确定的字段。

排序：有时间戳的步骤按时间升序；模型调用账本不带时间戳，按记录顺序排在末尾。
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from app.schemas.drafts import TaskTrace, TraceStep

# 事件 kind → 步骤状态。白名单之外一律 unknown，不猜。
_FAILURE_EVENT_KINDS = {"failed", "timeout", "dispatch_failed", "abandoned", "restart"}
_BLOCKED_EVENT_KINDS = {"awaiting_input", "check_info"}
_OK_EVENT_KINDS = {
    "created",
    "screen_input",
    "retrieve_evidence",
    "generate_draft",
    "run_check",
    "finalize",
    "clarified",
    "resumed",
    "recovery_at_least_once",
    "confirmed",
    "archived",
    "restored",
    "moved_to_trash",
    "draft_edited",
    "change_linked",
}


def _parse_at(value: Any) -> datetime | None:
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    if isinstance(value, str) and value.strip():
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            return None
        return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)
    return None


def _event_status(kind: str) -> str:
    if kind in _FAILURE_EVENT_KINDS:
        return "failed"
    if kind in _BLOCKED_EVENT_KINDS:
        return "waiting" if kind == "awaiting_input" else "blocked"
    if kind in _OK_EVENT_KINDS:
        return "ok"
    return "unknown"


def _build_steps(record: dict[str, Any]) -> tuple[list[TraceStep], list[str]]:
    unknown: list[str] = []
    steps: list[TraceStep] = []

    for item in record.get("events") or []:
        if not isinstance(item, dict):
            continue
        kind = str(item.get("kind") or "event")
        steps.append(
            TraceStep(
                index=0,
                kind="event",
                name=kind,
                status=_event_status(kind),
                at=_parse_at(item.get("at")),
                detail=str(item.get("detail") or ""),
            )
        )

    investigation = record.get("investigation") or {}
    observation_missing_duration = False
    for item in investigation.get("tool_observations") or []:
        if not isinstance(item, dict):
            continue
        duration = item.get("duration_ms")
        if not isinstance(duration, int) or isinstance(duration, bool):
            duration = None
            observation_missing_duration = True
        observed_at = _parse_at(item.get("observed_at"))
        if observed_at is None:
            unknown.append("tool_observation.observed_at")
        steps.append(
            TraceStep(
                index=0,
                kind="tool",
                name=str(item.get("tool") or "tool"),
                tool=str(item.get("tool") or "") or None,
                status="ok" if item.get("ok") else "failed",
                at=observed_at,
                duration_ms=duration,
                detail=str(item.get("summary") or ""),
                error=str(item.get("error") or "") or None,
                evidence_ids=[str(value) for value in (item.get("evidence_ids") or [])],
            )
        )
    if observation_missing_duration:
        unknown.append("step_duration_ms")

    usage = record.get("usage") or {}
    for call in usage.get("calls") or []:
        if not isinstance(call, dict):
            continue
        known = bool(call.get("usage_known"))
        duration = call.get("duration_ms")
        if not isinstance(duration, int) or isinstance(duration, bool):
            duration = None
            unknown.append("step_duration_ms")
        steps.append(
            TraceStep(
                index=0,
                kind="model_call",
                name=str(call.get("phase") or "model"),
                phase=str(call.get("phase") or "") or None,
                model=str(call.get("model") or "") or None,
                status=str(call.get("outcome") or "unknown"),
                duration_ms=duration,
                error=str(call.get("failure_type") or "") or None,
                prompt_tokens=(int(call["prompt_tokens"]) if known and call.get("prompt_tokens") is not None else None),
                completion_tokens=(
                    int(call["completion_tokens"]) if known and call.get("completion_tokens") is not None else None
                ),
                usage_known=known,
            )
        )

    draft = record.get("draft") or {}
    check = draft.get("deterministic_check") if isinstance(draft, dict) else None
    if isinstance(check, dict):
        steps.append(
            TraceStep(
                index=0,
                kind="check",
                name=str(check.get("source") or "local_scan"),
                status=str(check.get("status") or "NOT_RUN").lower(),
                at=_parse_at(check.get("checked_at")),
                detail=f"阻断 {int(check.get('blocking_count') or 0)} 项",
                error=str(check.get("error") or "") or None,
            )
        )
    elif record.get("draft"):
        unknown.append("check_status")

    # 排序：有时间戳的按时间升序，无时间戳的（模型调用账本）按记录顺序排在末尾。
    indexed = list(enumerate(steps))
    indexed.sort(key=lambda pair: (pair[1].at is None, pair[1].at or datetime.min.replace(tzinfo=timezone.utc), pair[0]))
    ordered = [step for _original, step in indexed]
    for position, step in enumerate(ordered, start=1):
        step.index = position
    return ordered, unknown


def _usage_summary(record: dict[str, Any]) -> tuple[dict[str, Any] | None, list[str]]:
    usage = record.get("usage")
    if not isinstance(usage, dict):
        return None, ["usage"]
    unknown: list[str] = []
    if not usage.get("known"):
        unknown.append("token_totals")
    if not usage.get("cost_known"):
        unknown.append("cost_estimate")
    return usage, unknown


def build_trace(record: dict[str, Any]) -> TaskTrace:
    """从任务记录投影出执行轨迹。纯函数，不修改记录。"""
    steps, unknown = _build_steps(record)
    usage, usage_unknown = _usage_summary(record)
    for field in usage_unknown:
        if field not in unknown:
            unknown.append(field)
    # 去重并保序。
    deduped = list(dict.fromkeys(unknown))

    notes = [
        "轨迹只包含事件、工具观察、模型调用元数据与检查结论，不含模型内部思维链。",
        "未记录的耗时 / token / 费用保持未知（null），不以 0 代替。",
    ]
    return TaskTrace(
        task_id=str(record.get("task_id") or ""),
        execution_id=(record.get("execution_id") or None),
        generated_at=datetime.now(timezone.utc),
        steps=steps,
        usage=usage,
        notes=notes,
        unknown=deduped,
        includes_model_reasoning=False,
    )
