"""provider 原生动作决策：真的由模型选择只读工具，边界仍由服务端强制。

这里用 `httpx.MockTransport` 模拟 OpenAI 兼容端点，**不依赖真实模型**。因此这些用例验证的是
契约与边界（白名单、schema 校验、身份注入、预算/超时/取消），**不是**模型质量证明——
真实模型质量评测在缺少凭据时必须标 `NOT_RUN`，不得用脚本化输出替代。

三条必须由代码而非提示词保证的性质：

1. 模型只能调用**服务端白名单内且注册为只读**的工具；注册表新增写工具不会自动获得调用能力；
2. 模型给出的参数必须通过该工具的 JSON Schema 校验，且不得夹带身份字段；
3. 身份与授权由服务端注入（`TrustedContext`），模型参数无法覆盖。
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import replace
from typing import Any, Callable

import httpx
import pytest

from app.config import Settings
from app.llm.provider import (
    ASK_USER_ACTION,
    FINISH_ACTION,
    MODEL_ACTION_TOOLS,
    ModelActionError,
    OpenAICompatibleProvider,
    action_tools_for_model,
)
from app.retrieval.corpus import build_retriever
from app.schemas.drafts import DatabaseKind, TaskSlots, ToolResult
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.workflow.investigate import (
    BoundedInvestigation,
    CallTool,
    ProviderPlanner,
    StopReason,
)
from tests.conftest import SCHEMA_SNAPSHOT, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")
SLOTS = TaskSlots(application="order-service", environment="生产", database=DatabaseKind.POSTGRESQL, table="orders")
SNAPSHOT = SCHEMA_SNAPSHOT


def configured(settings: Settings, **overrides: Any) -> Settings:
    base: dict[str, Any] = {
        "llm_base_url": "http://model-stub.invalid/v1",
        "llm_api_key": "stub-key",
        "llm_max_attempts": 1,
    }
    base.update(overrides)
    supported = set(Settings.__dataclass_fields__)
    return replace(settings, **{key: value for key, value in base.items() if key in supported})


def registry_specs(settings: Settings) -> list[dict[str, Any]]:
    """服务端只读工具规格——模型能看到的工具集合来源。"""
    retriever = build_retriever(settings.demo_dir)
    registry = Toolbox(settings=settings, retriever=retriever, schema_snapshot=SNAPSHOT).build()
    return registry.specs()


def hit(evidence_id: str, doc_id: str = "norms/sql-change-standards") -> dict[str, Any]:
    return {
        "evidence_id": evidence_id,
        "doc_id": doc_id,
        "title": "规范",
        "section": "并发建索引",
        "snippet": "片段",
        "source": f"{doc_id}.md",
        "version": "v1.0",
        "status": "active",
        "score": 1.0,
        "applicability": "PostgreSQL 生产库",
    }


def tool_call(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """构造一次 OpenAI 兼容的函数调用响应体。"""
    return {
        "choices": [
            {
                "message": {
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "call_1",
                            "type": "function",
                            "function": {"name": name, "arguments": json.dumps(arguments, ensure_ascii=False)},
                        }
                    ],
                }
            }
        ]
    }


class ModelEndpoint:
    """假模型端点：记录请求体与调用次数，支持同步/异步 handler。"""

    def __init__(self, handler: Callable[[dict[str, Any], int], Any]) -> None:
        self._handler = handler
        self.calls = 0
        self.requests: list[dict[str, Any]] = []

    def install(self, monkeypatch: pytest.MonkeyPatch) -> None:
        transport = httpx.MockTransport(self._dispatch)
        original = httpx.AsyncClient

        def factory(*args: Any, **kwargs: Any) -> httpx.AsyncClient:
            kwargs["transport"] = transport
            return original(*args, **kwargs)

        monkeypatch.setattr(httpx, "AsyncClient", factory)

    def _dispatch(self, request: httpx.Request) -> Any:
        self.calls += 1
        try:
            self.requests.append(json.loads(request.content.decode("utf-8")))
        except Exception:  # noqa: BLE001 - 请求体解析失败不影响被测行为
            self.requests.append({})
        return self._handler(self.requests[-1], self.calls)


def respond_with(*bodies: dict[str, Any]) -> Callable[[dict[str, Any], int], httpx.Response]:
    def handler(_body: dict[str, Any], call: int) -> httpx.Response:
        return httpx.Response(200, json=bodies[min(call, len(bodies)) - 1])

    return handler


class RecordingRegistry:
    """记录每次工具调用及其上下文；可按需挂住或失败。"""

    def __init__(
        self,
        hits: dict[str, list[dict[str, Any]]] | None = None,
        *,
        hang: tuple[str, ...] = (),
        failing: tuple[str, ...] = (),
    ) -> None:
        self.calls: list[tuple[str, dict[str, Any], TrustedContext]] = []
        self._hits = hits or {}
        self._hang = set(hang)
        self._failing = set(failing)

    async def call(self, name: str, args: Any, context: TrustedContext) -> ToolResult:
        self.calls.append((name, dict(args or {}), context))
        if name in self._hang:
            await asyncio.Event().wait()  # 永不返回，用于验证超时
        if name in self._failing:
            return ToolResult(ok=False, tool=name, error="注入的工具失败")
        return ToolResult(ok=True, tool=name, data={"hits": self._hits.get(name, [])})


def build_loop(
    settings: Settings,
    provider: Any,
    registry: Any,
    *,
    max_rounds: int = 4,
    max_tool_calls: int = 8,
    tool_timeout: float = 5.0,
) -> BoundedInvestigation:
    return BoundedInvestigation(
        planner=ProviderPlanner(provider, tools=registry_specs(settings)),
        registry=registry,
        context=CONTEXT,
        max_rounds=max_rounds,
        max_total_tool_calls=max_tool_calls,
        tool_timeout_seconds=tool_timeout,
    )


def investigate(loop: BoundedInvestigation) -> Any:
    return loop.run(requirement="订单索引", slots=SLOTS, schema_snapshot=SNAPSHOT)


# ---------------------------------------------------------------------------
# 白名单与注册表的一致性
# ---------------------------------------------------------------------------


def test_model_action_whitelist_matches_registry_read_only_tools(settings: Settings) -> None:
    """白名单必须与注册表的只读工具**恰好一致**。

    这条断言是防"注册表新增工具，模型自动获得调用能力"的闸门：
    新增只读工具会让它失败（迫使作者显式登记），新增写工具则不影响它。
    """
    read_only = {spec["name"] for spec in registry_specs(settings) if spec["read_only"]}
    assert set(MODEL_ACTION_TOOLS) == read_only, (
        "白名单与注册表只读工具不一致：新增只读工具必须显式登记，写工具不得进入白名单"
    )


def test_registry_write_tool_never_reaches_the_model(settings: Settings) -> None:
    """在注册表里加一个写工具，模型看不到它——白名单与 read_only 双重拦截。"""
    specs = list(registry_specs(settings))
    specs.append(
        {
            "name": "deploy_change",
            "description": "会执行部署的写工具",
            "parameters": {"type": "object", "properties": {}},
            "read_only": False,
        }
    )

    names = {item["function"]["name"] for item in action_tools_for_model(specs)}

    assert "deploy_change" not in names, "写工具不得下发给模型"
    assert set(MODEL_ACTION_TOOLS) <= names
    assert {FINISH_ACTION, ASK_USER_ACTION} <= names


# ---------------------------------------------------------------------------
# decide() 的直接契约
# ---------------------------------------------------------------------------


def test_decide_returns_a_validated_call_tool(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    endpoint = ModelEndpoint(respond_with(tool_call("search_norms", {"query": "索引", "limit": 2})))
    endpoint.install(monkeypatch)
    provider = OpenAICompatibleProvider(configured(settings))

    action = run(
        provider.decide(
            requirement="订单索引",
            slots=SLOTS,
            evidence=[],
            called_tools=[],
            round_index=1,
            observations=[],
            tools=registry_specs(settings),
        )
    )

    assert isinstance(action, CallTool)
    assert action.tool == "search_norms"
    assert action.args == {"query": "索引", "limit": 2}
    assert endpoint.calls == 1


def test_decide_exposes_only_whitelisted_tools_to_the_model(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    endpoint = ModelEndpoint(respond_with(tool_call(FINISH_ACTION, {})))
    endpoint.install(monkeypatch)
    provider = OpenAICompatibleProvider(configured(settings))

    run(
        provider.decide(
            requirement="订单索引",
            slots=SLOTS,
            evidence=[],
            called_tools=[],
            round_index=1,
            tools=registry_specs(settings),
        )
    )

    sent = endpoint.requests[0]
    names = {item["function"]["name"] for item in sent["tools"]}
    assert set(MODEL_ACTION_TOOLS) <= names
    assert sent["tool_choice"] == "auto"
    # 身份不得出现在发给模型的系统提示里被要求填写，也不作为工具参数下发。
    assert "X-Actor-Id" not in json.dumps(sent, ensure_ascii=False)
    assert "organization_id" not in json.dumps(sent.get("tools"), ensure_ascii=False)


def test_decide_rejects_forged_identity_fields(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    endpoint = ModelEndpoint(
        respond_with(tool_call("get_change_context", {"change_id": "chg_1", "organization_id": "org_evil"}))
    )
    endpoint.install(monkeypatch)
    provider = OpenAICompatibleProvider(configured(settings))

    with pytest.raises(ModelActionError) as error:
        run(
            provider.decide(
                requirement="r",
                slots=SLOTS,
                evidence=[],
                called_tools=[],
                round_index=1,
                tools=registry_specs(settings),
            )
        )

    assert "身份" in str(error.value)


# ---------------------------------------------------------------------------
# 循环：模型驱动多轮动作
# ---------------------------------------------------------------------------


def test_model_drives_multiple_rounds_of_read_only_tools(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """模型第 1 轮选检索、第 2 轮结束；工具确实被执行，证据确实来自这次调用。"""
    endpoint = ModelEndpoint(
        respond_with(
            tool_call("search_norms", {"query": "索引", "limit": 3}),
            tool_call(FINISH_ACTION, {}),
        )
    )
    endpoint.install(monkeypatch)
    registry = RecordingRegistry({"search_norms": [hit("norms/x#1")]})

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert [name for name, _args, _ctx in registry.calls] == ["search_norms"]
    assert registry.calls[0][1]["query"] == "索引"
    assert outcome.report.planner == "provider"
    assert outcome.report.stop_reason == StopReason.EVIDENCE_SUFFICIENT.value
    assert [item.evidence_id for item in outcome.evidence] == ["norms/x#1"]
    assert endpoint.calls == 2


def test_identity_is_injected_by_the_server_not_by_the_model(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """模型只能选工具；真正传给工具的上下文来自服务端，参数里也没有身份。"""
    endpoint = ModelEndpoint(
        respond_with(
            tool_call("get_change_context", {"change_id": "chg_1"}),
            tool_call(FINISH_ACTION, {}),
        )
    )
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    name, args, context = registry.calls[0]
    assert name == "get_change_context"
    assert args == {"change_id": "chg_1"}
    assert context is CONTEXT
    assert context.organization_id == "org_demo"


# ---------------------------------------------------------------------------
# 非法动作：一律拒绝，且不执行
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "arguments",
    [
        {"query": 123},  # 类型不符
        {"limit": 3},  # 缺少必填字段
        {"query": "x", "bogus": 1},  # 未知字段
    ],
)
def test_invalid_arguments_are_rejected_before_any_execution(
    settings: Settings, monkeypatch: pytest.MonkeyPatch, arguments: dict[str, Any]
) -> None:
    endpoint = ModelEndpoint(respond_with(tool_call("search_norms", arguments)))
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert registry.calls == [], "参数非法的动作不得被执行"
    assert outcome.report.stop_reason == StopReason.PLANNER_FAILED.value
    assert any("参数" in note for note in outcome.report.notes), outcome.report.notes


def test_unknown_tool_is_rejected(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    endpoint = ModelEndpoint(respond_with(tool_call("deploy_production", {"sql": "DROP TABLE users"})))
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert registry.calls == []
    assert outcome.report.stop_reason == StopReason.PLANNER_FAILED.value
    assert any("白名单" in note for note in outcome.report.notes), outcome.report.notes


@pytest.mark.parametrize("field", ["organization_id", "X-Actor-Id", "x_org_id", "application_id"])
def test_forged_identity_in_arguments_is_rejected(
    settings: Settings, monkeypatch: pytest.MonkeyPatch, field: str
) -> None:
    endpoint = ModelEndpoint(
        respond_with(tool_call("get_change_context", {"change_id": "chg_1", field: "org_evil"}))
    )
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert registry.calls == [], "夹带身份字段的动作不得被执行"
    assert outcome.report.stop_reason == StopReason.PLANNER_FAILED.value
    assert any("身份" in note for note in outcome.report.notes), outcome.report.notes


def test_model_finish_does_not_override_the_evidence_rule(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """模型主动结束调查不代表证据足够——完成条件仍由代码判定。"""
    endpoint = ModelEndpoint(respond_with(tool_call(FINISH_ACTION, {})))
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert outcome.report.missing_required


def test_model_can_ask_the_user_for_information(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    endpoint = ModelEndpoint(respond_with(tool_call(ASK_USER_ACTION, {"reason": "请补充目标表的结构快照"})))
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert registry.calls == []
    assert outcome.report.stop_reason == StopReason.INSUFFICIENT_EVIDENCE.value
    assert outcome.report.clarification_requests == ["请补充目标表的结构快照"]


# ---------------------------------------------------------------------------
# 预算、超时与取消
# ---------------------------------------------------------------------------


def test_tool_call_budget_bounds_model_driven_calls(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """模型每轮都要求调用，累计上限仍然是硬边界：超出预算的动作不会被真的执行。"""
    endpoint = ModelEndpoint(
        lambda _body, call: httpx.Response(200, json=tool_call("search_norms", {"query": f"q{call}"}))
    )
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    loop = build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry, max_rounds=10, max_tool_calls=2)
    outcome = run(investigate(loop))

    assert len(registry.calls) == 2, f"超预算的动作不得执行，实际 {registry.calls}"
    assert outcome.report.stop_reason == StopReason.TOOL_CALLS_EXHAUSTED.value


def test_model_request_timeout_is_reported_without_hanging(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    def handler(_body: dict[str, Any], _call: int) -> httpx.Response:
        raise httpx.TimeoutException("stub timeout")

    endpoint = ModelEndpoint(handler)
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert outcome.report.stop_reason == StopReason.PLANNER_FAILED.value
    assert any("决策者未能给出可执行的动作" in note for note in outcome.report.notes)
    assert registry.calls == []


def test_model_chosen_tool_timeout_is_treated_as_failure(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    endpoint = ModelEndpoint(respond_with(tool_call("search_norms", {"query": "慢"})))
    endpoint.install(monkeypatch)
    registry = RecordingRegistry(hang=("search_norms",))

    loop = build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry, tool_timeout=0.01)
    outcome = run(investigate(loop))

    assert outcome.report.stop_reason == StopReason.TOOL_FAILED.value
    assert outcome.report.observations
    assert outcome.report.observations[0].kind == "timeout"


def test_cancellation_stops_further_model_requests(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """取消在模型请求进行中生效：不得再发出后续请求。"""

    async def scenario() -> int:
        started = asyncio.Event()

        async def handler(_body: dict[str, Any], _call: int) -> httpx.Response:
            started.set()
            await asyncio.Event().wait()  # 永不返回
            raise AssertionError("unreachable")  # pragma: no cover

        endpoint = ModelEndpoint(handler)
        endpoint.install(monkeypatch)
        loop = build_loop(
            settings,
            OpenAICompatibleProvider(configured(settings, llm_max_attempts=3)),
            RecordingRegistry(),
        )
        task = asyncio.create_task(investigate(loop))

        await asyncio.wait_for(started.wait(), timeout=5)
        assert endpoint.calls == 1
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        for _ in range(20):
            await asyncio.sleep(0)
        return endpoint.calls

    assert run(scenario()) == 1, "取消之后不得再发出模型请求"


# ---------------------------------------------------------------------------
# usage：由 provider 提供才记录，缺失即 unknown
# ---------------------------------------------------------------------------


def test_usage_is_recorded_when_the_provider_reports_it(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    body = tool_call(FINISH_ACTION, {})
    body["usage"] = {"prompt_tokens": 120, "completion_tokens": 30}
    endpoint = ModelEndpoint(respond_with(body))
    endpoint.install(monkeypatch)
    registry = RecordingRegistry()

    outcome = run(investigate(build_loop(settings, OpenAICompatibleProvider(configured(settings)), registry)))

    assert outcome.report.usage.known is True
    assert outcome.report.usage.prompt_tokens == 120
    assert outcome.report.usage.completion_tokens == 30
