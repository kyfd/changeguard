"""3.1.2 白盒补充测试：盯住"只有真实分支才走到"的代码路径。

已有测试（test_application_binding / test_knowledge）把 `_verify_application` 与
`_application_authorized` 整体 monkeypatch 掉，因此**真实 HTTP 分支没有覆盖**：

- `_verify_application`：缺应用 / 缺共享密钥 / 治理不可达 / 非 200 / 返回体不是 JSON /
  名称为空 六条失败关闭路径，以及成功时**不信任客户端名称**（名称只取治理服务回填值）；
- `_application_authorized`：缺应用直接 True（组织通用查询）/ 缺密钥 False / 异常 False /
  非 200 False；
- `authorized_application_or_empty`：未授权时返回空串而不抛错。

这些分支正是"失败关闭"与"名称不作为授权依据"的实现处：把它们锁住，才不会在后续
重构里退化成"治理服务不可达时默认放行"或"信任调用方提交的名称"。
"""

from __future__ import annotations

from dataclasses import replace
from typing import Any, Callable

import httpx
import pytest

from app.config import Settings
from app.service import AgentService
from app.tools.registry import TrustedContext
from tests.conftest import run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")
GOVERNANCE_URL = "http://governance.internal:8080"


def install_transport(
    monkeypatch: pytest.MonkeyPatch,
    respond: Callable[[httpx.Request], httpx.Response],
) -> list[httpx.Request]:
    """把 httpx 传输层换成本地假实现，并记录实际发出的请求。"""
    captured: list[httpx.Request] = []

    def recording(request: httpx.Request) -> httpx.Response:
        captured.append(request)
        return respond(request)

    transport = httpx.MockTransport(recording)
    original = httpx.AsyncClient

    def factory(*args: Any, **kwargs: Any) -> httpx.AsyncClient:
        kwargs["transport"] = transport
        return original(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", factory)
    return captured


def governed(settings: Settings, token: str = "shared-secret") -> AgentService:
    """把服务指向假的治理地址并配上共享密钥。"""
    return AgentService(replace(settings, upstream_token=token, governance_base_url=GOVERNANCE_URL))


# -- _verify_application：六条失败关闭路径 -------------------------------------


def test_verify_application_without_shared_secret_never_calls_governance(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """没有共享密钥时不得发请求，也不能把应用当成已授权。"""
    captured = install_transport(monkeypatch, lambda _request: httpx.Response(200, json={"name": "订单服务"}))
    service = governed(settings, token="")

    assert run(service._verify_application(CONTEXT, "app_order")) is None
    assert captured == [], "缺共享密钥时不得发起任何核对请求"


def test_verify_application_without_application_id_returns_none(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """空应用 ID 不是"通用可见"，而是核对失败（失败关闭）。"""
    captured = install_transport(monkeypatch, lambda _request: httpx.Response(200, json={"name": "订单服务"}))
    service = governed(settings)

    assert run(service._verify_application(CONTEXT, "   ")) is None
    assert captured == []


def test_verify_application_returns_none_when_governance_unreachable(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """治理服务不可达必须失败关闭，不能放行。"""
    def boom(_request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("governance down")

    captured = install_transport(monkeypatch, boom)
    service = governed(settings)

    assert run(service._verify_application(CONTEXT, "app_order")) is None
    assert len(captured) == 1


@pytest.mark.parametrize("status_code", [401, 403, 404, 500])
def test_verify_application_returns_none_for_any_non_200(
    settings: Settings, monkeypatch: pytest.MonkeyPatch, status_code: int
) -> None:
    """任何非 200 都按未授权处理（不区分"不存在"与"无权限"，避免探测）。"""
    install_transport(monkeypatch, lambda _request: httpx.Response(status_code, json={}))
    service = governed(settings)

    assert run(service._verify_application(CONTEXT, "app_order")) is None


def test_verify_application_returns_none_when_body_is_not_json(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """200 但响应体不是 JSON：解析失败同样失败关闭。"""
    install_transport(monkeypatch, lambda _request: httpx.Response(200, text="<html>not json</html>"))
    service = governed(settings)

    assert run(service._verify_application(CONTEXT, "app_order")) is None


def test_verify_application_uses_name_from_governance_not_from_caller(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """展示名称必须取治理服务回填值：调用方提交的名称不参与授权与展示。"""
    captured = install_transport(
        monkeypatch, lambda _request: httpx.Response(200, json={"name": "订单服务"})
    )
    service = governed(settings)

    verified = run(service._verify_application(CONTEXT, "app_order"))

    assert verified == {"id": "app_order", "name": "订单服务"}
    request = captured[0]
    assert request.headers["X-Agent-Upstream-Token"] == "shared-secret"
    # 委托身份由服务端注入；浏览器凭据不得带上。
    assert request.headers["X-Actor-Id"] == "alice"
    assert request.headers["X-Org-Id"] == "org_demo"
    assert "cookie" not in {key.lower() for key in request.headers}


def test_verify_application_tolerates_missing_name_field(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """治理服务没回填名称时保持空串，不允许编造名称。"""
    install_transport(monkeypatch, lambda _request: httpx.Response(200, json={}))
    service = governed(settings)

    verified = run(service._verify_application(CONTEXT, "app_order"))

    assert verified == {"id": "app_order", "name": ""}


# -- _application_authorized：组织通用查询与失败关闭 ----------------------------


def test_application_authorized_is_true_for_org_wide_queries(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """未指定应用 = 组织通用查询：不需要应用授权，也不发核对请求。"""
    captured = install_transport(monkeypatch, lambda _request: httpx.Response(200, json={}))
    service = governed(settings)

    assert run(service._application_authorized(CONTEXT, "")) is True
    assert captured == []


def test_application_authorized_false_without_shared_secret(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    captured = install_transport(monkeypatch, lambda _request: httpx.Response(200, json={}))
    service = governed(settings, token="")

    assert run(service._application_authorized(CONTEXT, "app_order")) is False
    assert captured == []


def test_application_authorized_false_when_governance_unreachable(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    def boom(_request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("governance down")

    install_transport(monkeypatch, boom)
    service = governed(settings)

    assert run(service._application_authorized(CONTEXT, "app_order")) is False


@pytest.mark.parametrize("status_code", [403, 500])
def test_application_authorized_false_for_non_200(
    settings: Settings, monkeypatch: pytest.MonkeyPatch, status_code: int
) -> None:
    install_transport(monkeypatch, lambda _request: httpx.Response(status_code, json={}))
    service = governed(settings)

    assert run(service._application_authorized(CONTEXT, "app_order")) is False


def test_application_authorized_true_only_on_200(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    captured = install_transport(monkeypatch, lambda _request: httpx.Response(200, json={"name": "订单服务"}))
    service = governed(settings)

    assert run(service._application_authorized(CONTEXT, "app_order")) is True
    # 请求走内部只读接口，路径里带应用 ID。
    assert captured[0].url.path.endswith("/api/agent-tools/applications/app_order")


# -- _authorized_application：展示用的宽松版本 --------------------------------


def test_authorized_application_returns_empty_when_not_authorized(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """展示辅助函数不得把未授权当异常抛出，也不得回显未授权应用的 ID。"""
    install_transport(monkeypatch, lambda _request: httpx.Response(403, json={}))
    service = governed(settings)

    assert run(service._authorized_application(CONTEXT, "app_order")) == ""


def test_authorized_application_returns_id_when_authorized(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    install_transport(monkeypatch, lambda _request: httpx.Response(200, json={"name": "订单服务"}))
    service = governed(settings)

    assert run(service._authorized_application(CONTEXT, "app_order")) == "app_order"
    # 未指定应用时是组织通用范围：直接返回空串，不发核对请求。
    assert run(service._authorized_application(CONTEXT, "")) == ""
