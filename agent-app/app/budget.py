"""任务级调用账本与预算。

四条口径，缺一条就会把"未知"说成"已知"，或者让预算形同虚设：

1. **按请求**：每一次 HTTP 请求都记一条结构化记录（含重试、失败、超时）；
   响应没带 usage 就记一笔"缺失"，绝不复用上一次的值。
2. **按任务**：账本由调用方创建并放在 `usage_scope` 里，用 `ContextVar` 承载；
   provider 是跨任务共享的，把累计量放在它上面会让并发任务互相串账。
3. **缺失不等于 0**：真实 token 只累加真的报了 usage 的响应；未报的计入 `missing`，
   `known` 为假；预算判定则对未报的请求按**保守值**计费，使预算在"未知"时仍然有界。
4. **必须可持久化**：账本支持从已落盘的累计值**续算**（恢复/重启不重置预算），
   并在每次记账后回调持久化钩子；钩子失败必须上抛——不允许在无账本的情况下继续调用模型。

本模块不依赖 `app.llm` 或 `app.workflow`，避免为了一个异常类型产生循环导入。
"""

from __future__ import annotations

import contextvars
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator, Mapping

# 调用阶段：调查（决策）与生成（含修订）分开计费，便于回答"钱花在哪一步"。
PHASE_INVESTIGATE = "investigate"
PHASE_GENERATE = "generate"
PHASE_OTHER = "other"

# 账本里保留的调用记录条数上限：够复盘，又不会让任务记录无限膨胀。
MAX_CALL_RECORDS = 60


class TaskBudgetExceeded(RuntimeError):
    """任务级预算已用尽。

    必须与"模型调用失败"区分开：它不是故障，而是**按预算主动停止**。
    """


class LedgerPersistError(RuntimeError):
    """账本无法持久化。

    此时必须**停止**继续调用模型：继续调用就会产生没有账目的消耗。
    """


@dataclass(frozen=True)
class CallRecord:
    """一次模型 HTTP 请求的结构化记录。

    只记录可核对的元数据：不含凭据、不含完整提示词、不含隐式思维链。
    """

    sequence: int
    phase: str
    outcome: str  # ok / error / timeout / cancelled
    requests: int
    duration_ms: int
    usage_known: bool
    prompt_tokens: int | None
    completion_tokens: int | None
    model: str
    failure_type: str | None = None


@dataclass
class UsageLedger:
    """按请求累计的调用账本（同时承担任务级预算）。"""

    # 预算（0 = 不限制）
    max_total_tokens: int = 0
    max_prompt_tokens: int = 0
    # 费用上限与单价。**没有定价数据时费用上限必须失败关闭**：
    # 把"不知道花了多少钱"当成"没超预算"，正是要避免的那种谎报。
    max_cost_estimate: float = 0.0
    prompt_price_per_1k: float = 0.0
    completion_price_per_1k: float = 0.0
    max_requests: int = 0
    # 未提供 usage 的响应，按这个值计入**预算**（保守估计），但不计入真实 token 统计。
    unknown_charge_tokens: int = 0

    # 身份：结构化记录要能按任务/执行复盘。
    task_id: str = ""
    execution_id: str = ""
    model: str = ""

    # 累计量
    requests: int = 0
    reported: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    charged_unknown_tokens: int = 0
    calls: list[CallRecord] = field(default_factory=list)

    # 每次记账后回调（服务层用它落盘）。抛错即代表"不能再继续调用"。
    on_update: Callable[[dict[str, Any]], None] | None = None

    # -- 累计 ---------------------------------------------------------------

    def add_call(
        self,
        *,
        phase: str,
        outcome: str,
        requests: int,
        duration_ms: int,
        usage: Mapping[str, Any] | None,
        model: str = "",
        failure_type: str | None = None,
    ) -> CallRecord:
        """登记一次模型请求；缺失 usage 时显式记为缺失。"""
        self.requests += 1
        known = isinstance(usage, Mapping)
        if known:
            self.reported += 1
            self.prompt_tokens += int(usage.get("prompt_tokens") or 0)
            self.completion_tokens += int(usage.get("completion_tokens") or 0)
        else:
            # 缺失：只为预算计一笔保守费用，真实统计保持"未知"。
            self.charged_unknown_tokens += max(0, int(self.unknown_charge_tokens))
        record = CallRecord(
            sequence=self.requests,
            phase=phase or PHASE_OTHER,
            outcome=outcome,
            requests=max(0, int(requests)),
            duration_ms=max(0, int(duration_ms)),
            usage_known=known,
            prompt_tokens=(int(usage["prompt_tokens"]) if known and usage.get("prompt_tokens") is not None else None),
            completion_tokens=(
                int(usage["completion_tokens"]) if known and usage.get("completion_tokens") is not None else None
            ),
            model=model or self.model,
            failure_type=failure_type,
        )
        self.calls.append(record)
        if len(self.calls) > MAX_CALL_RECORDS:
            del self.calls[: len(self.calls) - MAX_CALL_RECORDS]
        if self.on_update is not None:
            # 落盘失败必须上抛：宁可停止任务，也不要继续产生没有账目的消耗。
            try:
                self.on_update(self.as_dict())
            except LedgerPersistError:
                raise
            except Exception as error:  # noqa: BLE001 - 统一转成显式的账本故障
                raise LedgerPersistError(f"调用账本未能落盘：{type(error).__name__}: {error}") from error
        return record

    # -- 汇总 ---------------------------------------------------------------

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
        if self.max_requests > 0 and self.requests >= self.max_requests:
            return f"任务模型请求次数已用尽：上限 {self.max_requests}，已发 {self.requests}"
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

    # -- 表示与续算 ---------------------------------------------------------

    def as_dict(self) -> dict[str, Any]:
        """对外表示。缺失项显式为 unknown，不填 0。"""
        if self.missing:
            note = (
                f"{self.requests} 次模型请求中有 {self.missing} 次未提供 usage："
                "token 总量不完整、按未知处理，不填 0 冒充已知"
            )
        elif self.requests:
            note = "usage 由 provider 按请求提供；估算费用不等于供应商账单"
        else:
            note = "本任务没有发生模型调用：无 usage 可报"
        return {
            "task_id": self.task_id,
            "execution_id": self.execution_id,
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
            "calls": [record.__dict__ for record in self.calls],
        }

    def seed_from_prior(self, prior: Mapping[str, Any] | None) -> None:
        """从已落盘的累计值续算。

        恢复、重试与进程重启都**不得清零**任务累计预算；只有真正新建任务才从零开始。
        """
        if not isinstance(prior, Mapping):
            return
        self.requests += int(prior.get("requests") or 0)
        self.reported += int(prior.get("reported_responses") or 0)
        self.prompt_tokens += int(prior.get("prompt_tokens") or 0)
        self.completion_tokens += int(prior.get("completion_tokens") or 0)
        self.charged_unknown_tokens += int(prior.get("charged_unknown_tokens") or 0)


_ACTIVE_LEDGERS: contextvars.ContextVar[tuple[UsageLedger, ...]] = contextvars.ContextVar(
    "agent_usage_ledgers", default=()
)
_ACTIVE_PHASE: contextvars.ContextVar[str] = contextvars.ContextVar("agent_call_phase", default=PHASE_OTHER)


def active_ledgers() -> tuple[UsageLedger, ...]:
    return _ACTIVE_LEDGERS.get()


def active_phase() -> str:
    return _ACTIVE_PHASE.get()


@contextmanager
def usage_scope(ledger: UsageLedger, *, phase: str = PHASE_OTHER) -> Iterator[UsageLedger]:
    """在作用域内把每次模型响应都记到这个账本上。

    支持嵌套：服务层开任务级账本，调查循环在其内部再开一个"调查部分"的账本，
    两者都会收到同一批响应，因此既有任务总量也能分阶段归因。
    作用域跟随 asyncio 任务，不会串到其他任务。
    """
    ledger_token = _ACTIVE_LEDGERS.set(_ACTIVE_LEDGERS.get() + (ledger,))
    phase_token = _ACTIVE_PHASE.set(phase)
    try:
        yield ledger
    finally:
        _ACTIVE_PHASE.reset(phase_token)
        _ACTIVE_LEDGERS.reset(ledger_token)
