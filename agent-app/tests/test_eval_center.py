"""3.1 M3 独立评测中心：作业/报告/对照、隔离、真实模型门禁与失败分类。"""
from __future__ import annotations

from dataclasses import replace

import pytest

from app.evalcenter import EvalNotFound, EvalStateUnavailable
from app.schemas.evals import EvalJobRequest
from app.service import AgentService
from app.tools.registry import TrustedContext
from tests.conftest import run

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")


def test_scripted_job_runs_in_isolation_and_reports(service):
    job = run(service.create_eval_job(EvalJobRequest(provider="scripted", strategy="bounded_agent", split="dev", limit=2), CONTEXT))
    assert job.status == "completed"
    assert job.task_source == "evaluation"
    assert job.case_count == 2 and job.summary and job.summary["total"] == 2
    assert isinstance(job.failure_classes, dict)
    report = run(service.eval_report(job.job_id, CONTEXT))
    assert len(report.cases) == 2 and report.limitations
    # 评测不向正式任务存储写任何东西。
    assert service._repository.list() == []


def test_live_model_requires_explicit_enablement(settings):
    # 未配置凭据 → not_run。
    service = AgentService(settings)
    job = run(service.create_eval_job(EvalJobRequest(provider="live"), CONTEXT))
    assert job.status == "not_run" and job.case_count == 0
    assert any("NOT_RUN" in note for note in job.notes)

    # 配置了凭据但没有显式启用 → 仍然 not_run。
    configured = replace(settings, llm_base_url="http://example.invalid", llm_api_key="secret", eval_live_enabled=False)
    service = AgentService(configured)
    job = run(service.create_eval_job(EvalJobRequest(provider="live"), CONTEXT))
    assert job.status == "not_run"

    # 显式启用且配置凭据 → 允许运行（本环境未真正调用外部模型，故用例结果是 not_run/skipped）。
    enabled = replace(settings, llm_base_url="http://example.invalid", llm_api_key="secret", eval_live_enabled=True)
    service = AgentService(enabled)
    job = run(service.create_eval_job(EvalJobRequest(provider="live", split="dev", limit=1), CONTEXT))
    assert job.status in {"completed", "failed"}


def test_comparison_and_org_isolation(service):
    first = run(service.create_eval_job(EvalJobRequest(provider="scripted", split="dev", limit=2), CONTEXT))
    second = run(service.create_eval_job(EvalJobRequest(provider="scripted", split="dev", limit=2), CONTEXT))
    comparison = run(service.compare_eval_jobs(first.job_id, second.job_id, CONTEXT))
    assert comparison.summary_delta.get("passed") == 0
    assert comparison.case_delta == []
    assert comparison.note

    other = TrustedContext(user_id="mallory", organization_id="org_other")
    assert run(service.list_eval_jobs(other)) == []
    with pytest.raises(EvalNotFound):
        run(service.get_eval_job(first.job_id, other))
    with pytest.raises(EvalNotFound):
        run(service.compare_eval_jobs(first.job_id, second.job_id, other))


def test_eval_jobs_survive_restart(settings):
    service = AgentService(settings)
    job = run(service.create_eval_job(EvalJobRequest(provider="scripted", split="dev", limit=1), CONTEXT))
    restarted = AgentService(settings)
    assert run(restarted.get_eval_job(job.job_id, CONTEXT)).status == "completed"


def test_eval_persistence_failure_closes(service, monkeypatch):
    def fail(_records):
        raise OSError("test disk failure")

    # 发起时首次落盘失败 → 显式 503，不返回"看起来已排队"的作业。
    monkeypatch.setattr(service._evals, "_persist_records", fail)
    with pytest.raises(EvalStateUnavailable):
        run(service.create_eval_job(EvalJobRequest(provider="scripted", split="dev", limit=1), CONTEXT))


def test_api_eval_center_auth_and_flow(tmp_path):
    from tests.test_api import HEADERS, build_client

    with build_client(tmp_path) as client:
        assert client.post("/api/agent/evals", json={"provider": "scripted"}).status_code == 401
        created = client.post("/api/agent/evals", headers=HEADERS, json={"provider": "scripted", "split": "dev", "limit": 2})
        assert created.status_code == 201, created.text
        job_id = created.json()["job_id"]
        assert client.get("/api/agent/evals", headers=HEADERS).json()[0]["job_id"] == job_id
        assert client.get(f"/api/agent/evals/{job_id}", headers=HEADERS).json()["status"] == "completed"
        assert len(client.get(f"/api/agent/evals/{job_id}/report", headers=HEADERS).json()["cases"]) == 2
        assert client.post("/api/agent/evals", headers=HEADERS, json={"provider": "nope"}).status_code == 422
        second = client.post("/api/agent/evals", headers=HEADERS, json={"provider": "scripted", "split": "dev", "limit": 2}).json()
        comparison = client.get("/api/agent/evals/compare", headers=HEADERS, params={"base": job_id, "target": second["job_id"]})
        assert comparison.status_code == 200 and comparison.json()["summary_delta"]["passed"] == 0
        # 跨组织：404，不泄漏存在性。
        other = {"X-Actor-Id": "mallory", "X-Org-Id": "org_other"}
        assert client.get(f"/api/agent/evals/{job_id}", headers=other).status_code == 404
        assert client.get("/api/agent/evals", headers=other).json() == []
