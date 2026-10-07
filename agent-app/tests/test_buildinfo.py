"""构建身份（Agent 侧）的失败关闭回归。

这些断言锁住三件事：

- 未注入 `AGENT_BUILD_*` 时如实报告 unknown，**不**用版本号或 dev 冒充；
- `provenance_verified` 只有在四项都像发布身份时才为真；
- `/healthz` 暴露的 build 字段是只读身份，不含任何业务数据，也不需要额外身份。
"""

from __future__ import annotations

from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app import buildinfo
from app.config import Settings
from app.main import create_app
from tests.conftest import DEMO_DIR

BUILD_ENV = (
    "AGENT_BUILD_VERSION",
    "AGENT_BUILD_COMMIT",
    "AGENT_BUILD_SOURCE_SHA256",
    "AGENT_BUILD_BUILT_AT",
)


@pytest.fixture(autouse=True)
def clear_build_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """每个用例都从"未注入"开始，避免宿主机环境串味。"""
    for name in BUILD_ENV:
        monkeypatch.delenv(name, raising=False)


def build_client(tmp_path: Path) -> TestClient:
    settings = Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(tmp_path / "agent-tasks.json"),
        execution_mode="inline",
        allow_header_identity=True,
        upstream_token="unit-test-secret",
    )
    return TestClient(create_app(settings), headers={"X-Agent-Upstream-Token": "unit-test-secret"})


def test_absent_identity_is_reported_as_unknown() -> None:
    info = buildinfo.current()
    assert info.version == "unknown"
    assert info.commit == "unknown"
    assert info.source_sha256 == "unknown"
    assert info.built_at == "unknown"
    assert info.provenance_verified is False
    assert info.identified is False


def test_incomplete_identity_keeps_provenance_false(monkeypatch: pytest.MonkeyPatch) -> None:
    """只有版本、没有 commit / 摘要 / 时间时不能"部分可信"。"""
    monkeypatch.setenv("AGENT_BUILD_VERSION", "3.1.4")
    info = buildinfo.current()
    assert info.version == "3.1.4"
    assert info.identified is True
    assert info.provenance_verified is False


def test_dev_version_is_not_a_comparable_identity(monkeypatch: pytest.MonkeyPatch) -> None:
    """"dev" 是开发构建，不是版本号：不能被当成可核对的发布身份。"""
    monkeypatch.setenv("AGENT_BUILD_VERSION", "dev")
    info = buildinfo.current()
    assert info.version == "dev"
    assert info.identified is False


def test_full_identity_verifies_and_lowercases_hashes(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_BUILD_VERSION", "3.1.4")
    monkeypatch.setenv("AGENT_BUILD_COMMIT", "A" * 40)
    monkeypatch.setenv("AGENT_BUILD_SOURCE_SHA256", "B" * 64)
    monkeypatch.setenv("AGENT_BUILD_BUILT_AT", "2026-10-06T08:40:04Z")
    info = buildinfo.current()
    assert info.commit == "a" * 40, "提交号统一小写，避免大小写造成假不匹配"
    assert info.source_sha256 == "b" * 64
    assert info.provenance_verified is True


def test_malformed_hash_is_not_verified(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_BUILD_VERSION", "3.1.4")
    monkeypatch.setenv("AGENT_BUILD_COMMIT", "not-a-commit")
    monkeypatch.setenv("AGENT_BUILD_SOURCE_SHA256", "z" * 64)
    monkeypatch.setenv("AGENT_BUILD_BUILT_AT", "2026-10-06T08:40:04Z")
    assert buildinfo.current().provenance_verified is False


def test_blank_value_falls_back_to_unknown(monkeypatch: pytest.MonkeyPatch) -> None:
    """空白不等于已配置：不能把空串当成一个真实版本。"""
    monkeypatch.setenv("AGENT_BUILD_VERSION", "   ")
    info = buildinfo.current()
    assert info.version == "unknown"
    assert info.identified is False


def test_naive_timestamp_is_not_verified(monkeypatch: pytest.MonkeyPatch) -> None:
    """无时区的时间戳不能与发布时间对账。"""
    for naive in ("2026-10-06T08:40:04", "2026-10-06"):
        with monkeypatch.context() as scope:
            scope.setenv("AGENT_BUILD_VERSION", "3.1.4")
            scope.setenv("AGENT_BUILD_COMMIT", "c" * 40)
            scope.setenv("AGENT_BUILD_SOURCE_SHA256", "d" * 64)
            scope.setenv("AGENT_BUILD_BUILT_AT", naive)
            assert buildinfo.current().provenance_verified is False, naive


def test_offset_timestamp_is_verified(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_BUILD_VERSION", "3.1.4")
    monkeypatch.setenv("AGENT_BUILD_COMMIT", "c" * 40)
    monkeypatch.setenv("AGENT_BUILD_SOURCE_SHA256", "d" * 64)
    monkeypatch.setenv("AGENT_BUILD_BUILT_AT", "2026-10-06T08:40:04+08:00")
    assert buildinfo.current().provenance_verified is True


def test_garbage_version_is_never_identified(monkeypatch: pytest.MonkeyPatch) -> None:
    """identified 只认真实版本号；乱写的字符串不能冒充发布身份。"""
    for garbage in ("latest", "3.1", "v3.1.4", "3_1_4", "3.1.4-rc1"):
        with monkeypatch.context() as scope:
            scope.setenv("AGENT_BUILD_VERSION", garbage)
            assert buildinfo.current().identified is False, garbage
            assert buildinfo.current().provenance_verified is False, garbage


def test_garbage_version_is_not_verified_even_with_valid_other_fields(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """其余三项合法时，非法版本仍不能通过 provenance_verified。"""
    monkeypatch.setenv("AGENT_BUILD_VERSION", "latest")
    monkeypatch.setenv("AGENT_BUILD_COMMIT", "a" * 40)
    monkeypatch.setenv("AGENT_BUILD_SOURCE_SHA256", "b" * 64)
    monkeypatch.setenv("AGENT_BUILD_BUILT_AT", "2026-10-06T00:00:00Z")
    info = buildinfo.current()
    assert info.identified is False
    assert info.provenance_verified is False


def test_hyphenated_version_is_identified(monkeypatch: pytest.MonkeyPatch) -> None:
    """连字符版本（发布构建格式）是可识别的真实版本号。"""
    monkeypatch.setenv("AGENT_BUILD_VERSION", "3-1-4")
    assert buildinfo.current().identified is True


def test_healthz_exposes_build_identity(tmp_path: Path) -> None:
    client = build_client(tmp_path)
    payload = client.get("/api/agent/healthz").json()
    build = payload["build"]
    assert set(build) == {"version", "commit", "source_sha256", "built_at", "provenance_verified"}
    assert build["version"] == "unknown", "测试环境未注入身份时应如实报告"
    assert payload["status"] == "ok"


def test_healthz_reflects_injected_identity(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_BUILD_VERSION", "3.1.4")
    monkeypatch.setenv("AGENT_BUILD_COMMIT", "c" * 40)
    monkeypatch.setenv("AGENT_BUILD_SOURCE_SHA256", "d" * 64)
    monkeypatch.setenv("AGENT_BUILD_BUILT_AT", "2026-10-06T08:40:04Z")
    client = build_client(tmp_path)
    build = client.get("/api/agent/healthz").json()["build"]
    assert build["version"] == "3.1.4"
    assert build["provenance_verified"] is True


def test_build_identity_does_not_leak_secrets(tmp_path: Path) -> None:
    """健康响应是只读身份：不得回显共享密钥或模型 key。"""
    client = build_client(tmp_path)
    body = client.get("/api/agent/healthz").text
    assert "unit-test-secret" not in body
