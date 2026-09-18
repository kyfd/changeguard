"""受约束的调查循环。

与"让模型自由跑"的区别在于：**边界由代码决定，不由提示词决定**。
提示词里写"最多查 3 次"，模型可以不遵守；这里的上限由循环自己强制。

本模块只做循环的骨架与判定，不假定决策者是模型：

- `InvestigationPlanner` 是决策契约，返回的动作限于下面四种之一；
- `RulePlanner` 是**确定性的规则决策者**，用于没有模型时也能跑通（它明确不叫"模型决策"）；
- `ProviderPlanner` 要求 provider 具备原生动作能力；不具备时**显式报告不可用**，
  而不是把脚本包装成"模型的决定"；
- `ScriptedPlanner` 供测试构造确定的动作序列。

四条硬边界：

1. 轮次上限；
2. 累计工具调用上限（跨轮累计，不只是每轮）；
3. 单工具超时——超时按失败处理，不让循环挂住；
4. **空转检测**：相同工具 + 相同参数再次出现即视为无进展，立刻停止。

以及一条确定性完成条件：`required_evidence_missing` 由代码判定"证据是否够用"，
**模型不能自行宣布证据已经足够**。
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Mapping, Protocol

from app.budget import PHASE_INVESTIGATE, TaskBudgetExceeded, UsageLedger, usage_scope
from app.schemas.drafts import EvidenceRef, TaskSlots, ToolResult

# 引用片段里只保留这些前缀的文档作为"规范"，与检索层的作用域一致。
NORM_PREFIX = "norms/"

# 结构化观察的摘要长度上限。工具结果可能很大，但反馈给下一轮决策必须是**有界**的，
# 同时保留可追溯标识（内容摘要哈希 + 版本/时间），不能只截断成无法核对的文本。
OBSERVATION_SUMMARY_CHARS = 400


class StopReason(str, Enum):
    """循环为什么停下。每一种都必须能被外部看到，不能含糊成"结束了"。"""

    EVIDENCE_SUFFICIENT = "EVIDENCE_SUFFICIENT"
    INSUFFICIENT_EVIDENCE = "INSUFFICIENT_EVIDENCE"
    ROUNDS_EXHAUSTED = "ROUNDS_EXHAUSTED"
    TOOL_CALLS_EXHAUSTED = "TOOL_CALLS_EXHAUSTED"
    NO_PROGRESS = "NO_PROGRESS"
    PLANNER_UNAVAILABLE = "PLANNER_UNAVAILABLE"
    # 决策者存在且具备动作能力，但这一次没能给出可执行的动作（未知工具、参数非法、
    # 试图改写身份、模型调用失败等）。与 PLANNER_UNAVAILABLE（根本没有动作能力）区分开。
    PLANNER_FAILED = "PLANNER_FAILED"
    TOOL_FAILED = "TOOL_FAILED"
    # 任务级 token 预算用尽：不是故障，是按预算主动停止。
    BUDGET_EXHAUSTED = "BUDGET_EXHAUSTED"


@dataclass(frozen=True)
class CallTool:
    """调用一个只读工具。"""

    tool: str
    args: dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class Finish:
    """认为调查完成，可以进入草案生成。是否真的够了由 RequiredEvidence 判定。"""


@dataclass(frozen=True)
class AskUser:
    """证据不足，需要用户补充信息。"""

    reason: str


InvestigationAction = CallTool | Finish | AskUser


class InvestigationPlanner(Protocol):
    """决策契约。实现方必须能说明自己是谁——不要把规则包装成模型。

    `plan` 是**异步**的：模型驱动的决策者需要发起网络调用，规则与脚本决策者则立即返回。
    循环只 `await` 它，不关心背后是模型还是规则。
    """

    name: str

    async def plan(
        self,
        *,
        requirement: str,
        slots: TaskSlots,
        evidence: list[EvidenceRef],
        called_tools: list[str],
        round_index: int,
        observations: list["ToolObservation"] | None = None,
    ) -> InvestigationAction:
        ...


# 目标库 → 适用范围文本里可能出现的写法。刻意保守：只认明确写出的库名。
_DATABASE_ALIASES: dict[str, tuple[str, ...]] = {
    "postgresql": ("postgresql", "postgres", "pg"),
    "mysql": ("mysql", "mariadb"),
}

# 只认 CREATE TABLE：这是**保守**解析。解析不出任何表时，调用方必须显式判为"无法核对"，
# 而不是用脆弱的字符串匹配宣称已经完成 SQL 语义校验。
_TABLE_DEF = re.compile(r"create\s+table\s+(?:if\s+not\s+exists\s+)?[\"`\[]?([a-zA-Z_][\w.]*)", re.IGNORECASE)


def _applicable_to(applicability: str, target_database: str) -> bool:
    """规范的适用范围是否覆盖目标数据库。

    **没写适用范围 = 无法核对 = 不算适用**（失败关闭）：把"没写"当成"通用"，
    正是"unknown 默认等于有效"的那种错误。
    """
    text = (applicability or "").strip().lower()
    target = (target_database or "").strip().lower()
    if not text or not target:
        return False
    return any(alias in text for alias in _DATABASE_ALIASES.get(target, (target,)))


def _snapshot_tables(snapshot: str) -> set[str]:
    """从快照里解析出的表名（小写、去掉 schema 限定）。"""
    return {
        match.group(1).split(".")[-1].strip("\"`[]").lower()
        for match in _TABLE_DEF.finditer(snapshot or "")
    }


class RequiredEvidence:
    """"什么算证据够用"的确定性判定。

    刻意不由模型回答"我觉得够了"：完成条件必须是可复核的代码。
    判定覆盖三类，而不只是"有没有 norms/ 前缀"：

    1. **规范证据**：至少一条**未被废弃**的规范片段。已废弃的规范能命中前缀，
       但引用它就是引用失效条款——仅凭前缀判定会把这种情况判成"够了"。
    2. **可核对身份**：规范片段必须带版本或来源，否则无法说明依据的是哪一版。
    3. **目标材料**：指定了目标表却没有表结构快照时无法核对字段名，
       此时生成草案就是在编造列名。
    """

    def missing(
        self,
        evidence: list[EvidenceRef],
        slots: TaskSlots,
        schema_snapshot: str = "",
    ) -> list[str]:
        missing: list[str] = []
        norm_evidence = [item for item in evidence if item.doc_id.startswith(NORM_PREFIX)]
        active_norms = [item for item in norm_evidence if item.status == "active"]

        if not active_norms:
            if any(item.status == "deprecated" for item in norm_evidence):
                missing.append("至少一条仍生效的规范片段（当前命中的规范已标记为废弃）")
            elif any(item.status == "unknown" for item in norm_evidence):
                # 状态未知**不等于**有效：必须显式说清，不能默默当成可用证据。
                missing.append("至少一条状态可确认生效的规范片段（命中的规范状态未知，未知不等于有效）")
            else:
                missing.append("至少一条规范片段（norms/）")
        else:
            # 可核对身份。
            identified = [item for item in active_norms if (item.version or item.source or "").strip()]
            if not identified:
                missing.append("规范片段的版本或来源（用于说明依据的是哪一版）")
            else:
                # 适用性：规范的适用范围必须覆盖目标数据库；没写适用范围就是无法核对。
                target = (slots.database.value if slots.database else "") or ""
                if not any(_applicable_to(item.applicability, target) for item in identified):
                    missing.append(
                        "适用当前目标数据库的规范片段：命中的规范适用范围不匹配或未标注适用范围"
                        + (f"（当前目标：{target}）" if target else "（当前目标数据库未指定）")
                    )

        # 目标材料：不能只看"快照非空"，要核对快照里确实有目标表。
        table = (slots.table or "").strip()
        if table:
            if not schema_snapshot.strip():
                missing.append("目标表的结构快照（缺少它无法核对字段名）")
            else:
                tables = _snapshot_tables(schema_snapshot)
                if not tables:
                    missing.append("表结构快照中未能识别出任何表定义，无法核对目标表")
                elif table.lower() not in tables:
                    missing.append(
                        f"表结构快照中未找到目标表 {table!r}（快照中的表：{', '.join(sorted(tables))}）"
                    )
        return missing


@dataclass
class ToolObservation:
    """一次工具调用的结构化观察。

    只把搜索 hits 反馈给下一轮是不够的：表结构快照、变更上下文、扫描结果同样影响
    下一步决策，丢掉它们等于让决策者"看不见"已经拿到的事实。

    同时必须**有界可追溯**：摘要截断，但保留证据标识、数据版本/时间与内容摘要哈希，
    这样截断不会让结果变得无法核对。
    """

    tool: str
    ok: bool
    args_digest: str
    payload_digest: str = ""
    summary: str = ""
    error: str = ""
    evidence_ids: list[str] = field(default_factory=list)
    data_version: str = ""
    observed_at: str = ""
    kind: str = "generic"


@dataclass
class UsageBudget:
    """调查部分的 token 用量：**按请求累计**，缺失即显式未知。

    **usage 缺失时必须标 unknown，不得填 0**：0 是一个"确定没有消耗"的断言，
    而缺失只是"不知道"。两者混同会让成本报告看起来精确，实际是编的。
    """

    requests: int = 0
    reported: int = 0
    prompt_tokens: int | None = None
    completion_tokens: int | None = None
    cost_estimate: float | None = None
    note: str = "usage 未由 provider 提供：token 与费用标记为未知，不做估算"

    @property
    def known(self) -> bool:
        """只有**每一次**响应都提供了 usage，总量才算已知。"""
        return self.requests > 0 and self.reported == self.requests

    @property
    def missing(self) -> int:
        return self.requests - self.reported

    def add_usage(self, usage: Mapping[str, Any] | None) -> None:
        self.requests += 1
        if not isinstance(usage, Mapping):
            self.note = f"{self.missing} 次模型响应未提供 usage：总量不完整，不填 0 冒充已知"
            return
        self.reported += 1
        self.prompt_tokens = int(self.prompt_tokens or 0) + int(usage.get("prompt_tokens") or 0)
        self.completion_tokens = int(self.completion_tokens or 0) + int(usage.get("completion_tokens") or 0)
        self.note = (
            "usage 由 provider 按请求累计"
            if self.known
            else f"{self.missing} 次模型响应未提供 usage：总量不完整，不填 0 冒充已知"
        )

    def merge(self, ledger: UsageLedger) -> None:
        """把一段（例如某一轮）的记账并入本预算。"""
        self.requests += ledger.requests
        self.reported += ledger.reported
        if ledger.reported:
            self.prompt_tokens = int(self.prompt_tokens or 0) + ledger.prompt_tokens
            self.completion_tokens = int(self.completion_tokens or 0) + ledger.completion_tokens
        if self.known:
            self.note = "usage 由 provider 按请求累计"
        elif self.requests:
            self.note = f"{self.missing} 次模型响应未提供 usage：总量不完整，不填 0 冒充已知"


def digest_payload(payload: Any) -> str:
    """结构化结果的稳定性摘要，便于跨轮核对"是不是同一份内容"。"""
    try:
        canonical = json.dumps(payload, sort_keys=True, ensure_ascii=False, default=str)
    except (TypeError, ValueError):
        canonical = str(payload)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()[:16]


def summarize_payload(payload: Any) -> str:
    """截断成有界摘要。截断的是展示，不是可追溯性——摘要哈希另行保留。"""
    try:
        text = json.dumps(payload, ensure_ascii=False, default=str)
    except (TypeError, ValueError):
        text = str(payload)
    text = " ".join(text.split())
    return text if len(text) <= OBSERVATION_SUMMARY_CHARS else text[:OBSERVATION_SUMMARY_CHARS] + "…"


@dataclass
class InvestigationReport:
    """循环做过的每一步都要可见，便于复盘。不含模型内部推理。"""

    planner: str
    stop_reason: str
    rounds: int = 0
    tool_calls: int = 0
    called_tools: list[str] = field(default_factory=list)
    missing_required: list[str] = field(default_factory=list)
    # 决策者要求用户补充信息的原文。必须结构化带出来，供工作流转成追问；
    # 塞进 notes 再靠解析字符串取回是不可靠的。
    clarification_requests: list[str] = field(default_factory=list)
    # 每一次工具调用的结构化观察（含失败），用于反馈下一轮决策与事后复盘。
    observations: list[ToolObservation] = field(default_factory=list)
    usage: UsageBudget = field(default_factory=UsageBudget)
    notes: list[str] = field(default_factory=list)

    def summary(self) -> str:
        parts = [f"planner={self.planner}", f"stop={self.stop_reason}", f"轮次={self.rounds}", f"工具调用={self.tool_calls}"]
        if self.missing_required:
            parts.append("缺证据：" + "、".join(self.missing_required))
        if not self.usage.known:
            # 预算报告里必须显式写出"未知"，而不是让读者以为消耗为 0。
            parts.append("usage=unknown")
        return "；".join(parts)


@dataclass
class InvestigationOutcome:
    evidence: list[EvidenceRef]
    report: InvestigationReport


# ---------------------------------------------------------------------------
# 决策者实现
# ---------------------------------------------------------------------------


class PlannerUnavailable(Exception):
    """决策者不具备所需能力。必须显式失败，不能用规则顶替并宣称是模型决策。"""


class PlannerDecisionError(Exception):
    """决策者具备动作能力，但这次没有给出可执行的动作。

    典型来源：模型选了白名单之外的工具、参数不符合 schema、参数里夹带身份字段，
    或模型调用本身失败。这些都不允许被悄悄降级成"跳过这一步继续生成草案"，
    必须变成可见的停止原因。
    """


class ProviderPlanner:
    """把模型的原生动作能力接进循环。

    关键约束：provider 必须**明确声明**支持动作决策（实现 `decide()`）。
    不具备时这里显式报告不可用，而不是拿规则去冒充"模型的选择"。

    `tools` 是**服务端**给出的只读工具规格（来自工具注册表）。它只用于两件事：
    告诉模型哪些只读工具可用、以及校验模型给出的参数。模型无从新增工具或改写身份。
    """

    name = "provider"

    def __init__(self, provider: Any, tools: list[dict[str, Any]] | None = None) -> None:
        if not hasattr(provider, "decide"):
            raise PlannerUnavailable(
                f"provider {getattr(provider, 'name', type(provider).__name__)} 未实现 decide()，"
                "无法进行模型驱动的工具选择；请显式选择 RulePlanner，或为该 provider 实现动作契约。"
            )
        self._provider = provider
        self._tools = list(tools or [])
        # 由 provider 报告的 token 用量；缺失时保持 None，循环据此标为 unknown 而不是 0。
        self.last_usage: Any = None

    async def plan(self, **kwargs: Any) -> InvestigationAction:
        try:
            action = await self._provider.decide(tools=list(self._tools), **kwargs)
        except TaskBudgetExceeded:
            # 预算用尽必须原样上抛：它是"按预算停止"，不是决策者坏了。
            raise
        except Exception as error:  # noqa: BLE001 - 决策失败必须变成可见的停止原因，不能让循环崩掉
            raise PlannerDecisionError(f"{type(error).__name__}: {error}") from error
        self.last_usage = getattr(self._provider, "last_usage", None)
        return action


class RulePlanner:
    """确定性的规则决策者。

    它不是模型的替代品，而是**没有模型时也能把闭环跑通**的基线；因此它的名字与
    报告里的 `planner` 字段都明确写 "rule"，不写 "model"。
    """

    name = "rule"

    def __init__(self, steps: list[tuple[str, dict[str, Any]]] | None = None) -> None:
        self._steps = steps or [
            ("search_norms", {"query": "{requirement}", "limit": 4}),
            ("search_historical_changes", {"query": "{requirement}", "limit": 2}),
        ]

    async def plan(
        self,
        *,
        requirement: str,
        slots: TaskSlots,
        evidence: list[EvidenceRef],
        called_tools: list[str],
        round_index: int,
        observations: list[ToolObservation] | None = None,
    ) -> InvestigationAction:
        if round_index > len(self._steps):
            return Finish()
        tool, args = self._steps[round_index - 1]
        rendered = {
            key: (value.replace("{requirement}", requirement) if isinstance(value, str) else value)
            for key, value in args.items()
        }
        if tool == "search_norms":
            rendered["query"] = " ".join(
                part for part in [rendered.get("query", ""), slots.table or "", slots.query_sql or ""] if part
            )
        return CallTool(tool=tool, args=rendered)


class ScriptedPlanner:
    """按脚本返回动作，供测试构造确定的序列。"""

    name = "scripted"

    def __init__(self, actions: list[InvestigationAction]) -> None:
        self._actions = list(actions)
        self.seen_rounds: list[int] = []

    async def plan(self, *, round_index: int, **_kwargs: Any) -> InvestigationAction:
        self.seen_rounds.append(round_index)
        if round_index > len(self._actions):
            return Finish()
        return self._actions[round_index - 1]


# ---------------------------------------------------------------------------
# 循环
# ---------------------------------------------------------------------------


def evidence_from_tool_result(result: ToolResult) -> list[EvidenceRef]:
    """把工具结果里的命中转成可引用片段。"""
    collected: list[EvidenceRef] = []
    for hit in (result.data or {}).get("hits") or []:
        collected.append(
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
                applicability=str(hit.get("applicability") or ""),
            )
        )
    return collected


class BoundedInvestigation:
    """受约束的调查循环。"""

    def __init__(
        self,
        *,
        planner: InvestigationPlanner,
        registry: Any,
        context: Any,
        max_rounds: int,
        max_total_tool_calls: int,
        tool_timeout_seconds: float,
    ) -> None:
        self._planner = planner
        self._registry = registry
        self._context = context
        self._max_rounds = max(1, int(max_rounds))
        self._max_tool_calls = max(0, int(max_total_tool_calls))
        self._tool_timeout = float(tool_timeout_seconds)
        self._required = RequiredEvidence()

    async def run(
        self,
        *,
        requirement: str,
        slots: TaskSlots,
        schema_snapshot: str = "",
    ) -> InvestigationOutcome:
        evidence: list[EvidenceRef] = []
        called_tools: list[str] = []
        seen_signatures: set[str] = set()
        observations: list[ToolObservation] = []
        notes: list[str] = []
        # 调查部分的用量：按轮记账、按请求累计；缺失即未知，不用上一次的值顶替。
        usage = UsageBudget()
        report = InvestigationReport(planner=self._planner.name, stop_reason=StopReason.ROUNDS_EXHAUSTED.value)

        for round_index in range(1, self._max_rounds + 1):
            report.rounds = round_index
            round_ledger = UsageLedger()
            try:
                with usage_scope(round_ledger, phase=PHASE_INVESTIGATE):
                    action = await self._planner.plan(
                        requirement=requirement,
                        slots=slots,
                        evidence=list(evidence),
                        called_tools=list(called_tools),
                        round_index=round_index,
                        # 把**全部**结构化观察反馈给下一轮：只给搜索 hits 会让决策者
                        # 看不见已经拿到的表结构、变更上下文与扫描结果。
                        observations=list(observations),
                    )
            except TaskBudgetExceeded as error:
                # 任务级 token 预算用尽：按预算主动停止，并如实标注原因。
                report.stop_reason = StopReason.BUDGET_EXHAUSTED.value
                notes.append(f"任务预算用尽，调查提前停止：{error}")
                usage.merge(round_ledger)
                break
            except PlannerDecisionError as error:
                # 决策者没能给出可执行的动作。此时**没有任何工具被执行**，
                # 因此不能算作"调用失败"，也不能继续生成草案。
                report.stop_reason = StopReason.PLANNER_FAILED.value
                notes.append(f"决策者未能给出可执行的动作：{error}")
                usage.merge(round_ledger)
                break
            usage.merge(round_ledger)

            if isinstance(action, Finish):
                missing = self._required.missing(evidence, slots, schema_snapshot)
                # 关键：模型说"够了"不算数，由代码判定完成条件。
                report.stop_reason = (
                    StopReason.EVIDENCE_SUFFICIENT.value if not missing else StopReason.INSUFFICIENT_EVIDENCE.value
                )
                break
            if isinstance(action, AskUser):
                report.stop_reason = StopReason.INSUFFICIENT_EVIDENCE.value
                report.clarification_requests.append(action.reason)
                notes.append(f"决策者要求补充信息：{action.reason}")
                break

            if report.tool_calls >= self._max_tool_calls:
                report.stop_reason = StopReason.TOOL_CALLS_EXHAUSTED.value
                notes.append(f"累计工具调用已达上限 {self._max_tool_calls}")
                break

            signature = f"{action.tool}::{json.dumps(action.args, sort_keys=True, ensure_ascii=False)}"
            args_digest = digest_payload(action.args)
            if signature in seen_signatures:
                # 相同工具 + 相同参数再调一次只会得到同样的结果，属于空转。
                report.stop_reason = StopReason.NO_PROGRESS.value
                notes.append(f"重复调用 {action.tool} 且参数未变，判定为无进展")
                break
            seen_signatures.add(signature)

            try:
                result = await asyncio.wait_for(
                    self._registry.call(action.tool, action.args, self._context),
                    timeout=self._tool_timeout,
                )
            except asyncio.TimeoutError:
                # 超时按失败处理，不让循环挂住；也不当成"没问题"。失败同样要留下观察。
                report.tool_calls += 1
                called_tools.append(action.tool)
                observations.append(
                    ToolObservation(
                        tool=action.tool,
                        ok=False,
                        args_digest=args_digest,
                        error=f"超时（{self._tool_timeout:g} 秒未返回）",
                        observed_at=datetime.now(timezone.utc).isoformat(),
                        kind="timeout",
                    )
                )
                report.stop_reason = StopReason.TOOL_FAILED.value
                notes.append(f"{action.tool} 超过 {self._tool_timeout:g} 秒未返回，按失败处理")
                break

            report.tool_calls += 1
            called_tools.append(action.tool)
            observations.append(
                ToolObservation(
                    tool=str(getattr(result, "tool", "") or action.tool),
                    ok=bool(result.ok),
                    args_digest=args_digest,
                    # 摘要截断，但保留内容摘要哈希与证据标识，截断不等于无法核对。
                    payload_digest=digest_payload(result.data) if result.ok else "",
                    summary=summarize_payload(result.data) if result.ok else "",
                    error=result.error or "",
                    evidence_ids=list(result.evidence_ids or []),
                    data_version=str(result.data_version or ""),
                    observed_at=(result.observed_at or datetime.now(timezone.utc)).isoformat(),
                    kind="search" if isinstance((result.data or {}).get("hits"), list) else "material",
                )
            )
            if not result.ok:
                notes.append(f"{action.tool} 失败：{result.error}")
                continue
            fresh = evidence_from_tool_result(result)
            if fresh:
                evidence.extend(fresh)
            else:
                notes.append(f"{action.tool} 未返回可引用片段")

        report.called_tools = called_tools
        report.observations = observations
        report.missing_required = self._required.missing(evidence, slots, schema_snapshot)
        report.notes = notes
        # 规则/脚本决策者没有逐次响应可记账，但可以声明一个总量；只有在整轮都没经过
        # provider 记账时才用它，避免把同一份用量重复计入。
        if usage.requests == 0:
            last_usage = getattr(self._planner, "last_usage", None)
            if isinstance(last_usage, Mapping) and {"prompt_tokens", "completion_tokens"} <= set(last_usage):
                usage.add_usage(last_usage)
        report.usage = usage
        # 预算用尽但必需证据已经齐了，就不是"证据不足"，如实改判。
        if (
            report.stop_reason in {StopReason.ROUNDS_EXHAUSTED.value, StopReason.TOOL_CALLS_EXHAUSTED.value}
            and not report.missing_required
        ):
            report.stop_reason = StopReason.EVIDENCE_SUFFICIENT.value
        return InvestigationOutcome(evidence=evidence, report=report)


def build_planner(
    settings: Any, provider: Any, tools: list[dict[str, Any]] | None = None
) -> InvestigationPlanner:
    """按配置选择决策者。

    默认是规则决策者：它明确叫 "rule"，不冒充模型。想让模型参与工具选择，
    必须显式要求 provider 决策，而 provider 不具备该能力时会**显式失败**。

    `tools` 是服务端给出的只读工具规格，只有模型决策者会用到：模型只能在这些工具里选。
    """
    requested = str(getattr(settings, "investigation_planner", "rule") or "rule").lower()
    if requested == "provider":
        return ProviderPlanner(provider, tools=tools)
    if requested != "rule":  # pragma: no cover - 配置校验在 Settings 层
        raise PlannerUnavailable(f"未知的决策者：{requested}")
    return RulePlanner()
