"""独立评测中心：作业仓储、运行入口、失败分类与基线对比。

隔离原则：

- 评测在**独立工作目录**里跑，`build_settings` 把任务来源强制为 `evaluation`，
  每次用例使用独立的 JSON / SQLite 文件——不会向正式存储批量写测试变更。
- 真实模型评测必须**显式启用**且配置凭据，否则作业记为 `not_run`：
  未运行就是未运行，绝不用离线结果冒充真实模型质量。
- 报告与逐例结果原样保留（含失败原文），失败分类只做聚合，不覆盖原文。
"""

from __future__ import annotations

import argparse
import json
import os
import tempfile
from contextlib import suppress
from datetime import datetime, timezone
from pathlib import Path
from threading import Lock
from typing import Any

from app.config import Settings


class EvalNotFound(Exception):
    """评测作业不存在或不属于调用方组织。"""


class EvalInvalid(Exception):
    """评测请求不合法（数据集缺失、数据集为空等）。"""


class EvalStateUnavailable(Exception):
    """评测作业无法落盘。"""


class EvalRepository:
    """评测作业仓储（单实例 JSON 原子落盘）。"""

    def __init__(self, path: str) -> None:
        if not str(path or "").strip():
            raise ValueError("评测仓储需要非空的存储路径")
        self._path = Path(path)
        self._lock = Lock()
        self._records: dict[str, dict[str, Any]] = {}
        self._load()

    def _load(self) -> None:
        if not self._path.exists():
            return
        try:
            raw = self._path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError) as error:
            raise EvalStateUnavailable(f"评测文件不可读：{self._path}（{error}）") from error
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError as error:
            raise EvalStateUnavailable(f"评测文件不是合法 JSON：{self._path}") from error
        records = payload.get("jobs", []) if isinstance(payload, dict) else []
        if not isinstance(records, list):
            raise EvalStateUnavailable(f"评测文件的 jobs 字段必须是数组：{self._path}")
        for record in records:
            if isinstance(record, dict) and record.get("job_id"):
                self._records[str(record["job_id"])] = record

    def _persist_records(self, records: dict[str, dict[str, Any]]) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        payload = json.dumps({"jobs": list(records.values())}, ensure_ascii=False, indent=2)
        handle = tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", delete=False, dir=str(self._path.parent), suffix=".tmp"
        )
        temporary_path = handle.name
        try:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
            handle.close()
            os.replace(temporary_path, self._path)
        except BaseException:
            if not handle.closed:
                handle.close()
            with suppress(OSError):
                os.unlink(temporary_path)
            raise

    def save(self, record: dict[str, Any]) -> None:
        job_id = record.get("job_id")
        if not job_id:
            raise ValueError("评测作业必须带 job_id")
        with self._lock:
            candidate = dict(self._records)
            candidate[str(job_id)] = json.loads(json.dumps(record))
            self._persist_records(candidate)
            self._records = candidate

    def get(self, job_id: str) -> dict[str, Any] | None:
        with self._lock:
            record = self._records.get(job_id)
            return json.loads(json.dumps(record)) if record is not None else None

    def list(self) -> list[dict[str, Any]]:
        with self._lock:
            return [json.loads(json.dumps(item)) for item in self._records.values()]


async def run_evaluation(
    settings: Settings, *, provider: str, strategy: str, split: str, limit: int | None, workdir: Path
) -> dict[str, Any]:
    """在隔离目录里跑一遍评测，返回数据集信息、汇总与逐例结果。"""
    # 延迟导入：run_eval 会导入 app.service / app.workflow，模块顶层导入会形成循环。
    try:
        from evals import run_eval
    except ImportError as error:  # pragma: no cover - 依赖部署产物，本地与镜像都提供
        raise EvalInvalid("评测框架未随部署提供（缺少 evals 模块）：无法运行评测，记为失败而不是伪造结果") from error

    args = argparse.Namespace(
        dataset=None, split=split, provider=provider, strategy=strategy, compare=False, repeats=1, workdir=workdir
    )
    entries = run_eval.resolve_datasets(args)
    missing = [str(path) for _name, path in entries if not path.exists()]
    if missing:
        raise EvalInvalid("数据集不存在：" + "、".join(missing))
    cases: list[dict[str, Any]] = []
    for name, path in entries:
        for case in run_eval.load_cases(path):
            case.setdefault("split", name)
            cases.append(case)
    if limit:
        cases = cases[: int(limit)]
    if not cases:
        raise EvalInvalid("数据集为空，没有可运行的用例")
    version, digest = run_eval.dataset_digest(entries)
    expectations = {case["id"]: (case.get("expect") or {}) for case in cases}

    # 真实模型调用必须有界：超时与（可选）token / 请求上限。离线 provider 同样套用超时。
    # 语料目录显式指向本服务的语料，避免评测用例在镜像里读到不存在的仓库路径。
    overrides: dict[str, Any] = {
        "task_timeout_seconds": float(settings.eval_task_timeout_seconds),
        "agent_demo_dir": str(settings.demo_dir),
    }
    if settings.eval_max_task_tokens:
        overrides["max_task_tokens"] = int(settings.eval_max_task_tokens)
    if settings.eval_max_task_requests:
        overrides["max_task_requests"] = int(settings.eval_max_task_requests)

    arm = await run_eval.run_arm(args, cases, expectations, strategy=strategy, overrides=overrides)
    return {
        "dataset": {"version": version, "sha256": digest},
        "summary": arm["summary"],
        "cases": arm["cases"],
        "limitations": list(run_eval.LIMITATIONS),
    }


def aggregate_failures(cases: list[dict[str, Any]]) -> dict[str, int]:
    """按用例结局聚合失败分类（原文仍保留在报告里）。"""
    counts: dict[str, int] = {}
    for case in cases or []:
        outcome = str(case.get("outcome") or "unknown")
        if outcome == "passed":
            continue
        if outcome in {"not_run", "skipped"}:
            counts[outcome] = counts.get(outcome, 0) + 1
            continue
        observed = case.get("observed") or {}
        classification = str(observed.get("error_type") or "other")
        counts[classification] = counts.get(classification, 0) + 1
    return counts


def compare_jobs(base: dict[str, Any], target: dict[str, Any]) -> dict[str, Any]:
    """并排对照两次作业，只陈述实测差异。"""
    base_summary = base.get("summary") or {}
    target_summary = target.get("summary") or {}
    keys = ("total", "executed", "passed", "failed", "not_run", "skipped")
    summary_delta = {
        key: int(target_summary.get(key) or 0) - int(base_summary.get(key) or 0)
        for key in keys
        if isinstance(base_summary.get(key), (int, float)) or isinstance(target_summary.get(key), (int, float))
    }
    base_failures = base.get("failure_classes") or {}
    target_failures = target.get("failure_classes") or {}
    failure_class_delta = {
        key: int(target_failures.get(key) or 0) - int(base_failures.get(key) or 0)
        for key in sorted(set(base_failures) | set(target_failures))
    }
    base_cases = {str(item.get("id")): item for item in (base.get("cases") or [])}
    target_cases = {str(item.get("id")): item for item in (target.get("cases") or [])}
    case_delta: list[dict[str, Any]] = []
    for case_id in sorted(set(base_cases) | set(target_cases)):
        before = (base_cases.get(case_id) or {}).get("outcome")
        after = (target_cases.get(case_id) or {}).get("outcome")
        if before != after:
            case_delta.append({"id": case_id, "base_outcome": before, "target_outcome": after})
    return {
        "base_summary": base_summary,
        "target_summary": target_summary,
        "summary_delta": summary_delta,
        "failure_class_delta": failure_class_delta,
        "case_delta": case_delta,
        "note": (
            "两次作业可能使用不同 provider / strategy / 数据集；对照只陈述实测差异，"
            "不预设任何提升比例，样本小时也不做统计结论。"
        ),
    }


def job_view(record: dict[str, Any]) -> dict[str, Any]:
    """把仓储记录投影成对外视图。"""
    return {
        "job_id": str(record.get("job_id") or ""),
        "organization_id": str(record.get("organization_id") or ""),
        "created_by": str(record.get("created_by") or ""),
        "provider": str(record.get("provider") or "scripted"),
        "strategy": str(record.get("strategy") or "bounded_agent"),
        "split": str(record.get("split") or "dev"),
        "status": str(record.get("status") or "queued"),
        "created_at": record.get("created_at"),
        "started_at": record.get("started_at"),
        "finished_at": record.get("finished_at"),
        "dataset_version": (record.get("dataset") or {}).get("version"),
        "dataset_sha256": (record.get("dataset") or {}).get("sha256"),
        "task_source": str(record.get("task_source") or "evaluation"),
        "summary": record.get("summary"),
        "failure_classes": dict(record.get("failure_classes") or {}),
        "notes": list(record.get("notes") or []),
        "error": record.get("error"),
        "case_count": len(record.get("cases") or []),
    }


def new_job_record(
    *, job_id: str, organization_id: str, created_by: str, provider: str, strategy: str, split: str, limit: int | None
) -> dict[str, Any]:
    now = datetime.now(timezone.utc).isoformat()
    return {
        "job_id": job_id,
        "organization_id": organization_id,
        "created_by": created_by,
        "provider": provider,
        "strategy": strategy,
        "split": split,
        "limit": limit,
        "status": "running",
        "created_at": now,
        "started_at": now,
        "finished_at": None,
        "task_source": "evaluation",
        "dataset": None,
        "summary": None,
        "cases": [],
        "failure_classes": {},
        "limitations": [],
        "notes": [],
        "error": None,
    }


LIVE_NOT_RUN_NOTE = "真实模型评测未启用或缺少凭据：记为 NOT_RUN，不伪造通过率或质量提升"
LIVE_ENABLE_HINT = (
    "启用方式：设置 AGENT_EVAL_LIVE_ENABLED=1，并提供 AGENT_LLM_BASE_URL 与 AGENT_LLM_API_KEY；"
    "评测会在独立工作目录中运行，并受 AGENT_EVAL_TASK_TIMEOUT / AGENT_EVAL_MAX_TASK_TOKENS 约束。"
)
