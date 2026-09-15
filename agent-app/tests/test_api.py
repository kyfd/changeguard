"""HTTP 层测试。"""

from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from app.config import Settings
from app.main import create_app
from tests.conftest import DEMO_DIR, PLANNED_AT, REQUIREMENT, SCHEMA_SNAPSHOT, SLOW_QUERY

HEADERS = {"X-Actor-Id": "alice", "X-Org-Id": "org_demo"}

EXPECTED_TOOLS = {
    "get_schema_snapshot",
    "scan_sql",
    "search_norms",
    "search_historical_changes",
    "get_change_context",
    "get_rule_findings",
    "get_experiment_report",
}


def build_client(tmp_path: Path, *, allow_identity: bool = True) -> TestClient:
    settings = Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode="inline",
        allow_header_identity=allow_identity,
    )
    return TestClient(create_app(settings))


def test_identity_is_denied_by_default(tmp_path: Path) -> None:
    """不安全的默认值是关闭的：没配置身份来源就拒绝。"""
    client = build_client(tmp_path, allow_identity=False)
    response = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)
    assert response.status_code == 401


def test_health_and_tool_listing(tmp_path: Path) -> None:
    client = build_client(tmp_path)

    health = client.get("/api/agent/healthz")
    assert health.status_code == 200
    assert health.json()["status"] == "ok"

    tools = client.get("/api/agent/tools").json()["tools"]
    assert {item["name"] for item in tools} == EXPECTED_TOOLS
    assert all(item["read_only"] for item in tools), "Agent 可见的工具必须全部只读"


def test_ask_then_clarify_reaches_draft(tmp_path: Path) -> None:
    client = build_client(tmp_path)

    created = client.post(
        "/api/agent/tasks",
        headers=HEADERS,
        json={"requirement": REQUIREMENT, "schema_snapshot": SCHEMA_SNAPSHOT},
    )
    assert created.status_code == 202
    body = created.json()
    assert body["status"] == "NEEDS_INFO"
    task_id = body["task_id"]

    clarified = client.post(
        f"/api/agent/tasks/{task_id}/clarify",
        headers=HEADERS,
        json={
            "application": "order-service",
            "environment": "生产",
            "database": "postgresql",
            "table": "orders",
            "query_sql": SLOW_QUERY,
            "planned_at": PLANNED_AT.isoformat(),
            "planned_at_timezone": "Asia/Shanghai",
        },
    )
    assert clarified.status_code == 200
    final = clarified.json()
    assert final["status"] == "DRAFT_READY", final.get("error")
    assert "CONCURRENTLY" in final["draft"]["sql"].upper()
    assert final["draft"]["evidence"], "草案必须带可追溯引用"

    detail = client.get(f"/api/agent/tasks/{task_id}", headers=HEADERS)
    assert detail.status_code == 200
    assert detail.json()["task_id"] == task_id


def test_cancel_marks_task_cancelled(tmp_path: Path) -> None:
    client = build_client(tmp_path)
    created = client.post("/api/agent/tasks", headers=HEADERS, json={"requirement": REQUIREMENT})
    task_id = created.json()["task_id"]

    cancelled = client.post(f"/api/agent/tasks/{task_id}/cancel", headers=HEADERS)
    assert cancelled.status_code == 200
    assert cancelled.json()["status"] == "CANCELLED"


def test_unknown_task_returns_404(tmp_path: Path) -> None:
    client = build_client(tmp_path)
    assert client.get("/api/agent/tasks/task_missing", headers=HEADERS).status_code == 404
    assert client.post("/api/agent/tasks/task_missing/cancel", headers=HEADERS).status_code == 404


def test_clarify_on_finished_task_conflicts(tmp_path: Path) -> None:
    client = build_client(tmp_path)
    response = client.post(
        "/api/agent/tasks",
        headers=HEADERS,
        json={"requirement": REQUIREMENT, "schema_snapshot": SCHEMA_SNAPSHOT},
    )
    task_id = response.json()["task_id"]
    client.post(f"/api/agent/tasks/{task_id}/cancel", headers=HEADERS)
    conflicted = client.post(f"/api/agent/tasks/{task_id}/clarify", headers=HEADERS, json={"table": "orders"})
    assert conflicted.status_code == 409
