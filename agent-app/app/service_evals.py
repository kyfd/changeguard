"""AgentService 独立评测中心 Mixin。"""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any

from app.evalcenter import (
    LIVE_ENABLE_HINT,
    LIVE_NOT_RUN_NOTE,
    EvalInvalid,
    EvalNotFound,
    EvalStateUnavailable,
    aggregate_failures,
    compare_jobs,
    job_view,
    new_job_record,
    run_evaluation,
)
from app.schemas.evals import EvalComparison, EvalJobRequest, EvalJobView, EvalReport
from app.tools.registry import TrustedContext


class EvalServiceMixin:
    """独立评测作业管理与基准对比。"""

    async def create_eval_job(self, request: EvalJobRequest, context: TrustedContext) -> EvalJobView:
        """发起一次评测作业并在**隔离目录**中执行。

        真实模型（live）必须显式启用且配置凭据，否则作业记为 `not_run`——
        未运行就是未运行，不用离线结果冒充真实模型质量。
        """
        if not context.organization_id or not context.user_id:
            raise EvalInvalid("缺少可信身份，拒绝发起评测")
        record = new_job_record(
            job_id=f"eval_{uuid.uuid4().hex[:12]}",
            organization_id=context.organization_id,
            created_by=context.user_id,
            provider=request.provider,
            strategy=request.strategy,
            split=request.split,
            limit=request.limit,
        )
        if request.provider == "live" and not self._settings.live_model_ready:
            record["status"] = "not_run"
            record["finished_at"] = datetime.now(timezone.utc).isoformat()
            record["notes"] = [LIVE_NOT_RUN_NOTE, LIVE_ENABLE_HINT]
            self._save_eval(record)
            return EvalJobView.model_validate(job_view(record))

        self._save_eval(record)
        workdir = self._settings.eval_dir / str(record["job_id"])
        try:
            report = await run_evaluation(
                self._settings,
                provider=request.provider,
                strategy=request.strategy,
                split=request.split,
                limit=request.limit,
                workdir=workdir,
            )
        except Exception as error:  # noqa: BLE001 - 任何失败都必须留下可见的作业记录，不伪装成功
            record["status"] = "failed"
            record["finished_at"] = datetime.now(timezone.utc).isoformat()
            record["error"] = f"{type(error).__name__}: {error}"
            self._save_eval(record)
            return EvalJobView.model_validate(job_view(record))

        record["status"] = "completed"
        record["finished_at"] = datetime.now(timezone.utc).isoformat()
        record["dataset"] = report["dataset"]
        record["summary"] = report["summary"]
        record["cases"] = report["cases"]
        record["limitations"] = report.get("limitations") or []
        record["failure_classes"] = aggregate_failures(report["cases"])
        self._save_eval(record)
        return EvalJobView.model_validate(job_view(record))

    async def list_eval_jobs(self, context: TrustedContext) -> list[EvalJobView]:
        if not context.organization_id:
            return []
        records = [
            record for record in self._evals.list()
            if str(record.get("organization_id") or "") == context.organization_id
        ]
        records.sort(key=lambda item: str(item.get("created_at") or ""), reverse=True)
        return [EvalJobView.model_validate(job_view(record)) for record in records]

    async def get_eval_job(self, job_id: str, context: TrustedContext) -> EvalJobView:
        return EvalJobView.model_validate(job_view(self._require_eval(job_id, context)))

    async def eval_report(self, job_id: str, context: TrustedContext) -> EvalReport:
        record = self._require_eval(job_id, context)
        return EvalReport(
            job=EvalJobView.model_validate(job_view(record)),
            cases=list(record.get("cases") or []),
            limitations=list(record.get("limitations") or []),
        )

    async def compare_eval_jobs(self, base_id: str, target_id: str, context: TrustedContext) -> EvalComparison:
        base = self._require_eval(base_id, context)
        target = self._require_eval(target_id, context)
        return EvalComparison(base_job_id=base_id, target_job_id=target_id, **compare_jobs(base, target))

    def _save_eval(self, record: dict[str, Any]) -> None:
        try:
            self._evals.save(record)
        except OSError as error:
            raise EvalStateUnavailable("评测作业未落盘，请重试") from error

    def _require_eval(self, job_id: str, context: TrustedContext) -> dict[str, Any]:
        if not context.organization_id or not context.user_id:
            raise EvalNotFound(job_id)
        record = self._evals.get(job_id)
        if record is None or str(record.get("organization_id") or "") != context.organization_id:
            raise EvalNotFound(job_id)
        return record
