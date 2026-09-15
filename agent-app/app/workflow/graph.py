"""工作流图（LangGraph）。

显式状态 + **有限循环**：

```
START → check_info ──缺失──▶ END（追问）
            │完整
            ▼
     retrieve_evidence → generate_draft → run_check
                                ▲            │
                                └── 可修订且未达上限 ──┘
                                             │完成/无进展/达上限
                                             ▼
                                          finalize → END
```

三条边界：
1. `max_revisions` 是硬上限，达到就停，不做无上限重试。
2. 检查结果与上一轮相同视为"无进展"，提前停止，避免空转。
3. 失败与"检查通过"必须区分：确定性检查失败时状态是 `CHECK_BLOCKED`，不是 `DRAFT_READY`。
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass
from typing import Any, Awaitable, Callable

from langgraph.graph import END, START, StateGraph

from app.config import Settings
from app.guard import detect_injection
from app.llm.provider import DraftProvider, DraftRequest
from app.schemas.drafts import (
    Assumption,
    CheckItem,
    DeterministicCheck,
    Draft,
    EvidenceRef,
    TaskSlots,
    TaskStatus,
)
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.tools.scan import feedback_lines
from app.workflow.state import WorkflowState, build_questions, event, slots_from_state

# 允许模型提供的字段。其余字段一律拒绝，避免模型改写服务端已确认的信息。
ALLOWED_MODEL_FIELDS = {
    "sql",
    "rollback_sql",
    "assumptions",
    "open_questions",
    "advisory_risk",
    "advice_summary",
    "evidence_ids",
}

_FENCE = re.compile(r"^\s*```(?:json)?\s*|\s*```\s*$", re.IGNORECASE)


class DraftParseError(Exception):
    """模型输出无法解析成合法草案。"""


def parse_model_draft(
    text: str,
    *,
    requirement: str,
    slots: TaskSlots,
    evidence_pool: list[EvidenceRef],
) -> Draft:
    """严格解析模型输出。

    严格体现在三处：
    - 顶层必须是 JSON 对象；
    - 出现未声明字段即拒绝（不静默忽略）；
    - 引用的证据 ID 必须真实存在，编造引用直接失败。

    服务端已确认的槽位（应用/环境/数据库/时间）**只从 slots 取**，模型无法改写。
    """
    cleaned = _FENCE.sub("", text or "")
    try:
        payload = json.loads(cleaned)
    except json.JSONDecodeError as error:
        raise DraftParseError(f"模型输出不是合法 JSON：{error}") from error
    if not isinstance(payload, dict):
        raise DraftParseError("模型输出的顶层必须是 JSON 对象")

    unknown = sorted(set(payload) - ALLOWED_MODEL_FIELDS)
    if unknown:
        raise DraftParseError(f"模型输出包含未声明字段：{unknown}")

    sql = payload.get("sql")
    if not isinstance(sql, str) or not sql.strip():
        raise DraftParseError("模型输出缺少非空 sql")

    rollback_sql = payload.get("rollback_sql")
    if rollback_sql is not None and not isinstance(rollback_sql, str):
        raise DraftParseError("rollback_sql 必须是字符串")

    known_ids = {item.evidence_id for item in evidence_pool}
    by_id = {item.evidence_id: item for item in evidence_pool}
    cited: list[EvidenceRef] = []
    for evidence_id in payload.get("evidence_ids") or []:
        if evidence_id not in known_ids:
            raise DraftParseError(f"引用了不存在的证据 ID：{evidence_id}")
        cited.append(by_id[evidence_id])

    assumptions: list[Assumption] = []
    for item in payload.get("assumptions") or []:
        if not isinstance(item, dict) or not isinstance(item.get("statement"), str):
            raise DraftParseError("assumptions 的每一项都必须包含 statement 字符串")
        assumptions.append(
            Assumption(
                statement=item["statement"],
                confirmed=bool(item.get("confirmed", False)),
                needs_confirmation=bool(item.get("needs_confirmation", True)),
            )
        )

    open_questions = [str(item) for item in (payload.get("open_questions") or [])]

    advisory_risk = str(payload.get("advisory_risk") or "UNKNOWN").upper()
    if advisory_risk not in {"LOW", "MEDIUM", "HIGH", "UNKNOWN"}:
        raise DraftParseError(f"advisory_risk 取值非法：{advisory_risk}")

    return Draft(
        version=1,
        requirement=requirement,
        application=slots.application or "",
        environment=slots.environment or "",
        database=slots.database,
        planned_at=slots.planned_at,
        planned_at_timezone=slots.planned_at_timezone,
        sql=sql,
        rollback_sql=rollback_sql or "",
        assumptions=assumptions,
        open_questions=open_questions,
        evidence=cited,
        ai_advice={
            "advisory_risk": advisory_risk,
            "summary": str(payload.get("advice_summary") or ""),
            "reasons": [item.statement for item in assumptions],
        },
    )


def _signature(draft: Draft) -> str:
    payload = f"{draft.sql}\x00{draft.rollback_sql}"
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


@dataclass
class WorkflowDeps:
    """工作流依赖。"""

    settings: Settings
    provider: DraftProvider
    trusted_context: TrustedContext
    toolbox_factory: Callable[[str], Toolbox]


class DraftWorkflow:
    """把 LangGraph 图封装成一个可调用的工作流。"""

    def __init__(self, deps: WorkflowDeps) -> None:
        self._deps = deps
        self._graph = self._build()

    def _build(self) -> Any:
        builder = StateGraph(WorkflowState)
        builder.add_node("screen_input", self._screen_input)
        builder.add_node("check_info", self._check_info)
        builder.add_node("retrieve_evidence", self._retrieve_evidence)
        builder.add_node("generate_draft", self._generate_draft)
        builder.add_node("run_check", self._run_check)
        builder.add_node("finalize", self._finalize)

        builder.add_edge(START, "screen_input")
        builder.add_conditional_edges(
            "screen_input",
            self._route_screen,
            {"rejected": END, "continue": "check_info"},
        )
        builder.add_conditional_edges(
            "check_info",
            self._route_entry,
            {"needs_info": END, "continue": "retrieve_evidence"},
        )
        builder.add_edge("retrieve_evidence", "generate_draft")
        builder.add_edge("generate_draft", "run_check")
        builder.add_conditional_edges(
            "run_check",
            self._route_after_check,
            {"revise": "generate_draft", "done": "finalize"},
        )
        builder.add_edge("finalize", END)
        return builder.compile()

    async def run(self, state: WorkflowState) -> WorkflowState:
        return await self._graph.ainvoke(state)

    # -- 路由 --------------------------------------------------------------

    def _route_screen(self, state: WorkflowState) -> str:
        if state.get("status") == TaskStatus.INPUT_REJECTED.value:
            return "rejected"
        return "continue"

    def _route_entry(self, state: WorkflowState) -> str:
        if state.get("status") == TaskStatus.NEEDS_INFO.value:
            return "needs_info"
        return "continue"

    def _route_after_check(self, state: WorkflowState) -> str:
        if state.get("status") == TaskStatus.FAILED.value:
            return "done"
        # 无进展：检查结论与上一轮完全一致，继续修订只会空转。
        if state.get("no_progress"):
            return "done"
        items = (state.get("check") or {}).get("items") or []
        if not items:
            return "done"
        if int(state.get("revisions") or 0) >= int(state.get("max_revisions") or 0):
            return "done"
        return "revise"

    # -- 节点 --------------------------------------------------------------

    async def _screen_input(self, state: WorkflowState) -> dict[str, Any]:
        """入口注入检测。命中即停止，而不是继续生成一份"看起来正常"的草案。"""
        hits = detect_injection(state.get("requirement", ""))
        events = list(state.get("events") or [])
        if hits:
            detail = f"检测到疑似提示注入：{', '.join(hits)}"
            events.append(event("screen_input", detail))
            return {
                "status": TaskStatus.INPUT_REJECTED.value,
                "error": detail,
                "questions": [
                    {
                        "field": "input_rejected",
                        "question": "输入中检测到疑似提示注入，已停止处理。请确认需求文本后重新提交。",
                        "reason": f"命中规则：{', '.join(hits)}",
                        "examples": [],
                    }
                ],
                "events": events,
            }
        events.append(event("screen_input", "输入未发现疑似注入"))
        return {"events": events}

    async def _check_info(self, state: WorkflowState) -> dict[str, Any]:
        slots = slots_from_state(state)
        missing = slots.missing()
        events = list(state.get("events") or [])
        if missing:
            events.append(event("check_info", f"缺少必要信息：{', '.join(missing)}"))
            return {
                "status": TaskStatus.NEEDS_INFO.value,
                "questions": [item.model_dump(mode="json") for item in build_questions(missing)],
                "events": events,
            }
        events.append(event("check_info", "必要信息完整，继续生成草案"))
        return {"status": TaskStatus.RUNNING.value, "questions": [], "events": events}

    async def _retrieve_evidence(self, state: WorkflowState) -> dict[str, Any]:
        slots = slots_from_state(state)
        query = " ".join(part for part in [state.get("requirement", ""), slots.table or "", slots.query_sql or ""] if part)
        registry = self._deps.toolbox_factory(state.get("schema_snapshot", "")).build()

        evidence: list[EvidenceRef] = []
        notes: list[str] = []
        for tool_name, limit in (("search_norms", 4), ("search_historical_changes", 2)):
            result = await registry.call(
                tool_name, {"query": query, "limit": limit}, self._deps.trusted_context
            )
            if not result.ok:
                notes.append(f"{tool_name}：{result.error}")
                continue
            for hit in result.data.get("hits") or []:
                evidence.append(
                    EvidenceRef(
                        evidence_id=str(hit["evidence_id"]),
                        doc_id=str(hit["doc_id"]),
                        title=str(hit["title"]),
                        section=hit.get("section"),
                        version=hit.get("version") or None,
                        snippet=str(hit.get("snippet") or ""),
                        source=str(hit.get("source") or ""),
                        status=str(hit.get("status") or "unknown"),
                        score=float(hit.get("score") or 0.0),
                    )
                )

        events = list(state.get("events") or [])
        events.append(event("retrieve_evidence", f"检索到 {len(evidence)} 条可引用片段"))
        note = "；".join(notes) if notes else None
        if not evidence:
            # 找不到依据必须明说，不能生成虚假引用。
            events.append(event("retrieve_evidence", "没有可引用的规范或案例片段，草案将标注依据不足"))
        return {"evidence": [item.model_dump(mode="json") for item in evidence], "evidence_note": note, "events": events}

    async def _generate_draft(self, state: WorkflowState) -> dict[str, Any]:
        slots = slots_from_state(state)
        pool = [EvidenceRef.model_validate(item) for item in (state.get("evidence") or [])]
        already_generated = bool(state.get("draft_text"))
        revisions = int(state.get("revisions") or 0) + (1 if already_generated else 0)

        request = DraftRequest(
            requirement=state.get("requirement", ""),
            slots=slots,
            schema_snapshot=state.get("schema_snapshot", ""),
            evidence=pool,
            previous_draft=state.get("draft_text"),
            check_feedback=list(state.get("feedback") or []),
            revision=revisions,
        )

        events = list(state.get("events") or [])
        failures: list[str] = []
        attempts = max(1, self._deps.settings.llm_max_attempts)
        for attempt in range(attempts):
            try:
                text = await self._deps.provider.generate(request)
                draft = parse_model_draft(
                    text,
                    requirement=state.get("requirement", ""),
                    slots=slots,
                    evidence_pool=pool,
                )
            except Exception as error:  # noqa: BLE001 - 模型输出不可信，任何异常都要转成明确失败
                failures.append(f"第 {attempt + 1} 次尝试失败：{error}")
                continue

            events.append(event("generate_draft", f"生成草案 v{revisions + 1}"))
            return {
                "draft": draft.model_dump(mode="json"),
                "draft_text": text,
                "draft_signature": _signature(draft),
                "revisions": revisions,
                "status": TaskStatus.RUNNING.value,
                "error": None,
                "events": events,
            }

        message = "；".join(failures) or "草案生成失败"
        events.append(event("generate_draft", f"生成失败：{message}"))
        return {"status": TaskStatus.FAILED.value, "error": message, "revisions": revisions, "events": events}

    async def _run_check(self, state: WorkflowState) -> dict[str, Any]:
        if state.get("status") == TaskStatus.FAILED.value:
            return {}

        draft = Draft.model_validate(state["draft"])
        registry = self._deps.toolbox_factory(state.get("schema_snapshot", "")).build()
        result = await registry.call(
            "scan_sql",
            {"sql": draft.sql, "rollback_sql": draft.rollback_sql},
            self._deps.trusted_context,
        )

        if not result.ok:
            # 工具失败绝不能被当成"检查通过"。
            check = DeterministicCheck(
                status="FAILED",
                source="scan_sql",
                error=result.error or "确定性扫描未执行",
            )
        else:
            check = DeterministicCheck.model_validate(result.data)

        # 确定性结果覆盖进草案；AI 建议字段保持不变，两者不混。
        updated = draft.model_copy(update={"deterministic_check": check})
        feedback = feedback_lines(check)
        previous = list(state.get("previous_feedback") or [])
        no_progress = bool(previous) and previous == feedback

        events = list(state.get("events") or [])
        events.append(event("run_check", f"确定性检查：{check.status}（阻断 {check.blocking_count} 项）"))

        return {
            "draft": updated.model_dump(mode="json"),
            "check": check.model_dump(mode="json"),
            "feedback": feedback,
            "previous_feedback": feedback,
            "no_progress": no_progress,
            "events": events,
        }

    async def _finalize(self, state: WorkflowState) -> dict[str, Any]:
        if state.get("status") == TaskStatus.FAILED.value:
            return {}

        draft = Draft.model_validate(state["draft"])
        check = draft.deterministic_check

        unresolved = [item.title for item in check.items if item.blocking]
        if unresolved:
            draft = draft.model_copy(update={"open_questions": list(draft.open_questions) + unresolved})
        if state.get("evidence_note"):
            draft = draft.model_copy(
                update={"open_questions": list(draft.open_questions) + [f"依据检索不完整：{state['evidence_note']}"]}
            )

        if check.status in {"BLOCKED", "FAILED"}:
            status = TaskStatus.CHECK_BLOCKED.value
        else:
            status = TaskStatus.DRAFT_READY.value

        questions = build_questions([], draft.open_questions)
        events = list(state.get("events") or [])
        events.append(event("finalize", f"输出草案 v{draft.version}，状态 {status}"))

        return {
            "draft": draft.model_dump(mode="json"),
            "status": status,
            "questions": [item.model_dump(mode="json") for item in questions],
            "events": events,
        }


def new_check_item(code: str, title: str) -> CheckItem:  # pragma: no cover - 便于扩展的占位
    return CheckItem(code=code, title=title)
