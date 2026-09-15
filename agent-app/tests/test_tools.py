"""工具层测试：白名单、参数校验、只读约束、失败语义。"""

from __future__ import annotations

import pytest

from app.config import Settings
from app.retrieval.corpus import build_retriever
from app.schemas.drafts import TaskStatus, ToolResult
from app.tools.business import Toolbox
from app.tools.registry import (
    InvalidToolArgs,
    Tool,
    ToolNotReadOnly,
    TrustedContext,
    UnknownTool,
    object_schema,
)
from app.tools.scan import scan_sql
from app.workflow.graph import DraftWorkflow, WorkflowDeps
from tests.conftest import SCHEMA_SNAPSHOT, complete_request, run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")


def _registry(settings: Settings):
    return Toolbox(settings=settings, retriever=build_retriever(settings.demo_dir), schema_snapshot=SCHEMA_SNAPSHOT).build()


def test_unknown_tool_is_rejected(settings: Settings) -> None:
    registry = _registry(settings)
    with pytest.raises(UnknownTool):
        run(registry.call("delete_everything", {}, CONTEXT))


def test_identity_cannot_be_supplied_as_argument(settings: Settings) -> None:
    """组织身份只能来自可信上下文；出现在工具参数里必须被拒绝。"""
    registry = _registry(settings)
    with pytest.raises(InvalidToolArgs):
        run(registry.call("search_norms", {"query": "索引", "organization_id": "org_other"}, CONTEXT))


def test_missing_and_ill_typed_arguments_are_rejected(settings: Settings) -> None:
    registry = _registry(settings)
    with pytest.raises(InvalidToolArgs):
        run(registry.call("search_norms", {}, CONTEXT))
    with pytest.raises(InvalidToolArgs):
        run(registry.call("search_norms", {"query": "索引", "limit": "很多"}, CONTEXT))
    with pytest.raises(InvalidToolArgs):
        run(registry.call("search_norms", {"query": "索引", "limit": 99}, CONTEXT))


def test_non_read_only_tool_is_refused(settings: Settings) -> None:
    registry = _registry(settings)

    async def noop(_context: TrustedContext, _args) -> ToolResult:  # type: ignore[no-untyped-def]
        return ToolResult(ok=True, tool="write_something")

    registry.register(
        Tool(
            name="write_something",
            description="会修改数据",
            parameters=object_schema(),
            execute=noop,
            read_only=False,
        )
    )
    with pytest.raises(ToolNotReadOnly):
        run(registry.call("write_something", {}, CONTEXT))


def test_scan_blocks_non_concurrent_index_on_hot_table() -> None:
    check = scan_sql(
        "CREATE INDEX idx_orders_user ON orders (user_id);",
        "DROP INDEX idx_orders_user;",
        SCHEMA_SNAPSHOT,
    )
    codes = {item.code for item in check.items}
    assert check.status == "BLOCKED"
    assert "INDEX_NOT_CONCURRENT" in codes
    assert check.blocking_count >= 1


def test_scan_passes_concurrent_index_with_timeout_and_rollback() -> None:
    check = scan_sql(
        "SET lock_timeout = '3s';\n\nCREATE INDEX CONCURRENTLY idx_orders_user_created ON orders (user_id, created_at DESC);",
        "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
        SCHEMA_SNAPSHOT,
    )
    assert check.status == "PASSED", [item.code for item in check.items]
    assert check.blocking_count == 0


def test_scan_blocks_missing_rollback_and_unguarded_update() -> None:
    check = scan_sql("UPDATE orders SET status = 'x';", "", SCHEMA_SNAPSHOT)
    codes = {item.code for item in check.items}
    assert "MISSING_ROLLBACK" in codes
    assert "UPDATE_WITHOUT_WHERE" in codes
    assert check.status == "BLOCKED"


def test_schema_snapshot_tool_reports_missing_snapshot(settings: Settings) -> None:
    registry = Toolbox(
        settings=settings, retriever=build_retriever(settings.demo_dir), schema_snapshot=""
    ).build()
    result = run(registry.call("get_schema_snapshot", {}, CONTEXT))
    assert result.ok is False
    assert "快照" in (result.error or "")


def test_search_without_match_reports_gap_instead_of_fabricating(settings: Settings) -> None:
    registry = _registry(settings)
    result = run(registry.call("search_historical_changes", {"query": "zzzqqq frobnicate gibberish"}, CONTEXT))
    assert result.ok is False
    assert result.evidence_ids == []


def test_governance_tool_failure_is_explicit(tmp_path) -> None:
    """治理后端不可达时必须显式失败，不能返回"没问题"。"""
    settings = Settings(
        governance_base_url="http://127.0.0.1:1",
        governance_timeout_seconds=1.0,
        agent_demo_dir=str((__import__("pathlib").Path(__file__).resolve().parents[2] / "examples" / "agent-demo")),
        task_store_path=str(tmp_path / "tasks.json"),
    )
    registry = Toolbox(settings=settings, retriever=build_retriever(settings.demo_dir)).build()
    result = run(registry.call("get_change_context", {"change_id": "chg_x"}, CONTEXT))
    assert result.ok is False
    assert result.error


class FailingScanToolbox(Toolbox):
    """故障注入：确定性扫描失败。"""

    def build(self):  # type: ignore[override]
        registry = super().build()

        async def failing(_context: TrustedContext, _args) -> ToolResult:  # type: ignore[no-untyped-def]
            return ToolResult(ok=False, tool="scan_sql", error="扫描服务超时")

        registry.register(
            Tool(
                name="scan_sql",
                description="故障注入的扫描工具",
                parameters=object_schema(
                    properties={"sql": {"type": "string"}, "rollback_sql": {"type": "string"}},
                    required=["sql"],
                ),
                execute=failing,
            )
        )
        return registry


def test_check_tool_failure_never_becomes_passing(settings: Settings) -> None:
    """工具失败不能被解释成"检查通过"——这是本设计最关键的一条失败语义。"""
    retriever = build_retriever(settings.demo_dir)
    workflow = DraftWorkflow(
        WorkflowDeps(
            settings=settings,
            provider=__import__("app.llm.provider", fromlist=["build_provider"]).build_provider(settings),
            trusted_context=CONTEXT,
            toolbox_factory=lambda snapshot: FailingScanToolbox(
                settings=settings, retriever=retriever, schema_snapshot=snapshot
            ),
        )
    )
    request = complete_request()
    state = {
        "task_id": "t_fail",
        "requirement": request.requirement,
        "slots": request.model_dump(mode="json"),
        "schema_snapshot": request.schema_snapshot or "",
        "max_revisions": settings.max_revisions,
        "events": [],
        "revisions": 0,
    }
    result = run(workflow.run(state))  # type: ignore[arg-type]
    assert result["status"] == TaskStatus.CHECK_BLOCKED.value
    assert (result["check"] or {}).get("status") == "FAILED"
