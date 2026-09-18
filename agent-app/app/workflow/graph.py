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

import json
import re
from dataclasses import dataclass
from typing import Any, Awaitable, Callable

from langgraph.graph import END, START, StateGraph
from langgraph.types import Command, interrupt

from app.budget import TaskBudgetExceeded
from app.config import Settings
from app.guard import detect_injection
from app.llm.provider import DraftProvider, DraftRequest, ModelCallError
from app.schemas.drafts import (
    Assumption,
    CheckItem,
    DatabaseKind,
    DeterministicCheck,
    Draft,
    EvidenceRef,
    TaskSlots,
    TaskStatus,
)
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.tools.scan import feedback_lines
from app.workflow.investigate import (
    BoundedInvestigation,
    PlannerUnavailable,
    StopReason,
    build_planner,
    evidence_from_tool_result,
)
from app.workflow.state import (
    WorkflowState,
    build_questions,
    event,
    material_hash,
    merge_slot_data,
    slots_from_state,
)

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

# 每条假设允许模型提供的字段。`confirmed` **不在**其中：
# "已获人工确认"是人的结论，模型无权声明（见下方解析逻辑）。
ALLOWED_ASSUMPTION_FIELDS = {"statement", "needs_confirmation"}

_FENCE = re.compile(r"^\s*```(?:json)?\s*|\s*```\s*$", re.IGNORECASE)

# 同一个节点内连续中断的次数上限：用户反复补充仍不完整时不再无限等待，
# 而是返回 NEEDS_INFO 终态（由路由结束），避免图永远停在同一节点。
_MAX_INFO_INTERRUPTS = 4


class DraftParseError(Exception):
    """模型输出无法解析成合法草案。"""


def parse_model_draft(
    text: str,
    *,
    requirement: str,
    slots: TaskSlots,
    evidence_pool: list[EvidenceRef],
    version: int = 1,
    revision_notes: list[str] | None = None,
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
        unknown_assumption = sorted(set(item) - ALLOWED_ASSUMPTION_FIELDS)
        if unknown_assumption:
            # 其中 `confirmed` 尤其不能由模型提供：它表示"已获人工确认"，
            # 是人的结论，不是模型的结论。按本解析器一贯的严格口径直接拒绝，
            # 而不是悄悄忽略——被忽略的确认标记最容易变成一条虚假的审计记录。
            raise DraftParseError(f"assumptions 包含未声明字段：{unknown_assumption}")
        assumptions.append(
            Assumption(
                statement=item["statement"],
                # 两个标志都由服务端决定，不采信模型：
                #   confirmed 恒为 False——模型无权声称已获人工确认；
                #   needs_confirmation 恒为 True——未经确认的假设就需要确认。
                # 模型送来的 needs_confirmation 在契约内（不报错），但不作为依据。
                confirmed=False,
                needs_confirmation=True,
            )
        )

    open_questions = [str(item) for item in (payload.get("open_questions") or [])]

    advisory_risk = str(payload.get("advisory_risk") or "UNKNOWN").upper()
    if advisory_risk not in {"LOW", "MEDIUM", "HIGH", "UNKNOWN"}:
        raise DraftParseError(f"advisory_risk 取值非法：{advisory_risk}")

    return Draft(
        # 版本与修订说明由服务端写入，模型无从决定：模型无法把自己标成"第 1 版"
        # 来掩盖"这已经是第三轮修订"。
        version=max(1, int(version)),
        revision_notes=list(revision_notes or []),
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
    return material_hash(draft.sql, draft.rollback_sql)


@dataclass
class WorkflowDeps:
    """工作流依赖。"""

    settings: Settings
    provider: DraftProvider
    trusted_context: TrustedContext
    toolbox_factory: Callable[[str], Toolbox]
    # 持久化检查点。为 None 时图不可中断，`check_info` 退回"走到 END + NEEDS_INFO"的既有行为。
    checkpointer: Any | None = None


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
            {"needs_info": END, "unsupported": END, "continue": "retrieve_evidence"},
        )
        builder.add_conditional_edges(
            "retrieve_evidence",
            self._route_after_evidence,
            {"blocked": END, "continue": "generate_draft"},
        )
        builder.add_edge("generate_draft", "run_check")
        builder.add_conditional_edges(
            "run_check",
            self._route_after_check,
            {"revise": "generate_draft", "done": "finalize"},
        )
        builder.add_edge("finalize", END)
        return builder.compile(checkpointer=self._deps.checkpointer)

    def _config(self, thread_id: str) -> dict[str, Any]:
        """检查点按 `thread_id` 分区；这里用 task_id，使恢复指向同一条线程。"""
        return {"configurable": {"thread_id": thread_id}}

    async def run(self, state: WorkflowState, *, thread_id: str | None = None) -> WorkflowState:
        # 没有检查点的调用方不需要 thread_id（config 会被忽略）；有检查点时由服务层显式传入。
        resolved = thread_id or str(state.get("task_id") or "adhoc")
        return await self._graph.ainvoke(state, self._config(resolved))

    async def resume_interrupt(self, *, thread_id: str, value: Any) -> WorkflowState:
        """从节点级中断恢复：`interrupt()` 返回 `value`，图从该节点继续。"""
        return await self._graph.ainvoke(Command(resume=value), self._config(thread_id))

    async def continue_pending(self, *, thread_id: str) -> WorkflowState:
        """续跑最后未完成的节点（没有中断，只是上一个执行失败或进程中断）。"""
        return await self._graph.ainvoke(None, self._config(thread_id))

    async def snapshot(self, *, thread_id: str) -> Any:
        """读取检查点状态（恢复前校验输入版本、判断是否有待恢复的工作）。"""
        return await self._graph.aget_state(self._config(thread_id))

    # -- 路由 --------------------------------------------------------------

    def _route_screen(self, state: WorkflowState) -> str:
        if state.get("status") == TaskStatus.INPUT_REJECTED.value:
            return "rejected"
        return "continue"

    def _route_entry(self, state: WorkflowState) -> str:
        if state.get("status") == TaskStatus.NEEDS_INFO.value:
            return "needs_info"
        if state.get("status") == TaskStatus.FAILED.value:
            # 入口阶段就已确定无法继续（例如目标数据库不在支持范围内）：直接结束，
            # 不要继续走到检索与生成——那正是"给出方言不匹配的草案"的路径。
            return "unsupported"
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
        events = list(state.get("events") or [])
        schema_snapshot = state.get("schema_snapshot", "")
        requirement = state.get("requirement", "")
        version = state.get("input_version", "")

        # 有检查点时走**节点级中断**：图停在 check_info 并落检查点，用户补充后从本节点继续，
        # 之前的节点（screen_input）不会重跑。没有检查点时无法 interrupt，
        # 退回"走到 END + NEEDS_INFO"的既有行为（保持既有调用方与测试不变）。
        for _ in range(_MAX_INFO_INTERRUPTS):
            missing = slots.missing()
            if not missing:
                break
            if self._deps.checkpointer is None:
                events.append(event("check_info", f"缺少必要信息：{', '.join(missing)}"))
                return {
                    "status": TaskStatus.NEEDS_INFO.value,
                    "questions": [item.model_dump(mode="json") for item in build_questions(missing)],
                    "events": events,
                }
            questions = [item.model_dump(mode="json") for item in build_questions(missing)]
            # 中断前追加的事件不会被提交（节点没有返回），因此不会重复记录。
            events.append(event("check_info", f"缺少必要信息：{', '.join(missing)}"))
            provided = interrupt({"kind": "needs_info", "questions": questions})
            provided = provided if isinstance(provided, dict) else {}
            slots = merge_slot_data(slots, provided.get("slots") or {})
            # 恢复值由服务端给出**完整**的当前输入（而非增量），因此重复恢复是幂等的：
            # 需求、快照与输入版本直接覆盖，不会把同一条补充说明拼接两次。
            if provided.get("schema_snapshot") is not None:
                schema_snapshot = str(provided.get("schema_snapshot") or "")
            if provided.get("requirement") is not None:
                requirement = str(provided.get("requirement"))
            if provided.get("input_version") is not None:
                version = str(provided.get("input_version"))
            events.append(event("resumed", "用户补充信息后从等待点继续"))
        else:
            # 反复补充仍不完整：不静默继续，也不无限等待。
            missing = slots.missing()
            events.append(event("check_info", f"补充后仍缺少必要信息：{', '.join(missing)}"))
            return {
                "status": TaskStatus.NEEDS_INFO.value,
                "questions": [item.model_dump(mode="json") for item in build_questions(missing)],
                "slots": slots.model_dump(mode="json"),
                "events": events,
            }

        # 支持范围必须显式声明，并在入口就停下。
        # 给 MySQL 输出 PostgreSQL 专属语法（CREATE INDEX CONCURRENTLY、SET lock_timeout）
        # 是比直接拒绝更糟的结果：它看起来像一份可用的草案，而实际执行不了。
        if slots.database not in (None, DatabaseKind.POSTGRESQL):
            detail = (
                f"本版本仅支持 PostgreSQL 索引变更，暂不支持 {slots.database.value}；"
                "为避免给出与该方言不匹配的草案，任务停在此处而不是继续生成。"
            )
            events.append(event("check_info", detail))
            return {
                "status": TaskStatus.FAILED.value,
                "error": detail,
                "questions": [],
                "events": events,
            }
        events.append(event("check_info", "必要信息完整，继续生成草案"))
        return {
            "status": TaskStatus.RUNNING.value,
            "questions": [],
            "slots": slots.model_dump(mode="json"),
            "schema_snapshot": schema_snapshot,
            "requirement": requirement,
            "input_version": version,
            "events": events,
        }

    async def _retrieve_evidence(self, state: WorkflowState) -> dict[str, Any]:
        slots = slots_from_state(state)
        query = " ".join(part for part in [state.get("requirement", ""), slots.table or "", slots.query_sql or ""] if part)
        registry = self._deps.toolbox_factory(state.get("schema_snapshot", "")).build()

        strategy = str(getattr(self._deps.settings, "investigation_strategy", "fixed_workflow") or "").lower()
        if strategy == "bounded_agent":
            evidence, notes, investigation, trace = await self._investigate(state, slots, registry)
        else:
            evidence, notes = await self._fixed_retrieval(query, registry)
            investigation = {"strategy": "fixed_workflow", "planner": "fixed", "blocked": False}
            trace = ""

        events = list(state.get("events") or [])
        events.append(event("retrieve_evidence", f"检索到 {len(evidence)} 条可引用片段"))
        if trace:
            # 决策者是谁、为什么停下、用了几次工具，都必须可见——不含模型内部推理。
            events.append(event("retrieve_evidence", trace))
        note = "；".join(notes) if notes else None
        if not evidence:
            # 找不到依据必须明说，不能生成虚假引用。
            events.append(event("retrieve_evidence", "没有可引用的规范或案例片段，草案将标注依据不足"))

        payload: dict[str, Any] = {
            "evidence": [item.model_dump(mode="json") for item in evidence],
            "evidence_note": note,
            "events": events,
            "investigation": investigation,
        }
        if investigation.get("blocked"):
            # 调查结论**真正控制工作流**：由条件路由把状态定成终态并跳过草稿生成，
            # 而不是只写一句"循环停止了"然后照常产出草案。
            payload["status"] = investigation["status"]
            if investigation["status"] == TaskStatus.FAILED.value:
                # NEEDS_INFO 是"等待用户补充"，不是错误；把 error 留空以免界面把它显示成失败。
                payload["error"] = investigation.get("block_reason")
            if investigation.get("questions"):
                payload["questions"] = investigation["questions"]
            events.append(event("retrieve_evidence", f"调查未通过：{investigation.get('block_reason')}"))
        return payload

    async def _fixed_retrieval(self, query: str, registry: Any) -> tuple[list[EvidenceRef], list[str]]:
        """现有的固定检索顺序。保留为默认策略与回退路径。"""
        evidence: list[EvidenceRef] = []
        notes: list[str] = []
        for tool_name, limit in (("search_norms", 4), ("search_historical_changes", 2)):
            result = await registry.call(
                tool_name, {"query": query, "limit": limit}, self._deps.trusted_context
            )
            if not result.ok:
                notes.append(f"{tool_name}：{result.error}")
                continue
            evidence.extend(evidence_from_tool_result(result))
        return evidence, notes

    async def _investigate(
        self, state: WorkflowState, slots: TaskSlots, registry: Any
    ) -> tuple[list[EvidenceRef], list[str], dict[str, Any], str]:
        """走受约束调查循环，并把结论整理成**结构化状态**供路由使用。"""
        try:
            # 只把**服务端**的只读工具规格交给决策者：模型能选的工具集合不来自模型本身。
            planner = build_planner(self._deps.settings, self._deps.provider, registry.specs())
        except PlannerUnavailable as error:
            # 明确失败：既不能悄悄继续生成，也不能用规则顶替并说成"模型的选择"。
            reason = f"调查循环未启动：{error}"
            return (
                [],
                [str(error)],
                {
                    "strategy": "bounded_agent",
                    "planner": "unavailable",
                    "stop_reason": StopReason.PLANNER_UNAVAILABLE.value,
                    "blocked": True,
                    "status": TaskStatus.FAILED.value,
                    "block_reason": reason,
                    "questions": [],
                },
                # 决策者的身份要留在轨迹里：unavailable 就是 unavailable，不写成 rule。
                f"调查循环：planner=unavailable；stop={StopReason.PLANNER_UNAVAILABLE.value}；{reason}",
            )

        loop = BoundedInvestigation(
            planner=planner,
            registry=registry,
            context=self._deps.trusted_context,
            max_rounds=self._deps.settings.max_investigation_rounds,
            max_total_tool_calls=self._deps.settings.max_total_tool_calls,
            tool_timeout_seconds=self._deps.settings.tool_timeout_seconds,
        )
        outcome = await loop.run(
            requirement=state.get("requirement", ""),
            slots=slots,
            schema_snapshot=state.get("schema_snapshot", ""),
        )
        report = outcome.report
        notes = list(report.notes)
        investigation: dict[str, Any] = {
            "strategy": "bounded_agent",
            "planner": report.planner,
            "stop_reason": report.stop_reason,
            "rounds": report.rounds,
            "tool_calls": report.tool_calls,
            "missing_required": list(report.missing_required),
            "blocked": False,
            # 预算与工具结果摘要：工作台要展示"实际用了什么、为什么停下"。
            # 摘要本身已经是有界截断，且属于不可信数据，展示时按纯文本转义。
            "usage": {
                "known": report.usage.known,
                "prompt_tokens": report.usage.prompt_tokens,
                "completion_tokens": report.usage.completion_tokens,
                "cost_estimate": report.usage.cost_estimate,
                "note": report.usage.note,
            },
            "tool_observations": [
                {
                    "tool": item.tool,
                    "ok": item.ok,
                    "kind": item.kind,
                    "summary": item.summary,
                    "error": item.error,
                    "data_version": item.data_version,
                    "evidence_ids": list(item.evidence_ids),
                }
                for item in report.observations
            ],
        }

        if report.clarification_requests:
            # 决策者要求补充信息 → 进入 NEEDS_INFO，用户补充后可继续推进。
            investigation.update(
                {
                    "blocked": True,
                    "status": TaskStatus.NEEDS_INFO.value,
                    "block_reason": "调查需要补充信息：" + "；".join(report.clarification_requests),
                    "questions": [
                        item.model_dump(mode="json")
                        for item in build_questions([], list(report.clarification_requests))
                    ],
                }
            )
        elif report.stop_reason == StopReason.BUDGET_EXHAUSTED.value:
            # 预算用尽必须**终止**，而不是继续生成：继续只会再打一次模型并同样被拒。
            investigation.update(
                {
                    "blocked": True,
                    "status": TaskStatus.FAILED.value,
                    "block_reason": "任务 token 预算已用尽，调查已停止；请提高预算或缩小范围后重试",
                    "questions": [],
                }
            )
            notes.append("任务 token 预算用尽，未继续生成草案")
        elif report.missing_required:
            # 必需证据缺失不得进入草稿生成——那会产出一份看起来可用的草案。
            investigation.update(
                {
                    "blocked": True,
                    "status": TaskStatus.FAILED.value,
                    "block_reason": "必需证据缺失，未生成草案：" + "、".join(report.missing_required),
                    "questions": [],
                }
            )
            notes.append("必需证据仍缺失：" + "、".join(report.missing_required))
        return outcome.evidence, notes, investigation, f"调查循环：{report.summary()}"

    def _route_after_evidence(self, state: WorkflowState) -> str:
        investigation = state.get("investigation") or {}
        if investigation.get("blocked"):
            return "blocked"
        return "continue"

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
        # 内容层的重试与 provider 的传输层重试用**两个不同的开关**：
        # 以前两层共用 `llm_max_attempts`，于是一次生成最多打 2×2=4 次模型调用，
        # 成本与延迟被悄悄放大，而且无法单独调整任何一层。
        attempts = max(1, int(self._deps.settings.draft_parse_attempts))
        for attempt in range(attempts):
            try:
                text = await self._deps.provider.generate(request)
                draft = parse_model_draft(
                    text,
                    requirement=state.get("requirement", ""),
                    slots=slots,
                    evidence_pool=pool,
                    # 版本与修订说明来自服务端：本轮修订对应的是**上一轮确定性检查的反馈**，
                    # 而不是模型自己声称改了什么。
                    version=revisions + 1,
                    revision_notes=list(request.check_feedback),
                )
            except ModelCallError as error:
                # provider 已经用尽**它自己的**传输重试预算。工作流的解析重试只针对
                # "拿到了文本但内容不合格"，因此这里立即结束本次生成，不再消耗解析次数。
                # 之前这里写的是 continue，等于把传输重试又乘了一遍：
                # llm_max_attempts=2、draft_parse_attempts=3 时实测发出 6 次 HTTP 请求。
                failures.append(
                    f"模型调用失败（类型={error.failure_type}，已发出 {error.requests_sent} 次请求）：{error}"
                )
                break
            except TaskBudgetExceeded as error:
                # 预算用尽：不再重试、也不再消耗解析次数，直接如实失败。
                failures.append(f"任务 token 预算已用尽，停止生成：{error}")
                break
            except DraftParseError as error:
                # 只有这一种失败才消耗解析重试：确实拿到了文本，但解析不出合格草案。
                failures.append(f"第 {attempt + 1} 次尝试失败（解析）：{error}")
                continue
            except Exception as error:  # noqa: BLE001 - 未知异常不做重试，避免掩盖真实原因并放大成本
                failures.append(f"第 {attempt + 1} 次尝试失败（未知）：{type(error).__name__}: {error}")
                break

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
