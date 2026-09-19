"""评测运行器。

三条纪律：

- **硬性安全断言用确定性检查评分**，不靠模型自评；
- `--provider` 与 `--strategy` **真正改变执行路径**，不是只改报告文件名；
- 未运行的项（live 无凭据、不适用当前 provider/strategy 的用例）显式记为
  `NOT_RUN` / `SKIPPED`——既不算通过，也不假装失败。

报告写入 JSON 与 Markdown（同目录），保留**全部**用例（含失败），不预填任何提升比例。
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import math
import os
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

APP_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(APP_DIR))

from app.config import Settings  # noqa: E402
from app.llm.provider import DeterministicProvider, OpenAICompatibleProvider  # noqa: E402
from app.retrieval.corpus import build_retriever  # noqa: E402
from app.schemas.drafts import (  # noqa: E402
    ClarifyRequest,
    CreateTaskRequest,
    DatabaseKind,
    TaskStatus,
    ToolResult,
)
from app.service import AgentService  # noqa: E402
from app.tools.business import Toolbox  # noqa: E402
from app.tools.registry import Tool, TrustedContext, object_schema  # noqa: E402
from app.workflow.graph import DraftWorkflow, WorkflowDeps  # noqa: E402

REPO_ROOT = APP_DIR.parent
DATASET_DIR = APP_DIR / "evals" / "datasets"
# 报告目录可用环境变量覆盖，便于测试写到临时目录、不污染仓库。
REPORT_DIR = Path(os.getenv("AGENT_EVAL_REPORT_DIR") or (APP_DIR / "evals" / "reports"))
DEMO_DIR = REPO_ROOT / "examples" / "agent-demo"
CONTEXT = TrustedContext(user_id="eval-runner", organization_id="org_eval")
CONTEXT_OTHER = TrustedContext(user_id="eval-intruder", organization_id="org_other")

# 用例结局。`NOT_RUN` 与 `SKIPPED` 都**不是**通过。
PASSED = "passed"
FAILED = "failed"
NOT_RUN = "not_run"
SKIPPED = "skipped"

PROVIDERS = ("deterministic", "scripted", "live")
STRATEGIES = ("fixed_workflow", "bounded_agent")

LIMITATIONS = [
    "未配置真实模型凭据时，live 评测记为 NOT_RUN，不计入通过。",
    "deterministic / scripted 是确定性回归，不代表真实模型的起草质量。",
    "开发集与保留集已拆分：保留集不用于反复调参，只用于最终核验。",
    "费用未接入计费数据源：token 用量能拿到就报，费用一律标 unknown。",
]


# ---------------------------------------------------------------------------
# 工具函数
# ---------------------------------------------------------------------------


def percentile(values: list[int], p: float) -> int | None:
    """最近秩（nearest-rank）百分位；样本为空返回 None，不填 0 冒充有数据。"""
    if not values:
        return None
    ordered = sorted(values)
    rank = max(1, math.ceil(p / 100 * len(ordered)))
    return int(ordered[rank - 1])


def commit_sha() -> str:
    try:
        result = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=str(REPO_ROOT), capture_output=True, text=True, timeout=10
        )
        sha = result.stdout.strip()
        return sha if result.returncode == 0 and sha else "unknown"
    except Exception:  # noqa: BLE001 - 拿不到就如实写 unknown
        return "unknown"


def load_cases(path: Path) -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line:
            cases.append(json.loads(line))
    return cases


def resolve_datasets(args: argparse.Namespace) -> list[tuple[str, Path]]:
    if args.dataset is not None:
        return [(Path(args.dataset).stem, Path(args.dataset))]
    if args.split == "all":
        return [("dev", DATASET_DIR / "dev.jsonl"), ("holdout", DATASET_DIR / "holdout.jsonl")]
    return [(args.split, DATASET_DIR / f"{args.split}.jsonl")]


def dataset_digest(entries: list[tuple[str, Path]]) -> tuple[str, str]:
    """返回 (可读版本号, 全量 SHA-256)。版本号便于引用，摘要用于核对未被改动。"""
    overall = hashlib.sha256()
    parts: list[str] = []
    for name, path in entries:
        raw = path.read_bytes()
        overall.update(name.encode("utf-8"))
        overall.update(b"\x00")
        overall.update(raw)
        overall.update(b"\x00")
        parts.append(f"{name}@{hashlib.sha256(raw).hexdigest()[:12]}")
    return " + ".join(parts), overall.hexdigest()


class ScriptedProvider:
    """返回固定文本的 provider，用于构造确定的失败/冲突场景。"""

    name = "scripted"

    def __init__(self, text: str) -> None:
        self._text = text

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "scripted"}

    async def generate(self, _request: Any) -> str:
        return self._text


class HangingProvider:
    """永不返回的 provider：用于验证取消能真正命中工作流。"""

    name = "hanging"

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "hanging"}

    async def generate(self, _request: Any) -> str:
        await asyncio.Event().wait()
        raise AssertionError("unreachable")  # pragma: no cover


class FailingScanToolbox(Toolbox):
    """故障注入：确定性扫描不可用。"""

    def build(self):  # type: ignore[override]
        registry = super().build()

        async def failing(_context: TrustedContext, _args: Any) -> ToolResult:
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


def build_settings(workdir: Path, args: argparse.Namespace, overrides: dict[str, Any] | None = None) -> Settings:
    """每个用例独立的任务存储与检查点，避免用例之间互相污染。"""
    token = uuid.uuid4().hex[:10]
    # 只有真实模型才可能做原生动作决策；确定性/脚本 provider 没有 decide()。
    planner = "provider" if (args.provider == "live" and args.strategy == "bounded_agent") else "rule"
    values: dict[str, Any] = {
        "agent_demo_dir": str(DEMO_DIR),
        "task_store_path": str(workdir / f"eval-tasks-{token}.json"),
        "checkpoint_path": str(workdir / f"eval-checkpoint-{token}.sqlite"),
        "execution_mode": "inline",
        "max_revisions": 2,
        "investigation_strategy": args.strategy,
        "investigation_planner": planner,
    }
    values.update({k: v for k, v in (overrides or {}).items() if k in Settings.__dataclass_fields__})
    return Settings(**{k: v for k, v in values.items() if k in Settings.__dataclass_fields__})


def select_provider(slug: str, case: dict[str, Any], settings: Settings) -> Any | None:
    """按 `--provider` 选择运行时 provider；live 未配置凭据返回 None（→ NOT_RUN）。"""
    if case.get("provider_impl") == "hanging":
        # 取消场景需要一次"永远不返回"的模型调用。
        return HangingProvider()
    if slug == "live":
        return OpenAICompatibleProvider(settings) if settings.llm_configured else None
    if slug == "scripted":
        if case.get("scripted_draft") is not None:
            return ScriptedProvider(json.dumps(case["scripted_draft"], ensure_ascii=False))
        if case.get("scripted_text"):
            return ScriptedProvider(case["scripted_text"])
        return DeterministicProvider()
    return DeterministicProvider()


def provider_metrics(provider: Any) -> dict[str, Any]:
    usage = getattr(provider, "last_usage", None)
    return {
        "requests": int(getattr(provider, "requests_sent", 0) or 0),
        "model_ms": int(round(float(getattr(provider, "model_seconds", 0.0) or 0.0) * 1000)),
        "usage": usage if isinstance(usage, dict) else None,
    }


def classify_error(status: str | None, error: str | None) -> str | None:
    """对错误做粗分类（只用于报告聚合；原始错误原文一并保留）。"""
    if not error:
        return None
    if "类型=" in error:
        return "model_call"
    if "JSON" in error:
        return "parse"
    if "证据" in error:
        return "evidence"
    if "超时" in error:
        return "timeout"
    if "决策者" in error:
        return "planner"
    if status == TaskStatus.CHECK_BLOCKED.value:
        return "check_blocked"
    return "other"


# ---------------------------------------------------------------------------
# 断言与结局
# ---------------------------------------------------------------------------


def assert_expectations(expect: dict[str, Any], observed: dict[str, Any]) -> list[str]:
    failures: list[str] = []
    if expect.get("status") and observed.get("status") != expect["status"]:
        failures.append(f"状态不符：期望 {expect['status']}，实际 {observed.get('status')}")

    for needle in expect.get("sql_contains") or []:
        if needle.upper() not in (observed.get("sql") or "").upper():
            failures.append(f"SQL 缺少 {needle}")
    for needle in expect.get("rollback_contains") or []:
        if needle.upper() not in (observed.get("rollback_sql") or "").upper():
            failures.append(f"回滚 SQL 缺少 {needle}")

    if expect.get("min_evidence") is not None:
        if len(observed.get("evidence_ids") or []) < int(expect["min_evidence"]):
            failures.append(f"引用数量不足：需要 {expect['min_evidence']}")
    if expect.get("min_revisions") is not None:
        if int(observed.get("revisions") or 0) < int(expect["min_revisions"]):
            failures.append(f"修订次数不足：需要 {expect['min_revisions']}")

    for field in expect.get("ask_fields") or []:
        if field not in (observed.get("ask_fields") or []):
            failures.append(f"未追问 {field}")

    for code in expect.get("check_codes") or []:
        if code not in (observed.get("check_codes") or []):
            failures.append(f"检查项缺少 {code}")

    if expect.get("check_status") and observed.get("check_status") != expect["check_status"]:
        failures.append(f"检查状态不符：期望 {expect['check_status']}，实际 {observed.get('check_status')}")

    if expect.get("error_contains") and expect["error_contains"] not in (observed.get("error") or ""):
        failures.append(f"错误信息未包含 {expect['error_contains']}")

    if expect.get("resume_mode") is not None and observed.get("resume_mode") != expect["resume_mode"]:
        failures.append(f"恢复模式不符：期望 {expect['resume_mode']}，实际 {observed.get('resume_mode')}")

    if expect.get("screen_input_events") is not None:
        if int(observed.get("screen_input_events") or 0) != int(expect["screen_input_events"]):
            failures.append(
                f"入口节点执行次数不符：期望 {expect['screen_input_events']}，实际 {observed.get('screen_input_events')}"
            )

    # 全局硬断言：草案里不得出现模型自封的"已获人工确认"。
    if int(observed.get("assumptions_self_confirmed") or 0) != 0:
        failures.append("草案包含被模型自封为已确认的假设")

    return failures


def outcome_kind(expect: dict[str, Any], observed: dict[str, Any], failures: list[str]) -> str:
    if failures:
        return "incorrect"
    expected = expect.get("status")
    if expected == TaskStatus.DRAFT_READY.value and observed.get("status") == expected:
        return "completed"
    if observed.get("status") == expected:
        return "correct_refusal"
    return "incorrect"


# ---------------------------------------------------------------------------
# 用例执行
# ---------------------------------------------------------------------------


def create_request(case: dict[str, Any]) -> CreateTaskRequest:
    slots = case.get("slots") or {}
    return CreateTaskRequest(
        requirement=case["requirement"],
        application=slots.get("application"),
        environment=slots.get("environment"),
        database=_database(slots.get("database")),
        table=slots.get("table"),
        query_sql=slots.get("query_sql"),
        planned_at=slots.get("planned_at"),
        planned_at_timezone=slots.get("planned_at_timezone"),
        schema_snapshot=snapshot(case),
    )


def snapshot(case: dict[str, Any]) -> str:
    if case.get("schema_snapshot") == "demo":
        return (DEMO_DIR / "schema" / "orders.sql").read_text(encoding="utf-8")
    return case.get("schema_snapshot") or ""


def _database(value: Any) -> DatabaseKind | None:
    if not value:
        return None
    try:
        return DatabaseKind(str(value))
    except ValueError:
        return DatabaseKind.UNKNOWN


def _base_observed(status: str | None, error: str | None, metrics: dict[str, Any]) -> dict[str, Any]:
    return {
        "status": status,
        "error": error,
        "error_type": classify_error(status, error),
        "model_requests": metrics["requests"],
        "model_ms": metrics["model_ms"],
        "usage": metrics["usage"],
    }


def _draft_observed(draft: Any) -> dict[str, Any]:
    if not draft:
        return {
            "sql": "",
            "rollback_sql": "",
            "evidence_ids": [],
            "evidence_doc_ids": [],
            "assumptions_self_confirmed": 0,
            "check_status": None,
            "check_codes": [],
        }
    check = draft.deterministic_check
    return {
        "sql": draft.sql or "",
        "rollback_sql": draft.rollback_sql or "",
        "evidence_ids": [item.evidence_id for item in draft.evidence],
        "evidence_doc_ids": [item.doc_id for item in draft.evidence],
        "assumptions_self_confirmed": sum(1 for item in draft.assumptions if item.confirmed),
        "check_status": check.status,
        "check_codes": [item.code for item in check.items],
    }


async def run_case(case: dict[str, Any], args: argparse.Namespace, workdir: Path) -> dict[str, Any]:
    started = time.perf_counter()
    providers = case.get("providers") or list(PROVIDERS)
    strategies = case.get("strategies") or list(STRATEGIES)

    def finish(result: dict[str, Any]) -> dict[str, Any]:
        result.setdefault("id", case.get("id"))
        result.setdefault("type", case.get("type"))
        result.setdefault("split", case.get("split"))
        result.setdefault("harness", case.get("harness") or "service")
        result["latency_ms"] = int((time.perf_counter() - started) * 1000)
        return result

    if args.provider not in providers:
        return finish({"outcome": SKIPPED, "reason": f"用例不适用于 provider={args.provider}", "failures": []})
    if args.strategy not in strategies:
        return finish({"outcome": SKIPPED, "reason": f"用例不适用于 strategy={args.strategy}", "failures": []})

    settings = build_settings(workdir, args, case.get("settings"))
    provider = select_provider(args.provider, case, settings)
    if provider is None:
        return finish({"outcome": NOT_RUN, "reason": "live provider 未配置凭据", "failures": [], "observed": {}})

    observed: dict[str, Any] = {}
    failures: list[str] = []
    try:
        if case.get("harness") == "workflow":
            observed = await run_workflow_case(case, settings, provider)
        elif case.get("harness") == "lifecycle":
            observed = await run_lifecycle_case(case, settings, provider)
        else:
            observed = await run_service_case(case, settings, provider)
    except Exception as error:  # noqa: BLE001 - 任何异常都计入失败，不中断整轮
        observed = {"status": None, "error": f"{type(error).__name__}: {error}"}
        failures.append(f"执行异常：{type(error).__name__}: {error}")

    if not failures:
        failures = assert_expectations(case.get("expect") or {}, observed)
    kind = outcome_kind(case.get("expect") or {}, observed, failures)
    return finish(
        {
            "outcome": PASSED if not failures else FAILED,
            "failures": failures,
            "observed": observed,
            "outcome_kind": kind,
            "provider_used": args.provider,
        }
    )


async def run_service_case(case: dict[str, Any], settings: Settings, provider: Any) -> dict[str, Any]:
    service = AgentService(settings, provider=provider)
    view, _ = await service.create_task(create_request(case), CONTEXT)
    observed = _base_observed(view.status.value, view.error, provider_metrics(provider))
    observed.update(_draft_observed(view.draft))
    observed.update(
        {
            "ask_fields": [item.field for item in view.questions],
            "revisions": view.revisions,
            "confirmations": len(view.confirmations),
        }
    )
    return observed


async def run_workflow_case(case: dict[str, Any], settings: Settings, provider: Any) -> dict[str, Any]:
    retriever = build_retriever(settings.demo_dir)
    fault = case.get("fault")

    def factory(snapshot_text: str) -> Toolbox:
        if fault == "scan_failure":
            return FailingScanToolbox(settings=settings, retriever=retriever, schema_snapshot=snapshot_text)
        return Toolbox(settings=settings, retriever=retriever, schema_snapshot=snapshot_text)

    workflow = DraftWorkflow(
        WorkflowDeps(
            settings=settings,
            provider=provider,
            trusted_context=CONTEXT,
            toolbox_factory=factory,
        )
    )
    request = create_request(case)
    state = {
        "task_id": case["id"],
        "requirement": request.requirement,
        "slots": request.model_dump(mode="json"),
        "schema_snapshot": request.schema_snapshot or "",
        "max_revisions": settings.max_revisions,
        "events": [],
        "revisions": 0,
    }
    result = await workflow.run(state)  # type: ignore[arg-type]
    check = result.get("check") or {}
    draft = result.get("draft") or {}
    assumptions = draft.get("assumptions") or []
    observed = _base_observed(result.get("status"), result.get("error"), provider_metrics(provider))
    observed.update(
        {
            "sql": draft.get("sql") or "",
            "rollback_sql": draft.get("rollback_sql") or "",
            "evidence_ids": [item.get("evidence_id") for item in (draft.get("evidence") or [])],
            "evidence_doc_ids": [item.get("doc_id") for item in (draft.get("evidence") or [])],
            "assumptions_self_confirmed": sum(1 for item in assumptions if item.get("confirmed")),
            "ask_fields": [item.get("field") for item in (result.get("questions") or [])],
            "revisions": int(result.get("revisions") or 0),
            "check_status": check.get("status"),
            "check_codes": [item.get("code") for item in (check.get("items") or [])],
        }
    )
    return observed


async def run_lifecycle_case(case: dict[str, Any], settings: Settings, provider: Any) -> dict[str, Any]:
    """取消与恢复场景。它们必须驱动真实的服务路径，不能用桩结果替代。"""
    scenario = case.get("scenario")
    service = AgentService(settings, provider=provider)
    if scenario == "cancel":
        created, _ = await service.create_task(create_request(case), CONTEXT)
        # 等到执行真正在推进（RECEIVED/RUNNING）再取消，避免取消一个还没开始的执行。
        for _ in range(200):
            current = await service.get_task(created.task_id, CONTEXT)
            if current.status in {TaskStatus.RECEIVED, TaskStatus.RUNNING}:
                break
            await asyncio.sleep(0.01)
        cancelled = await service.cancel(created.task_id, CONTEXT)
        observed = _base_observed(cancelled.status.value, cancelled.error, provider_metrics(provider))
        observed["screen_input_events"] = _count_events(cancelled.events, "screen_input")
        return observed

    if scenario == "recovery":
        paused, _ = await service.create_task(create_request(case), CONTEXT)
        slots = case.get("resume_slots") or {}
        resumed = await service.resume(
            paused.task_id,
            CONTEXT,
            ClarifyRequest(
                application=slots.get("application"),
                environment=slots.get("environment"),
                database=_database(slots.get("database")),
                table=slots.get("table"),
                query_sql=slots.get("query_sql"),
                planned_at=slots.get("planned_at"),
                planned_at_timezone=slots.get("planned_at_timezone"),
                schema_snapshot=snapshot(case),
            ),
        )
        observed = _base_observed(resumed.status.value, resumed.error, provider_metrics(provider))
        observed.update(_draft_observed(resumed.draft))
        observed.update(
            {
                "resume_mode": resumed.resume_mode,
                "screen_input_events": _count_events(resumed.events, "screen_input"),
                "ask_fields": [item.field for item in resumed.questions],
                "revisions": resumed.revisions,
            }
        )
        return observed

    raise ValueError(f"未知的生命周期场景：{scenario}")


def _count_events(events: list[Any], kind: str) -> int:
    return sum(1 for item in events if getattr(item, "kind", None) == kind)


# ---------------------------------------------------------------------------
# 报告
# ---------------------------------------------------------------------------


def summarize(results: list[dict[str, Any]]) -> dict[str, Any]:
    executed = [item for item in results if item["outcome"] in {PASSED, FAILED}]
    passed = [item for item in executed if item["outcome"] == PASSED]
    expected_drafts = [item for item in executed if (item.get("expected_status") == TaskStatus.DRAFT_READY.value)]
    completed = [item for item in executed if item.get("outcome_kind") == "completed"]

    usages = [item.get("observed", {}).get("usage") for item in executed]
    known_usage = [item for item in usages if isinstance(item, dict)]
    prompt_tokens = sum(int(item.get("prompt_tokens") or 0) for item in known_usage) if known_usage else None
    completion_tokens = sum(int(item.get("completion_tokens") or 0) for item in known_usage) if known_usage else None

    e2e = [int(item["latency_ms"]) for item in executed]
    model_ms = [int(item.get("observed", {}).get("model_ms") or 0) for item in executed]
    return {
        "total": len(results),
        "executed": len(executed),
        "passed": len(passed),
        "failed": len(executed) - len(passed),
        "not_run": sum(1 for item in results if item["outcome"] == NOT_RUN),
        "skipped": sum(1 for item in results if item["outcome"] == SKIPPED),
        "task_completion_rate": (len(completed) / len(expected_drafts)) if expected_drafts else None,
        "completion_denominator": len(expected_drafts),
        "model_requests_total": sum(int(item.get("observed", {}).get("model_requests") or 0) for item in executed),
        "latency_ms": {
            "p50": percentile(e2e, 50),
            "p95": percentile(e2e, 95),
            "min": min(e2e) if e2e else None,
            "max": max(e2e) if e2e else None,
        },
        "model_latency_ms": {
            "p50": percentile(model_ms, 50),
            "p95": percentile(model_ms, 95),
            "min": min(model_ms) if model_ms else None,
            "max": max(model_ms) if model_ms else None,
        },
        "usage": {
            "known": bool(known_usage),
            "prompt_tokens": prompt_tokens,
            "completion_tokens": completion_tokens,
            "cost_estimate": None,
            "note": "未接入计费数据源：能拿到 usage 就报，费用一律标 unknown；缺失不填 0。",
        },
    }


def write_report(report: dict[str, Any], slug: str) -> Path:
    REPORT_DIR.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    json_path = REPORT_DIR / f"{stamp}-{slug}.json"
    json_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    summary = report["summary"]
    lines = [
        "# Agent 评估报告",
        "",
        f"- 数据集：`{report['dataset']['version']}`",
        f"- 数据集 SHA-256：`{report['dataset']['sha256']}`",
        f"- 提交：`{report['commit']}`",
        f"- 运行时间：{report['generated_at']}",
        f"- provider：`{report['provider']}`　strategy：`{report['strategy']}`",
        f"- 结果：**{summary['passed']}/{summary['executed']} 通过**"
        + (f"，{summary['failed']} 失败" if summary["failed"] else "")
        + (f"，NOT_RUN {summary['not_run']}" if summary["not_run"] else "")
        + (f"，SKIPPED {summary['skipped']}" if summary["skipped"] else ""),
        f"- 任务完成率：{_rate(summary['task_completion_rate'])}（分母 {summary['completion_denominator']}）",
        f"- 端到端耗时 P50/P95：{summary['latency_ms']['p50']} / {summary['latency_ms']['p95']} ms",
        f"- 模型耗时 P50/P95：{summary['model_latency_ms']['p50']} / {summary['model_latency_ms']['p95']} ms",
        f"- 模型请求总数：{summary['model_requests_total']}",
        f"- usage：{'已知' if summary['usage']['known'] else 'unknown（不填 0）'}",
        "",
        "## 逐例结果",
        "",
        "| 用例 | 类型 | 结局 | 结局类别 | 耗时(ms) | 模型请求 | 错误类型 | 失败原因 |",
        "| --- | --- | --- | --- | --- | --- | --- | --- |",
    ]
    for item in report["cases"]:
        reason = item.get("reason") or "；".join(item.get("failures") or [])
        observed = item.get("observed") or {}
        lines.append(
            f"| {item['id']} | {item.get('type')} | {item['outcome'].upper()} | {item.get('outcome_kind') or '-'} "
            f"| {item['latency_ms']} | {observed.get('model_requests', '-')} | {observed.get('error_type') or '-'} | {reason} |"
        )
    lines += ["", "## 局限", ""]
    lines += [f"- {item}" for item in report["limitations"]]
    lines += ["", "## 配置", "", "```json", json.dumps(report["config"], ensure_ascii=False, indent=2), "```"]

    md_path = REPORT_DIR / f"{stamp}-{slug}.md"
    md_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return json_path


def write_comparison_report(report: dict[str, Any], slug: str) -> Path:
    """对照报告：两臂并排，只陈述实测差异。"""
    REPORT_DIR.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    json_path = REPORT_DIR / f"{stamp}-{slug}.json"
    json_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    left = report["arms"]["fixed_workflow"]["summary"]
    right = report["arms"]["bounded_agent"]["summary"]
    rows = [
        ("通过 / 已执行", lambda s: f"{s['passed']}/{s['executed']}"),
        ("失败", lambda s: str(s["failed"])),
        ("NOT_RUN / SKIPPED", lambda s: f"{s['not_run']} / {s['skipped']}"),
        ("任务完成率", lambda s: _rate(s["task_completion_rate"])),
        ("模型请求总数", lambda s: str(s["model_requests_total"])),
        ("端到端 P50 / P95 (ms)", lambda s: f"{s['latency_ms']['p50']} / {s['latency_ms']['p95']}"),
        ("usage", lambda s: "已知" if s["usage"]["known"] else "unknown（不填 0）"),
    ]
    lines = [
        "# Agent 对照评测报告",
        "",
        f"- 数据集：`{report['dataset']['version']}`",
        f"- 数据集 SHA-256：`{report['dataset']['sha256']}`",
        f"- 提交：`{report['commit']}`　provider：`{report['provider']}`　重复次数：`{report['config']['repeats']}`",
        "",
        "> " + report["note"],
        "",
        "## 两臂对比（只有策略不同）",
        "",
        "| 指标 | fixed_workflow | bounded_agent |",
        "| --- | --- | --- |",
    ]
    for label, getter in rows:
        lines.append(f"| {label} | {getter(left)} | {getter(right)} |")
    lines += ["", "## 逐例结局", "", "| 用例 | 类型 | fixed_workflow | bounded_agent |", "| --- | --- | --- | --- |"]
    right_cases = {item["id"]: item for item in report["arms"]["bounded_agent"]["cases"]}
    for item in report["arms"]["fixed_workflow"]["cases"]:
        other = right_cases.get(item["id"], {})
        lines.append(
            f"| {item['id']} | {item.get('type')} | {item['outcome'].upper()} | {other.get('outcome', '-').upper()} |"
        )
    lines += ["", "## 局限", ""] + [f"- {item}" for item in report["limitations"]]

    md_path = REPORT_DIR / f"{stamp}-{slug}.md"
    md_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return json_path


def _rate(value: float | None) -> str:
    return "N/A" if value is None else f"{value * 100:.1f}%"


# ---------------------------------------------------------------------------
# 入口
# ---------------------------------------------------------------------------


async def main_async(args: argparse.Namespace) -> int:
    entries = resolve_datasets(args)
    missing = [str(path) for _name, path in entries if not path.exists()]
    if missing:
        print(f"数据集不存在：{', '.join(missing)}")
        return 1

    cases: list[dict[str, Any]] = []
    for name, path in entries:
        for case in load_cases(path):
            case.setdefault("split", name)
            cases.append(case)
    version, digest = dataset_digest(entries)

    args.workdir.mkdir(parents=True, exist_ok=True)
    # 供汇总区分"以产出草案为目标"的用例（完成率分母）。
    expectations = {case["id"]: (case.get("expect") or {}) for case in cases}

    if getattr(args, "compare", False):
        return await run_comparison(args, entries, cases, expectations, version, digest)

    arm = await run_arm(args, cases, expectations, strategy=args.strategy)
    summary = arm["summary"]
    report = {
        "dataset": {"version": version, "sha256": digest, "files": [str(path) for _n, path in entries]},
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "commit": commit_sha(),
        "provider": args.provider,
        "strategy": args.strategy,
        "config": _config(args),
        "summary": summary,
        "cases": arm["cases"],
        "limitations": LIMITATIONS,
    }
    slug = f"{args.provider}-{args.strategy}-{args.split}"
    path = write_report(report, slug)

    print(f"报告已写入：{path}")
    print(
        f"通过 {summary['passed']}/{summary['executed']}"
        f"，失败 {summary['failed']}，NOT_RUN {summary['not_run']}，SKIPPED {summary['skipped']}"
    )
    return _exit_code([summary])


def _config(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "split": args.split,
        "dataset_arg": str(args.dataset) if args.dataset else None,
        "workdir": str(args.workdir),
        "providers_available": list(PROVIDERS),
        "strategies_available": list(STRATEGIES),
        "repeats": int(getattr(args, "repeats", 1) or 1),
    }


def _exit_code(summaries: list[dict[str, Any]]) -> int:
    if any(item["failed"] for item in summaries):
        return 1
    if any(item["not_run"] for item in summaries):
        # 未运行不等于通过：用不同的退出码，避免 CI 把它当成绿灯。
        return 2
    return 0


async def run_arm(
    args: argparse.Namespace,
    cases: list[dict[str, Any]],
    expectations: dict[str, Any],
    *,
    strategy: str,
) -> dict[str, Any]:
    """在**同一份输入**上跑一遍某个策略。

    对照评测必须只改策略这一个变量：数据集、provider、生成设置与预算都相同。
    """
    scoped = argparse.Namespace(**{**vars(args), "strategy": strategy})
    results = [await run_case(case, scoped, args.workdir) for case in cases]
    for item in results:
        item["expected_status"] = expectations.get(item["id"], {}).get("status")
    return {"strategy": strategy, "summary": summarize(results), "cases": results}


COMPARISON_NOTE = (
    "两臂在**同一数据集、同一 provider、同一预算**下运行，只有策略不同；"
    "报告只陈述实测差异，不预设任何提升比例。"
    "差异可能来自策略，也可能来自模型随机性（重复次数见 config.repeats，样本小时不做统计结论）。"
    "离线 provider 的对照是工程回归，不代表真实模型的质量差异。"
)


async def run_comparison(
    args: argparse.Namespace,
    entries: list[tuple[str, Path]],
    cases: list[dict[str, Any]],
    expectations: dict[str, Any],
    version: str,
    digest: str,
) -> int:
    """固定工作流 vs 受约束调查：同输入对照。"""
    arms = [await run_arm(args, cases, expectations, strategy=name) for name in STRATEGIES]
    report = {
        "dataset": {"version": version, "sha256": digest, "files": [str(path) for _n, path in entries]},
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "commit": commit_sha(),
        "provider": args.provider,
        "compare": list(STRATEGIES),
        "config": _config(args),
        "arms": {arm["strategy"]: arm for arm in arms},
        "note": COMPARISON_NOTE,
        "limitations": LIMITATIONS,
    }
    path = write_comparison_report(report, f"compare-{args.provider}-{args.split}")
    print(f"对照报告已写入：{path}")
    for arm in arms:
        summary = arm["summary"]
        print(
            f"  {arm['strategy']}: 通过 {summary['passed']}/{summary['executed']}"
            f"，失败 {summary['failed']}，完成率 {_rate(summary['task_completion_rate'])}"
            f"，请求 {summary['model_requests_total']}，P50 {summary['latency_ms']['p50']}ms"
        )
    return _exit_code([arm["summary"] for arm in arms])


def main() -> int:
    parser = argparse.ArgumentParser(description="运行变更准备 Agent 评测")
    parser.add_argument("--dataset", type=Path, default=None, help="显式指定数据集；省略时按 --split 选择")
    parser.add_argument("--split", choices=["dev", "holdout", "all"], default="dev")
    parser.add_argument("--provider", choices=list(PROVIDERS), default="scripted")
    parser.add_argument("--strategy", choices=list(STRATEGIES), default="bounded_agent")
    # 对照评测：同一数据集/provider/预算下把两种策略都跑一遍。
    parser.add_argument("--compare", action="store_true", help="固定工作流与受约束调查的同输入对照")
    parser.add_argument("--repeats", type=int, default=1, help="每个用例重复次数（用于观察波动，默认 1）")
    parser.add_argument("--workdir", type=Path, default=APP_DIR / "evals" / ".work")
    args = parser.parse_args()
    return asyncio.run(main_async(args))


if __name__ == "__main__":
    raise SystemExit(main())
