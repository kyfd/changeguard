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
import json
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Mapping, Protocol

from app.schemas.drafts import EvidenceRef, TaskSlots, ToolResult

# 引用片段里只保留这些前缀的文档作为"规范"，与检索层的作用域一致。
NORM_PREFIX = "norms/"


class StopReason(str, Enum):
    """循环为什么停下。每一种都必须能被外部看到，不能含糊成"结束了"。"""

    EVIDENCE_SUFFICIENT = "EVIDENCE_SUFFICIENT"
    INSUFFICIENT_EVIDENCE = "INSUFFICIENT_EVIDENCE"
    ROUNDS_EXHAUSTED = "ROUNDS_EXHAUSTED"
    TOOL_CALLS_EXHAUSTED = "TOOL_CALLS_EXHAUSTED"
    NO_PROGRESS = "NO_PROGRESS"
    PLANNER_UNAVAILABLE = "PLANNER_UNAVAILABLE"
    TOOL_FAILED = "TOOL_FAILED"


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
    """决策契约。实现方必须能说明自己是谁——不要把规则包装成模型。"""

    name: str

    def plan(
        self,
        *,
        requirement: str,
        slots: TaskSlots,
        evidence: list[EvidenceRef],
        called_tools: list[str],
        round_index: int,
    ) -> InvestigationAction:
        ...


class RequiredEvidence:
    """"什么算证据够用"的确定性判定。

    刻意不由模型回答"我觉得够了"：完成条件必须是可复核的代码。
    """

    def missing(self, evidence: list[EvidenceRef], slots: TaskSlots) -> list[str]:
        missing: list[str] = []
        if not any(item.doc_id.startswith(NORM_PREFIX) for item in evidence):
            missing.append("至少一条规范片段（norms/）")
        return missing


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
    notes: list[str] = field(default_factory=list)

    def summary(self) -> str:
        parts = [f"planner={self.planner}", f"stop={self.stop_reason}", f"轮次={self.rounds}", f"工具调用={self.tool_calls}"]
        if self.missing_required:
            parts.append("缺证据：" + "、".join(self.missing_required))
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


class ProviderPlanner:
    """把模型的原生动作能力接进循环。

    关键约束：provider 必须**明确声明**支持动作决策。当前 OpenAI 兼容 provider
    只实现了 `generate`（返回草案文本），因此这里会显式报告不可用，
    而不是拿规则去冒充"模型的选择"。
    """

    name = "provider"

    def __init__(self, provider: Any) -> None:
        if not hasattr(provider, "decide"):
            raise PlannerUnavailable(
                f"provider {getattr(provider, 'name', type(provider).__name__)} 未实现 decide()，"
                "无法进行模型驱动的工具选择；请显式选择 RulePlanner，或为该 provider 实现动作契约。"
            )
        self._provider = provider

    def plan(self, **kwargs: Any) -> InvestigationAction:
        return self._provider.decide(**kwargs)


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

    def plan(
        self,
        *,
        requirement: str,
        slots: TaskSlots,
        evidence: list[EvidenceRef],
        called_tools: list[str],
        round_index: int,
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

    def plan(self, *, round_index: int, **_kwargs: Any) -> InvestigationAction:
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
        notes: list[str] = []
        report = InvestigationReport(planner=self._planner.name, stop_reason=StopReason.ROUNDS_EXHAUSTED.value)

        for round_index in range(1, self._max_rounds + 1):
            report.rounds = round_index
            action = self._planner.plan(
                requirement=requirement,
                slots=slots,
                evidence=list(evidence),
                called_tools=list(called_tools),
                round_index=round_index,
            )

            if isinstance(action, Finish):
                missing = self._required.missing(evidence, slots)
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
                # 超时按失败处理，不让循环挂住；也不当成"没问题"。
                report.tool_calls += 1
                called_tools.append(action.tool)
                report.stop_reason = StopReason.TOOL_FAILED.value
                notes.append(f"{action.tool} 超过 {self._tool_timeout:g} 秒未返回，按失败处理")
                break

            report.tool_calls += 1
            called_tools.append(action.tool)
            if not result.ok:
                notes.append(f"{action.tool} 失败：{result.error}")
                continue
            fresh = evidence_from_tool_result(result)
            if fresh:
                evidence.extend(fresh)
            else:
                notes.append(f"{action.tool} 未返回可引用片段")

        report.called_tools = called_tools
        report.missing_required = self._required.missing(evidence, slots)
        report.notes = notes
        # 预算用尽但必需证据已经齐了，就不是"证据不足"，如实改判。
        if (
            report.stop_reason in {StopReason.ROUNDS_EXHAUSTED.value, StopReason.TOOL_CALLS_EXHAUSTED.value}
            and not report.missing_required
        ):
            report.stop_reason = StopReason.EVIDENCE_SUFFICIENT.value
        return InvestigationOutcome(evidence=evidence, report=report)


def build_planner(settings: Any, provider: Any) -> InvestigationPlanner:
    """按配置选择决策者。

    默认是规则决策者：它明确叫 "rule"，不冒充模型。想让模型参与工具选择，
    必须显式要求 provider 决策，而 provider 不具备该能力时会**显式失败**。
    """
    requested = str(getattr(settings, "investigation_planner", "rule") or "rule").lower()
    if requested == "provider":
        return ProviderPlanner(provider)
    if requested != "rule":  # pragma: no cover - 配置校验在 Settings 层
        raise PlannerUnavailable(f"未知的决策者：{requested}")
    return RulePlanner()
