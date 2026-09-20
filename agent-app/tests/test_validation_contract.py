"""HTTP 层的校验错误契约：给一句人话，且不回显提交内容。

上面那层是 `describe_request_error` 的单元测试；这里证明它**真的接上了路由**——
默认的 FastAPI 422 体是结构化数组，工作台当初只能把它 stringify 成一大坨 JSON。
"""

from __future__ import annotations

from pathlib import Path

from tests.conftest import REQUIREMENT
from tests.test_api import HEADERS, build_client


def test_over_long_requirement_is_rejected_gently(tmp_path: Path) -> None:
    client = build_client(tmp_path)

    response = client.post("/api/agent/tasks", json={"requirement": "长" * 4001}, headers=HEADERS)

    assert response.status_code == 422
    detail = response.json()["detail"]
    assert isinstance(detail, str), f"必须是可读字符串，而不是结构化数组：{detail!r}"
    assert "4000" in detail and "4001" in detail, detail
    assert "长" * 20 not in detail, "错误响应回显了提交内容"


def test_invalid_database_value_is_rejected_gently(tmp_path: Path) -> None:
    client = build_client(tmp_path)

    response = client.post(
        "/api/agent/tasks",
        json={"requirement": REQUIREMENT, "database": "oracle"},
        headers=HEADERS,
    )

    assert response.status_code == 422
    detail = response.json()["detail"]
    assert isinstance(detail, str), detail
    assert "数据库类型" in detail, detail
    assert "oracle" not in detail, "错误响应回显了提交内容"


def test_valid_request_still_reaches_the_service(tmp_path: Path) -> None:
    """对照组：正常请求不受这个处理器影响。"""
    client = build_client(tmp_path)

    response = client.post("/api/agent/tasks", json={"requirement": REQUIREMENT}, headers=HEADERS)

    assert response.status_code == 202, response.text
