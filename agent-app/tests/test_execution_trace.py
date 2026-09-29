"""3.1 M2 执行轨迹：真实记录投影、未知即未知、不含模型思维链。"""
from __future__ import annotations

import pytest

from app.service import TaskNotFound
from app.trace import build_trace
from app.tools.registry import TrustedContext
from tests.conftest import complete_request, run


def _record(**overrides):
    base = {
        "task_id": "task_trace",
        "execution_id": "exec_1",
        "events": [
            {"at": "2026-09-29T00:00:01+00:00", "kind": "created", "detail": "任务已创建"},
            {"at": "2026-09-29T00:00:02+00:00", "kind": "retrieve_evidence", "detail": "检索到 2 条"},
            {"at": "2026-09-29T00:00:03+00:00", "kind": "failed", "detail": "执行失败"},
        ],
        "investigation": {
            "tool_observations": [
                {"tool": "search_norms", "ok": True, "kind": "search", "summary": "命中 2 条",
                 "duration_ms": 12, "observed_at": "2026-09-29T00:00:02.500+00:00",
                 "evidence_ids": ["norm:1"], "error": ""},
                {"tool": "scan_sql", "ok": False, "kind": "material", "summary": "",
                 "duration_ms": None, "observed_at": "2026-09-29T00:00:02.800+00:00",
                 "error": "扫描服务超时"},
            ],
        },
        "usage": {
            "known": False,
            "cost_known": False,
            "cost_estimate": None,
            "requests": 2,
            "reported_responses": 1,
            "missing_responses": 1,
            "calls": [
                {"sequence": 1, "phase": "investigate", "outcome": "ok", "requests": 1, "duration_ms": 120,
                 "usage_known": True, "prompt_tokens": 10, "completion_tokens": 5, "model": "m", "failure_type": None},
                {"sequence": 2, "phase": "generate", "outcome": "timeout", "requests": 1, "duration_ms": None,
                 "usage_known": False, "prompt_tokens": None, "completion_tokens": None, "model": "m",
                 "failure_type": "timeout"},
            ],
        },
        "draft": {"sql": "select 1", "rollback_sql": "",
                  "deterministic_check": {"status": "BLOCKED", "source": "local_scan",
                                          "blocking_count": 1, "items": []}},
    }
    base.update(overrides)
    return base


def test_trace_projects_real_steps_and_marks_unknown():
    trace = build_trace(_record())
    assert trace.includes_model_reasoning is False
    # 不含任何"思维链"字段，避免把内部推理混进轨迹。
    dumped = trace.model_dump()
    assert "chain_of_thought" not in dumped and "reasoning" not in dumped
    assert all("reasoning" not in step and "chain_of_thought" not in step for step in dumped["steps"])
    kinds = {step.kind for step in trace.steps}
    assert kinds == {"event", "tool", "model_call", "check"}
    # 未知耗时保持 None，不填 0。
    timeout_call = next(step for step in trace.steps if step.kind == "model_call" and step.status == "timeout")
    assert timeout_call.duration_ms is None
    assert "step_duration_ms" in trace.unknown
    # 缺 usage 的调用 token 为 None，且整体未知。
    assert timeout_call.prompt_tokens is None
    assert "token_totals" in trace.unknown
    assert "cost_estimate" in trace.unknown
    # 真实耗时与 token 被保留。
    ok_call = next(step for step in trace.steps if step.kind == "model_call" and step.status == "ok")
    assert ok_call.duration_ms == 120 and ok_call.prompt_tokens == 10
    tool_step = next(step for step in trace.steps if step.kind == "tool" and step.tool == "search_norms")
    assert tool_step.duration_ms == 12 and tool_step.evidence_ids == ["norm:1"]
    failed_tool = next(step for step in trace.steps if step.kind == "tool" and step.tool == "scan_sql")
    assert failed_tool.status == "failed" and failed_tool.error == "扫描服务超时"
    check = next(step for step in trace.steps if step.kind == "check")
    assert check.status == "blocked"
    # 索引连续且从 1 开始。
    assert [step.index for step in trace.steps] == list(range(1, len(trace.steps) + 1))


def test_trace_from_real_task_has_events_and_check(service, context):
    view, _ = run(service.create_task(complete_request(), context))
    trace = run(service.task_trace(view.task_id, context))
    assert trace.task_id == view.task_id
    assert any(step.kind == "event" and step.name == "created" for step in trace.steps)
    assert any(step.kind == "check" for step in trace.steps)
    assert trace.usage is not None
    # 未知值显式列出，不用 0 冒充。
    assert "cost_estimate" in trace.unknown


def test_trace_cross_owner_returns_not_found(service, context):
    view, _ = run(service.create_task(complete_request(), context))
    with pytest.raises(TaskNotFound):
        run(service.task_trace(view.task_id, TrustedContext(user_id="bob", organization_id="org_demo")))
