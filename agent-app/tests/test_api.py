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


def test_tasks_are_isolated_per_user_and_org(tmp_path: Path) -> None:
    """成员只能触达自己组织里自己的任务：读取、列表、续作、取消都按归属过滤。"""
    client = build_client(tmp_path)
    created = client.post("/api/agent/tasks", headers=HEADERS, json={"requirement": REQUIREMENT})
    assert created.status_code == 202
    task_id = created.json()["task_id"]

    # 同组织的其他成员：看不到、碰不到；归属不一致按"不存在"处理，不泄露存在性。
    other = {"X-Actor-Id": "mallory", "X-Org-Id": "org_demo"}
    assert client.get(f"/api/agent/tasks/{task_id}", headers=other).status_code == 404
    assert client.get("/api/agent/tasks", headers=other).json() == []
    assert client.post(f"/api/agent/tasks/{task_id}/clarify", headers=other, json={"table": "orders"}).status_code == 404
    assert client.post(f"/api/agent/tasks/{task_id}/cancel", headers=other).status_code == 404

    # 跨组织同样不可见。
    foreign = {"X-Actor-Id": "alice", "X-Org-Id": "org_other"}
    assert client.get(f"/api/agent/tasks/{task_id}", headers=foreign).status_code == 404
    assert client.get("/api/agent/tasks", headers=foreign).json() == []

    # 归属人自己可以继续使用。
    assert client.get(f"/api/agent/tasks/{task_id}", headers=HEADERS).status_code == 200
    owner_list = client.get("/api/agent/tasks", headers=HEADERS).json()
    assert [item["task_id"] for item in owner_list] == [task_id]
