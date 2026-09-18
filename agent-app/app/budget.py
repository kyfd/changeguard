"""模型用量记账与任务级预算。

三条口径，缺一条就会把"未知"说成"已知"：

1. **按请求**：每一次模型响应都记一笔；响应没带 usage 就记一笔"缺失"，
   绝不复用上一次的值（复用会让缺失显示成已知）。
2. **按任务**：记账器由调用方创建并放进 `usage_scope`，用 `ContextVar` 承载，
   因此并发任务各自独立——provider 是跨任务共享的，把累计量放在它上面会串数。
3. **缺失不等于 0**：真实 token 只累加真的报了 usage 的响应；未报的计入 `missing`，
   `known` 为假。预算判定则对未报的响应按一个**保守值**计费，使预算在"未知"时仍然有界。

本模块不依赖 `app.llm` 或 `app.workflow`，避免为了一个异常类型产生循环导入。
"""

from __future__ import annotations

import contextvars
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any, Iterator, Mapping


class TaskBudgetExceeded(RuntimeError):
    """任务级 token 预算已用尽。

    必须与"模型调用失败"区分开：它不是故障，而是**按预算主动停止**。
    """


@dataclass
class UsageLedger:
    """按请求累计的 usage 记账器（同时承担任务级预算）。"""

    max_total_tokens: int = 0
    max_prompt_tokens: int = 0
    # 费用上限与单价。**没有定价数据时费用上限必须失败关闭**：
    # 把"不知道花了多少钱"当成"没超预算"，正是要避免的那种谎报。
    max_cost_estimate: float = 0.0
    prompt_price_per_1k: float = 0.0
    completion_price_per_1k: float = 0.0
    # 未提供 usage 的响应，按这个值计入**预算**（保守估计），但不计入真实 token 统计。
    unknown_charge_tokens: int = 0

    requests: int = 0
    reported: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    charged_unknown_tokens: int = 0

    def add(self, usage: Mapping[str, Any] | None) -> None:
        self.requests += 1
        if not isinstance(usage, Mapping):
            # 缺失：只为预算计一笔保守费用，真实统计保持"未知"。
            self.charged_unknown_tokens += max(0, int(self.unknown_charge_tokens))
            return
        self.reported += 1
        self.prompt_tokens += int(usage.get("prompt_tokens") or 0)
        self.completion_tokens += int(usage.get("completion_tokens") or 0)

    @property
    def missing(self) -> int:
        return self.requests - self.reported

    @property
    def known(self) -> bool:
        return self.requests > 0 and self.missing == 0

    @property
    def counted_tokens(self) -> int:
        """用于**预算判定**的量：真实 token + 未知响应的保守计费。"""
        return self.prompt_tokens + self.completion_tokens + self.charged_unknown_tokens

    @property
    def cost_estimate(self) -> float | None:
        """按配置单价折算的费用；**没有单价就是 None（未知）**，不是 0。"""
        if self.prompt_price_per_1k <= 0 and self.completion_price_per_1k <= 0:
            return None
        return round(
            self.prompt_tokens / 1000 * self.prompt_price_per_1k
            + self.completion_tokens / 1000 * self.completion_price_per_1k,
            6,
        )

    def exceeded(self) -> str | None:
        if self.max_total_tokens > 0 and self.counted_tokens > self.max_total_tokens:
            detail = ""
            if self.charged_unknown_tokens:
                detail = (
                    f"，其中 {self.missing} 次未提供 usage 的响应按 "
                    f"{self.unknown_charge_tokens} token/次保守计入"
                )
            return f"任务 token 预算已用尽：上限 {self.max_total_tokens}，已计 {self.counted_tokens}{detail}"
        if self.max_prompt_tokens > 0 and self.prompt_tokens > self.max_prompt_tokens:
            return f"任务 prompt token 预算已用尽：上限 {self.max_prompt_tokens}，已计 {self.prompt_tokens}"
        if self.max_cost_estimate > 0:
            cost = self.cost_estimate
            if cost is None:
                # 失败关闭：没有定价数据就无法判定是否超预算，不能当作"没超"。
                return (
                    "配置了任务费用上限，但没有可用的定价数据（AGENT_LLM_PRICE_PROMPT_PER_1K / "
                    "AGENT_LLM_PRICE_COMPLETION_PER_1K 均为 0）：费用预算无法执行，按失败处理"
                )
            if cost > self.max_cost_estimate:
                return f"任务费用预算已用尽：上限 {self.max_cost_estimate}，已计 {cost}"
        return None

    def as_dict(self) -> dict[str, Any]:
        """对外表示。缺失项显式为 unknown，不填 0。"""
        if self.missing:
            note = (
                f"{self.requests} 次模型响应中有 {self.missing} 次未提供 usage："
                "token 总量不完整、按未知处理，不填 0 冒充已知"
            )
        elif self.requests:
            note = "usage 由 provider 按请求提供"
        else:
            note = "本任务没有发生模型调用：无 usage 可报"
        return {
            "requests": self.requests,
            "reported_responses": self.reported,
            "missing_responses": self.missing,
            "prompt_tokens": self.prompt_tokens if self.reported else None,
            "completion_tokens": self.completion_tokens if self.reported else None,
            "charged_unknown_tokens": self.charged_unknown_tokens,
            "known": self.known,
            "cost_estimate": self.cost_estimate,
            "cost_known": self.cost_estimate is not None,
            "note": note,
        }


_ACTIVE_LEDGERS: contextvars.ContextVar[tuple[UsageLedger, ...]] = contextvars.ContextVar(
    "agent_usage_ledgers", default=()
)


def active_ledgers() -> tuple[UsageLedger, ...]:
    return _ACTIVE_LEDGERS.get()


@contextmanager
def usage_scope(ledger: UsageLedger) -> Iterator[UsageLedger]:
    """在作用域内把每次模型响应的 usage 都记到这个记账器上。

    支持嵌套：服务层开任务级记账器，调查循环在其内部再开一个"调查部分"的记账器，
    两者都会收到同一批响应，因此既能看到任务总量，也能看到调查部分的量。
    作用域跟随 asyncio 任务，不会串到其他任务。
    """
    token = _ACTIVE_LEDGERS.set(_ACTIVE_LEDGERS.get() + (ledger,))
    try:
        yield ledger
    finally:
        _ACTIVE_LEDGERS.reset(token)
