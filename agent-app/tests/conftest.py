"""测试夹具。

约定：
- 所有测试都使用 `examples/agent-demo/` 下的**合成数据**，不接触任何真实业务数据。
- 默认走 `inline` 执行模式，让断言确定、可复现；取消相关的测试单独用 background 模式。
"""

from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Coroutine

import pytest

from app.config import Settings
from app.schemas.drafts import CreateTaskRequest, DatabaseKind, TaskSlots
from app.service import AgentService
from app.tools.registry import TrustedContext

REPO_ROOT = Path(__file__).resolve().parents[2]
DEMO_DIR = REPO_ROOT / "examples" / "agent-demo"

SCHEMA_SNAPSHOT = (DEMO_DIR / "schema" / "orders.sql").read_text(encoding="utf-8")
SLOW_QUERY = (DEMO_DIR / "queries" / "slow-orders.sql").read_text(encoding="utf-8")

PLANNED_AT = datetime(2026, 9, 18, 21, 30, tzinfo=timezone(timedelta(hours=8)))

REQUIREMENT = "给订单表按用户和创建时间查询的场景准备一个索引变更，目标 PostgreSQL，安排在周五晚上。"


def run(coro: Coroutine[Any, Any, Any]) -> Any:
    """在独立事件循环里执行协程，避免引入 pytest-asyncio 依赖。"""
    return asyncio.run(coro)


def execution_handles(service: Any) -> list[asyncio.Task[Any]]:
    """当前登记的执行任务（测试辅助）。

    用途有两个：确认执行结束后**资源确实被清理**，以及等待一次执行跑完。
    兼容改造前后的登记结构——测试不应该因为内部改名而失败，那不是业务问题。
    """
    registry = getattr(service, "_executions", None)
    if registry is None:
        registry = getattr(service, "_running", {}) or {}
    handles: list[asyncio.Task[Any]] = []
    for item in list(registry.values()):
        task = getattr(item, "task", item)
        if isinstance(task, asyncio.Task):
            handles.append(task)
    return handles


@pytest.fixture
def settings(tmp_path: Path) -> Settings:
    return Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode="inline",
        max_revisions=2,
        allow_header_identity=True,
    )


@pytest.fixture
def empty_settings(tmp_path: Path) -> Settings:
    """语料为空的配置，用于验证"找不到依据时会明说"。"""
    empty_dir = tmp_path / "empty-corpus"
    empty_dir.mkdir(parents=True, exist_ok=True)
    return Settings(
        agent_demo_dir=str(empty_dir),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode="inline",
        max_revisions=2,
    )


@pytest.fixture
def service(settings: Settings) -> AgentService:
    return AgentService(settings)


@pytest.fixture
def context() -> TrustedContext:
    return TrustedContext(user_id="alice", organization_id="org_demo")


def complete_request(**overrides: Any) -> CreateTaskRequest:
    """一份信息完整的请求：不会触发追问。"""
    base: dict[str, Any] = {
        "requirement": REQUIREMENT,
        "application": "order-service",
        "environment": "生产",
        "database": DatabaseKind.POSTGRESQL,
        "table": "orders",
        "query_sql": SLOW_QUERY,
        "planned_at": PLANNED_AT,
        "planned_at_timezone": "Asia/Shanghai",
        "schema_snapshot": SCHEMA_SNAPSHOT,
    }
    base.update(overrides)
    return CreateTaskRequest(**base)


def bare_request(**overrides: Any) -> CreateTaskRequest:
    """只有一句需求：应当触发追问。"""
    base: dict[str, Any] = {"requirement": REQUIREMENT}
    base.update(overrides)
    return CreateTaskRequest(**base)


def complete_slots() -> TaskSlots:
    """与 complete_request 对应的槽位对象，用于直接驱动工作流。"""
    request = complete_request()
    return TaskSlots(
        application=request.application,
        environment=request.environment,
        database=request.database,
        table=request.table,
        query_sql=request.query_sql,
        planned_at=request.planned_at,
        planned_at_timezone=request.planned_at_timezone,
    )
