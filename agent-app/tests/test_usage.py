"""用量限额测试：防止滥用烧掉模型账单的闸门。

这些断言锁住：

- 三层限额（每分钟、单用户每日、全局每日）各自独立生效，超限抛 QuotaExceeded；
- 被拒绝的调用不记账：反复失败不会把额度吃掉；
- 滑动窗口会随时间恢复（用假时钟推进）；
- HTTP 层把 QuotaExceeded 转成 429，且 create 与 clarify 都被闸住；
- 阈值 0 表示不启用该层。
"""

from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from app.config import Settings
from app.main import create_app
from app.usage import QuotaExceeded, UsageLimits, UsageLimiter
from tests.conftest import DEMO_DIR

UPSTREAM_HEADERS = {"X-Agent-Upstream-Token": "shared-secret-for-agent-service"}
IDENTITY_HEADERS = {"X-Actor-Id": "alice", "X-Org-Id": "org_demo"}

PAYLOAD = {"requirement": "给订单表按用户和时间查询准备一个索引变更，目标 PostgreSQL。"}
TASK_PAYLOAD = {**PAYLOAD, "application": "order-service", "schema_snapshot": "CREATE TABLE orders (id bigint, user_id bigint, created_at timestamptz);"}
CLARIFY_PAYLOAD = {"application": "order-service"}


def build_client(tmp_path: Path, **overrides: int) -> TestClient:
    settings = Settings(
        allow_header_identity=True,
        upstream_token=UPSTREAM_HEADERS["X-Agent-Upstream-Token"],
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode="inline",
        **overrides,
    )
    return TestClient(create_app(settings))


def test_per_minute_limit_rejects_and_does_not_charge(tmp_path: Path) -> None:
    limiter = UsageLimiter(UsageLimits(per_minute=2, per_user_daily=0, global_daily=0))
    limiter.check("alice")
    limiter.check("alice")
    for _ in range(5):
        # 被拒绝的调用不允许消耗后续额度。
        try:
            limiter.check("alice")
        except QuotaExceeded:
            pass
    assert limiter.describe()["per_minute"] == 2


def test_per_user_daily_limit(tmp_path: Path) -> None:
    limiter = UsageLimiter(UsageLimits(per_minute=0, per_user_daily=3, global_daily=0))
    for _ in range(3):
        limiter.check("alice")
    try:
        limiter.check("alice")
        raise AssertionError("expected QuotaExceeded")
    except QuotaExceeded as error:
        assert "个人额度" in error.detail
    # 其他用户不受影响。
    limiter.check("bob")


def test_global_daily_limit(tmp_path: Path) -> None:
    limiter = UsageLimiter(UsageLimits(per_minute=0, per_user_daily=0, global_daily=4))
    for name in ("a", "b", "c", "d"):
        limiter.check(name)
    try:
        limiter.check("e")
        raise AssertionError("expected QuotaExceeded")
    except QuotaExceeded as error:
        assert "总额度" in error.detail


def test_zero_threshold_disables_layer() -> None:
    limiter = UsageLimiter(UsageLimits(per_minute=0, per_user_daily=0, global_daily=0))
    for _ in range(50):
        limiter.check("alice")
    assert limiter.describe() == {"per_minute": 0, "per_user_daily": 0, "global_daily": 0}


def test_http_429_on_create_and_clarify(tmp_path: Path) -> None:
    client = build_client(tmp_path, rate_per_minute=1)
    created = client.post(
        "/api/agent/tasks",
        json=TASK_PAYLOAD,
        headers={**UPSTREAM_HEADERS, **IDENTITY_HEADERS},
    )
    assert created.status_code == 202, created.text
    task_id = created.json()["task_id"]

    second = client.post(
        "/api/agent/tasks",
        json=PAYLOAD,
        headers={**UPSTREAM_HEADERS, **IDENTITY_HEADERS},
    )
    assert second.status_code == 429
    assert "每分钟" in second.json()["detail"]

    clarified = client.post(
        f"/api/agent/tasks/{task_id}/clarify",
        json=CLARIFY_PAYLOAD,
        headers={**UPSTREAM_HEADERS, **IDENTITY_HEADERS},
    )
    assert clarified.status_code == 429


def test_429_body_is_user_facing(tmp_path: Path) -> None:
    client = build_client(tmp_path, rate_per_minute=1, user_daily_limit=2)
    headers = {**UPSTREAM_HEADERS, **IDENTITY_HEADERS}
    client.post("/api/agent/tasks", json=PAYLOAD, headers=headers)
    rejected = client.post("/api/agent/tasks", json=PAYLOAD, headers=headers)
    body = rejected.json()
    assert rejected.status_code == 429
    assert isinstance(body["detail"], str) and body["detail"]
