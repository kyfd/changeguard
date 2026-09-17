"""模型调用的重试分层与成本上限。

改造前 provider 与工作流**共用同一个** `llm_max_attempts`：
provider 内部重试 N 次，工作流又用同一个 N 再套一层，于是一次生成最多打 N×N 次调用。
默认 N=2 时就是 4 次——成本与延迟被悄悄平方，而且没有任何一层可以单独调整。

同时，权限拒绝、参数错误这类失败**重试不会改变结果**，原来也会被两层各重试一遍。

这些用例断言的是一次生成实际发出的**模型调用次数**，因此改前的失败是真实的成本问题，
而不是"少了某个方法或参数"。
"""

from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path
from typing import Any, Callable

import httpx
import pytest

from app.config import Settings
from app.llm.provider import DraftRequest, OpenAICompatibleProvider
from app.retrieval.corpus import build_retriever
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.workflow.graph import DraftWorkflow, WorkflowDeps
from tests.conftest import complete_request, complete_slots, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

VALID_DRAFT = json.dumps(
    {
        "sql": (
            "SET lock_timeout = '3s';\n\n"
            "CREATE INDEX CONCURRENTLY idx_orders_user_created ON orders (user_id, created_at DESC);"
        ),
        "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
        "assumptions": [],
        "open_questions": [],
        "advisory_risk": "LOW",
        "advice_summary": "占位结论",
    },
    ensure_ascii=False,
)


class ModelStub:
    """记录调用次数的假模型端点。"""

    def __init__(self, status_code: int) -> None:
        self.status_code = status_code
        self.calls = 0

    def install(self, monkeypatch: pytest.MonkeyPatch) -> None:
        def handler(_request: httpx.Request) -> httpx.Response:
            self.calls += 1
            if self.status_code == 200:
                return httpx.Response(200, json={"choices": [{"message": {"content": VALID_DRAFT}}]})
            return httpx.Response(self.status_code, json={"error": "stub"})

        transport = httpx.MockTransport(handler)
        original = httpx.AsyncClient

        def factory(*args: Any, **kwargs: Any) -> httpx.AsyncClient:
            kwargs["transport"] = transport
            return original(*args, **kwargs)

        monkeypatch.setattr(httpx, "AsyncClient", factory)


def configured(settings: Settings, **overrides: Any) -> Settings:
    base: dict[str, Any] = {
        "llm_base_url": "http://model-stub.invalid/v1",
        "llm_api_key": "stub-key",
        "max_revisions": 0,
    }
    base.update(overrides)
    # 只传 Settings 真正支持的字段。改造前不存在 `draft_parse_attempts`，
    # 若直接传会抛 TypeError，那样"改前失败"反映的就成了缺字段而不是调用次数。
    supported = set(Settings.__dataclass_fields__)
    return replace(settings, **{key: value for key, value in base.items() if key in supported})


def workflow_state(settings: Settings) -> dict[str, Any]:
    request = complete_request()
    return {
        "task_id": "t_retry",
        "requirement": request.requirement,
        "slots": request.model_dump(mode="json"),
        "schema_snapshot": request.schema_snapshot or "",
        "max_revisions": settings.max_revisions,
        "events": [],
        "revisions": 0,
    }


def build_workflow(settings: Settings, provider: Any) -> DraftWorkflow:
    retriever = build_retriever(settings.demo_dir)
    factory: Callable[[str], Toolbox] = lambda snapshot: Toolbox(  # noqa: E731
        settings=settings, retriever=retriever, schema_snapshot=snapshot
    )
    return DraftWorkflow(
        WorkflowDeps(
            settings=settings,
            provider=provider,
            trusted_context=CONTEXT,
            toolbox_factory=factory,
        )
    )


def draft_request() -> DraftRequest:
    return DraftRequest(requirement="占位需求", slots=complete_slots())


# ---------------------------------------------------------------------------
# provider 层：只重试值得重试的失败
# ---------------------------------------------------------------------------


def test_non_retryable_status_is_not_retried(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    """权限/参数类失败重试不会改变结果，不应重复发送。"""
    stub = ModelStub(401)
    stub.install(monkeypatch)
    provider = OpenAICompatibleProvider(configured(settings, llm_max_attempts=3))

    with pytest.raises(Exception):  # noqa: BLE001 - 期望显式失败
        run(provider.generate(draft_request()))

    assert stub.calls == 1, f"401 重试不会改变结果，实际却调用了 {stub.calls} 次"


@pytest.mark.parametrize("status_code", [429, 503])
def test_retryable_status_is_retried_within_the_bound(
    settings: Settings, monkeypatch: pytest.MonkeyPatch, status_code: int
) -> None:
    """限流与服务端暂时故障应当重试，且不超过配置次数。"""
    stub = ModelStub(status_code)
    stub.install(monkeypatch)
    provider = OpenAICompatibleProvider(configured(settings, llm_max_attempts=3))

    with pytest.raises(Exception):  # noqa: BLE001
        run(provider.generate(draft_request()))

    assert stub.calls == 3, f"{status_code} 属于暂时故障，应重试到配置上限，实际 {stub.calls} 次"


def test_successful_call_is_sent_once(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    stub = ModelStub(200)
    stub.install(monkeypatch)
    provider = OpenAICompatibleProvider(configured(settings, llm_max_attempts=3))

    assert run(provider.generate(draft_request()))
    assert stub.calls == 1


# ---------------------------------------------------------------------------
# 两层叠加后的总成本
# ---------------------------------------------------------------------------


def test_workflow_does_not_multiply_a_non_retryable_failure(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    """不可重试的失败只会打一次模型调用，而不是被两层各重试一遍。"""
    stub = ModelStub(401)
    stub.install(monkeypatch)
    scoped = configured(settings, llm_max_attempts=2, draft_parse_attempts=2)
    workflow = build_workflow(scoped, OpenAICompatibleProvider(scoped))

    result = run(workflow.run(workflow_state(scoped)))  # type: ignore[arg-type]

    assert result["status"] == "FAILED"
    assert stub.calls == 1, f"不可重试的失败被重复发送了 {stub.calls} 次"


@pytest.mark.parametrize(("llm_attempts", "parse_attempts"), [(2, 1), (3, 1)])
def test_total_model_calls_stay_within_the_documented_bound(
    settings: Settings, monkeypatch: pytest.MonkeyPatch, llm_attempts: int, parse_attempts: int
) -> None:
    """一次生成的总调用数不超过 `llm_max_attempts × draft_parse_attempts`。

    改造前两层共用同一个值，实际是 `llm_max_attempts²`，因此这两个参数化用例
    在改前会超出文档上限（2→4、3→9）。
    """
    stub = ModelStub(503)
    stub.install(monkeypatch)
    scoped = configured(settings, llm_max_attempts=llm_attempts, draft_parse_attempts=parse_attempts)
    workflow = build_workflow(scoped, OpenAICompatibleProvider(scoped))

    result = run(workflow.run(workflow_state(scoped)))  # type: ignore[arg-type]

    assert result["status"] == "FAILED"
    bound = llm_attempts * parse_attempts
    assert stub.calls == bound, f"实际 {stub.calls} 次，文档上限 {bound} 次"


def test_successful_generation_costs_one_call(settings: Settings, monkeypatch: pytest.MonkeyPatch) -> None:
    stub = ModelStub(200)
    stub.install(monkeypatch)
    scoped = configured(settings, llm_max_attempts=3, draft_parse_attempts=2)
    workflow = build_workflow(scoped, OpenAICompatibleProvider(scoped))

    result = run(workflow.run(workflow_state(scoped)))  # type: ignore[arg-type]

    assert result["status"] == "DRAFT_READY", result.get("error")
    assert stub.calls == 1, "成功的生成不应该产生额外调用"
