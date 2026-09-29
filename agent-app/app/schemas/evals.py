"""独立评测中心的契约。

评测使用**独立的存储与工作目录**，并把任务来源强制为 `evaluation`；
它不会向正式业务存储批量写测试变更，也不复用正式环境凭据之外的任何东西。
未提供真实模型条件时结果是 `not_run`，绝不用离线结果冒充真实模型质量。
"""

from __future__ import annotations

from datetime import datetime
from typing import Any, Literal

from pydantic import BaseModel, Field

EvalProvider = Literal["deterministic", "scripted", "live"]
EvalStrategy = Literal["fixed_workflow", "bounded_agent"]
EvalSplit = Literal["dev", "holdout", "all"]
EvalStatus = Literal["queued", "running", "completed", "not_run", "failed"]


class EvalJobRequest(BaseModel):
    """发起一次评测作业。"""

    provider: EvalProvider = "scripted"
    strategy: EvalStrategy = "bounded_agent"
    split: EvalSplit = "dev"
    # 只跑前 N 个用例，便于快速回归；None 表示全量。
    limit: int | None = Field(default=None, ge=1, le=200)


class EvalJobView(BaseModel):
    """评测作业的元信息与汇总。"""

    job_id: str
    organization_id: str
    created_by: str
    provider: EvalProvider
    strategy: EvalStrategy
    split: EvalSplit
    status: EvalStatus
    created_at: datetime
    started_at: datetime | None = None
    finished_at: datetime | None = None
    dataset_version: str | None = None
    dataset_sha256: str | None = None
    task_source: str = "evaluation"
    summary: dict[str, Any] | None = None
    # 失败分类（只做聚合，原始错误原文保留在报告里）。
    failure_classes: dict[str, int] = Field(default_factory=dict)
    notes: list[str] = Field(default_factory=list)
    error: str | None = None
    case_count: int = 0


class EvalReport(BaseModel):
    """评测报告：汇总 + 逐例结果。"""

    job: EvalJobView
    cases: list[dict[str, Any]] = Field(default_factory=list)
    limitations: list[str] = Field(default_factory=list)


class EvalComparison(BaseModel):
    """两次评测作业的并排对照（只陈述实测差异）。"""

    base_job_id: str
    target_job_id: str
    base_summary: dict[str, Any] | None = None
    target_summary: dict[str, Any] | None = None
    summary_delta: dict[str, Any] = Field(default_factory=dict)
    failure_class_delta: dict[str, int] = Field(default_factory=dict)
    case_delta: list[dict[str, Any]] = Field(default_factory=list)
    note: str = ""
