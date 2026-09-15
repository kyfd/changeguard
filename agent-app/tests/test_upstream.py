"""上游凭据与"内部后端"形态测试。

合并部署后只有 ChangeGuard 治理服务对外，本服务只应被它调用。这些断言锁住：

- 配置了共享密钥后，缺少或伪造凭据的请求一律拒绝（覆盖**每一个**路由）；
- 本服务不再对外提供界面，只有治理服务那一个入口；
- 身份仍未关闭：即使带着上游凭据，缺失身份来源依然是 401。
"""

from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from app.config import Settings
from app.main import create_app
from tests.conftest import DEMO_DIR

UPSTREAM_HEADERS = {"X-Agent-Upstream-Token": "shared-secret-for-agent-service"}
IDENTITY_HEADERS = {"X-Actor-Id": "alice", "X-Org-Id": "org_demo"}


def build_client(
    tmp_path: Path,
    *,
    upstream_token: str = "",
    allow_identity: bool = True,
) -> TestClient:
    settings = Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode="inline",
        allow_header_identity=allow_identity,
        upstream_token=upstream_token,
    )
    return TestClient(create_app(settings))


def test_upstream_token_is_enforced_on_every_agent_route(tmp_path: Path) -> None:
    client = build_client(tmp_path, upstream_token=UPSTREAM_HEADERS["X-Agent-Upstream-Token"])

    for method, path in (
        ("GET", "/api/agent/healthz"),
        ("GET", "/api/agent/tools"),
        ("GET", "/api/agent/tasks"),
        ("POST", "/api/agent/tasks"),
    ):
        missing = client.request(method, path, headers=IDENTITY_HEADERS, json={} if method == "POST" else None)
        assert missing.status_code == 401, f"{method} {path} 缺少上游凭据时必须拒绝"

        forged = client.request(
            method,
            path,
            headers={**IDENTITY_HEADERS, "X-Agent-Upstream-Token": "forged"},
            json={} if method == "POST" else None,
        )
        assert forged.status_code == 401, f"{method} {path} 伪造上游凭据时必须拒绝"


def test_correct_upstream_token_is_accepted(tmp_path: Path) -> None:
    client = build_client(tmp_path, upstream_token=UPSTREAM_HEADERS["X-Agent-Upstream-Token"])

    health = client.get("/api/agent/healthz", headers=UPSTREAM_HEADERS)
    assert health.status_code == 200
    assert health.json()["status"] == "ok"


def test_token_check_is_off_when_not_configured(tmp_path: Path) -> None:
    """本机开发不配密钥时不启用该检查——这是显式选择，不是遗漏。"""
    client = build_client(tmp_path, upstream_token="")
    assert client.get("/api/agent/healthz").status_code == 200


def test_identity_is_still_required_even_with_upstream_token(tmp_path: Path) -> None:
    client = build_client(
        tmp_path,
        upstream_token=UPSTREAM_HEADERS["X-Agent-Upstream-Token"],
        allow_identity=False,
    )
    response = client.get("/api/agent/tasks", headers=UPSTREAM_HEADERS)
    assert response.status_code == 401


def test_service_does_not_expose_a_second_ui(tmp_path: Path) -> None:
    """界面由治理服务同源提供；这里多一个入口就会多一份会漂移的副本。"""
    client = build_client(tmp_path)

    assert client.get("/ui/").status_code == 404
    assert client.get("/ui/app.js").status_code == 404

    root = client.get("/")
    assert root.status_code == 200
    payload = root.json()
    assert payload["role"] == "internal backend"
    assert "/agent/" in payload["ui"]


def test_static_workbench_files_are_no_longer_shipped_here() -> None:
    static_dir = Path(__file__).resolve().parents[1] / "app" / "static"
    if static_dir.exists():
        assert not any(static_dir.iterdir()), "工作台资源应只存在于 Go 侧 internal/httpapi/web/agent/"
