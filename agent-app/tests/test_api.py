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
        checkpoint_path=str(tmp_path / "agent-checkpoints.sqlite"),
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


def test_resume_endpoint_continues_from_the_interrupt(tmp_path: Path) -> None:
    """恢复接口从节点级中断继续，且不从头重跑。"""
    client = build_client(tmp_path)
    created = client.post(
        "/api/agent/tasks",
        headers=HEADERS,
        json={"requirement": REQUIREMENT, "schema_snapshot": SCHEMA_SNAPSHOT},
    )
    body = created.json()
    assert body["status"] == "NEEDS_INFO"
    assert body["awaiting_input"] is True
    task_id = body["task_id"]

    resumed = client.post(
        f"/api/agent/tasks/{task_id}/resume",
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
    assert resumed.status_code == 200
    final = resumed.json()
    assert final["status"] == "DRAFT_READY", final.get("error")
    assert final["resume_mode"] == "interrupt"
    kinds = [item["kind"] for item in final["events"]]
    assert kinds.count("screen_input") == 1, "恢复不得从头重跑"

    # 已到终态、没有待继续步骤：拒绝恢复。
    again = client.post(f"/api/agent/tasks/{task_id}/resume", headers=HEADERS)
    assert again.status_code == 409

    # 不存在或不属于调用方：一律 404，不做存在性探测。
    assert client.post("/api/agent/tasks/task_missing/resume", headers=HEADERS).status_code == 404


def test_confirm_endpoint_records_material_confirmation(tmp_path: Path) -> None:
    """确认接口：记录确认人；重复确认幂等；没有材料时 409；不存在时 404。"""
    client = build_client(tmp_path)
    created = client.post(
        "/api/agent/tasks",
        headers=HEADERS,
        json={
            "requirement": REQUIREMENT,
            "application": "order-service",
            "environment": "生产",
            "database": "postgresql",
            "table": "orders",
            "query_sql": SLOW_QUERY,
            "planned_at": PLANNED_AT.isoformat(),
            "planned_at_timezone": "Asia/Shanghai",
            "schema_snapshot": SCHEMA_SNAPSHOT,
        },
    )
    body = created.json()
    assert body["status"] == "DRAFT_READY", body.get("error")
    assert body["material_hash"], "存在材料时视图必须给出材料内容摘要"
    task_id = body["task_id"]

    confirmed = client.post(f"/api/agent/tasks/{task_id}/confirm", headers=HEADERS, json={"note": "已核对"})
    assert confirmed.status_code == 200
    records = confirmed.json()["confirmations"]
    assert len(records) == 1
    assert records[0]["confirmed_by"] == "alice"
    assert records[0]["material_hash"] == body["material_hash"]
    assert confirmed.json()["status"] == "DRAFT_READY", "确认不改变任务状态"

    # 幂等：重复确认不新增记录。
    again = client.post(f"/api/agent/tasks/{task_id}/confirm", headers=HEADERS)
    assert again.status_code == 200
    assert len(again.json()["confirmations"]) == 1

    # 没有可确认材料：409。
    bare = client.post("/api/agent/tasks", headers=HEADERS, json={"requirement": REQUIREMENT})
    assert client.post(f"/api/agent/tasks/{bare.json()['task_id']}/confirm", headers=HEADERS).status_code == 409
    assert client.post("/api/agent/tasks/task_missing/confirm", headers=HEADERS).status_code == 404
