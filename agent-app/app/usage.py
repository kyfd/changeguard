"""用量限额：防止恶意或失控的调用烧掉模型账单。

设计要点：

- 以治理服务在服务端注入的 ``X-Actor-Id`` 为键。身份不可由调用方伪造，
  所以按用户限额是可信的；伪造身份的调用在 ``resolve_context`` 就被拒绝。
- 三层限额：单用户每分钟窗口、单用户每日、全局每日。前两层防单点滥用，
  全局层是整个模型账单的软顶。
- 进程内计数器，重启清零。这是刻意的 v1 权衡：这里是防滥用的闸门，
  不是计费系统；账单的硬顶由模型服务商控制台的用量封顶负责。
- 阈值为 0 表示该层不启用。超限抛 :class:`QuotaExceeded`，由路由层转成 429。
"""

from __future__ import annotations

import threading
import time
from collections import defaultdict, deque
from dataclasses import dataclass
from datetime import date


class QuotaExceeded(Exception):
    """超出了某一层用量限额。detail 面向最终用户，可直接展示。"""

    def __init__(self, detail: str) -> None:
        super().__init__(detail)
        self.detail = detail


@dataclass(frozen=True)
class UsageLimits:
    """三层阈值。0 表示不启用该层。"""

    per_minute: int = 6
    per_user_daily: int = 60
    global_daily: int = 400

    @property
    def enabled(self) -> bool:
        return self.per_minute > 0 or self.per_user_daily > 0 or self.global_daily > 0


class UsageLimiter:
    """线程安全的进程内用量闸门。检查与记账在同一把锁里完成。"""

    def __init__(self, limits: UsageLimits) -> None:
        self._limits = limits
        self._lock = threading.Lock()
        self._windows: dict[str, deque[float]] = defaultdict(deque)
        self._user_daily: dict[tuple[str, date], int] = defaultdict(int)
        self._global_daily: dict[date, int] = defaultdict(int)

    def check(self, user_id: str) -> None:
        """记录一次调用；超限时抛 QuotaExceeded 且不记账。"""
        now = time.monotonic()
        today = date.today()
        with self._lock:
            if self._limits.per_minute > 0:
                window = self._windows[user_id]
                cutoff = now - 60.0
                while window and window[0] < cutoff:
                    window.popleft()
                if len(window) >= self._limits.per_minute:
                    raise QuotaExceeded(
                        f"请求过于频繁：每分钟最多 {self._limits.per_minute} 次，请稍后再试。"
                    )
            if self._limits.per_user_daily > 0:
                if self._user_daily[(user_id, today)] >= self._limits.per_user_daily:
                    raise QuotaExceeded(
                        f"今日个人额度已用完：每天最多 {self._limits.per_user_daily} 次，明天再试。"
                    )
            if self._limits.global_daily > 0:
                if self._global_daily[today] >= self._limits.global_daily:
                    raise QuotaExceeded("服务今日总额度已用完，请联系管理员。")
            self._windows[user_id].append(now)
            self._user_daily[(user_id, today)] += 1
            self._global_daily[today] += 1

    def describe(self) -> dict[str, int]:
        return {
            "per_minute": self._limits.per_minute,
            "per_user_daily": self._limits.per_user_daily,
            "global_daily": self._limits.global_daily,
        }
