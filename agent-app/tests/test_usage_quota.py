"""用量闸门（配额 / 限流）的回归。

这段功能只存在于生产 fork 里（上游仓库没有 app/usage.py），合并时按原样搬回，
这里给它最小覆盖：**超出单用户每分钟限额返回 429**，且闸门在真正调用模型之前生效。
"""

from __future__ import annotations

from pathlib import Path

from app.usage import UsageLimits, UsageLimiter
from tests.conftest import REQUIREMENT
from tests.test_api import HEADERS, build_client


def test_quota_returns_429_after_the_per_minute_limit(tmp_path: Path) -> None:
    client = build_client(tmp_path)
    client.app.state.usage = UsageLimiter(UsageLimits(per_minute=1, per_user_daily=0, global_daily=0))

    first = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)
    second = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)

    assert first.status_code == 202, first.text
    assert second.status_code == 429, second.text
    assert "每分钟" in second.json()["detail"]


def test_quota_is_counted_per_identity(tmp_path: Path) -> None:
    """限额按治理服务注入的 X-Actor-Id 计：换人不共享额度。"""
    client = build_client(tmp_path)
    client.app.state.usage = UsageLimiter(UsageLimits(per_minute=1, per_user_daily=0, global_daily=0))

    alice = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)
    bob = client.post(
        "/api/agent/tasks",
        json={"requirement": REQUIREMENT},
        headers={"X-Actor-Id": "bob", "X-Org-Id": "org_demo"},
    )

    assert alice.status_code == 202, alice.text
    assert bob.status_code == 202, bob.text


def test_disabled_limits_do_not_block(tmp_path: Path) -> None:
    """三层限额全为 0 时闸门关闭，行为与上游一致（不会误伤）。"""
    client = build_client(tmp_path)
    client.app.state.usage = UsageLimiter(UsageLimits(per_minute=0, per_user_daily=0, global_daily=0))

    for _ in range(3):
        response = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)
        assert response.status_code == 202, response.text


def test_global_daily_limit_is_enforced_across_users(tmp_path: Path) -> None:
    """全局每日限额是整站软顶：不同用户共享同一份计数。"""
    client = build_client(tmp_path)
    client.app.state.usage = UsageLimiter(UsageLimits(per_minute=0, per_user_daily=0, global_daily=1))

    first = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)
    second = client.post(
        "/api/agent/tasks",
        json={"requirement": REQUIREMENT},
        headers={"X-Actor-Id": "bob", "X-Org-Id": "org_demo"},
    )

    assert first.status_code == 202, first.text
    assert second.status_code == 429, second.text
    assert "总额度" in second.json()["detail"]
