"""离线评估运行器。

产出**可复现报告**：数据集版本、provider、模型、配置、逐例结果、失败样例与局限。

设计原则：
- 硬性安全断言用确定性检查，不靠模型评分；
- 未测项必须显式列出，不能因为"没测"就当成通过；
- 不预先承诺"提升 X%"，只写实测结果。
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

APP_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(APP_DIR))

from app.config import Settings  # noqa: E402
from app.retrieval.corpus import build_retriever  # noqa: E402
from app.schemas.drafts import CreateTaskRequest, DatabaseKind, TaskStatus, ToolResult  # noqa: E402
from app.service import AgentService  # noqa: E402
from app.tools.business import Toolbox  # noqa: E402
from app.tools.registry import Tool, TrustedContext, object_schema  # noqa: E402
from app.workflow.graph import DraftWorkflow, WorkflowDeps  # noqa: E402

DATASET = APP_DIR / "evals" / "datasets" / "starter.jsonl"
REPORT_DIR = APP_DIR / "evals" / "reports"
CONTEXT = TrustedContext(user_id="eval-runner", organization_id="org_eval")

LIMITATIONS = [
    "离线评估使用确定性兜底或脚本化输出，不代表真实模型的起草质量。",
    "未统计 token 用量与费用：未接入计费数据源时不做估算。",
    "保留测试集尚未与开发集拆分，当前仅能防止明显回归。",
    "未覆盖多轮对话、跨应用权限与并发场景。",
]


class ScriptedProvider:
    """返回固定文本的 provider，用于构造确定的失败/冲突场景。"""

    name = "scripted"

    def __init__(self, text: str) -> None:
        self._text = text

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "scripted"}

    async def generate(self, _request: Any) -> str:
        return self._text


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


def load_cases(path: Path) -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line:
            cases.append(json.loads(line))
    return cases


def dataset_version(path: Path) -> str:
    digest = hashlib.sha256(path.read_bytes()).hexdigest()[:12]
    return f"{path.stem}@{digest}"


def build_settings(tmp_path: Path, demo_dir: Path) -> Settings:
    return Settings(
        agent_demo_dir=str(demo_dir),
        task_store_path=str(tmp_path / "eval-tasks.json"),
        checkpoint_path=str(tmp_path / "eval-checkpoints.sqlite"),
        execution_mode="inline",
        max_revisions=2,
    )


async def run_case(case: dict[str, Any], workdir: Path, demo_dir: Path) -> dict[str, Any]:
    started = time.perf_counter()
    settings = build_settings(workdir, demo_dir)
    provider_payload = case.get("scripted_draft")
    provider = None
    if provider_payload is not None:
        provider = ScriptedProvider(json.dumps(provider_payload, ensure_ascii=False))
    elif case.get("scripted_text"):
        provider = ScriptedProvider(case["scripted_text"])

    expectation = case.get("expect") or {}
    observed: dict[str, Any] = {}
    failures: list[str] = []

    try:
        if case.get("harness") == "workflow":
            observed = await _run_workflow_case(case, settings, provider, demo_dir)
        else:
            observed = await _run_service_case(case, settings, provider)
    except Exception as error:  # noqa: BLE001 - 任何异常都要计入失败而不是中断评估
        failures.append(f"执行异常：{type(error).__name__}: {error}")

    failures.extend(_assert_expectations(expectation, observed))
    return {
        "id": case.get("id"),
        "type": case.get("type"),
        "passed": not failures,
        "failures": failures,
        "observed": observed,
        "latency_ms": int((time.perf_counter() - started) * 1000),
    }


async def _run_service_case(case: dict[str, Any], settings: Settings, provider: Any) -> dict[str, Any]:
    service = AgentService(settings, provider=provider)
    request = CreateTaskRequest(
        requirement=case["requirement"],
        application=(case.get("slots") or {}).get("application"),
        environment=(case.get("slots") or {}).get("environment"),
        database=_database((case.get("slots") or {}).get("database")),
        table=(case.get("slots") or {}).get("table"),
        query_sql=(case.get("slots") or {}).get("query_sql"),
        planned_at=(case.get("slots") or {}).get("planned_at"),
        planned_at_timezone=(case.get("slots") or {}).get("planned_at_timezone"),
        schema_snapshot=_snapshot(case),
    )
    view, _ = await service.create_task(request, CONTEXT)
    draft = view.draft
    return {
        "status": view.status.value,
        "error": view.error,
        "ask_fields": [item.field for item in view.questions],
        "sql": (draft.sql if draft else "") or "",
        "rollback_sql": (draft.rollback_sql if draft else "") or "",
        "evidence_ids": [item.evidence_id for item in draft.evidence] if draft else [],
        "evidence_doc_ids": [item.doc_id for item in draft.evidence] if draft else [],
        "revisions": view.revisions,
        "check_status": draft.deterministic_check.status if draft else None,
        "check_codes": [item.code for item in draft.deterministic_check.items] if draft else [],
    }


async def _run_workflow_case(
    case: dict[str, Any], settings: Settings, provider: Any, demo_dir: Path
) -> dict[str, Any]:
    retriever = build_retriever(demo_dir)
    fault = case.get("fault")

    def factory(snapshot: str) -> Toolbox:
        if fault == "scan_failure":
            return FailingScanToolbox(settings=settings, retriever=retriever, schema_snapshot=snapshot)
        return Toolbox(settings=settings, retriever=retriever, schema_snapshot=snapshot)

    workflow = DraftWorkflow(
        WorkflowDeps(
            settings=settings,
            provider=provider or _deterministic(settings),
            trusted_context=CONTEXT,
            toolbox_factory=factory,
        )
    )
    state = {
        "task_id": case["id"],
        "requirement": case["requirement"],
        "slots": _slots_payload(case),
        "schema_snapshot": _snapshot(case),
        "max_revisions": settings.max_revisions,
        "events": [],
        "revisions": 0,
    }
    result = await workflow.run(state)  # type: ignore[arg-type]
    check = result.get("check") or {}
    return {
        "status": result.get("status"),
        "error": result.get("error"),
        "ask_fields": [item.get("field") for item in (result.get("questions") or [])],
        "sql": ((result.get("draft") or {}).get("sql") or ""),
        "rollback_sql": ((result.get("draft") or {}).get("rollback_sql") or ""),
        "evidence_ids": [item.get("evidence_id") for item in ((result.get("draft") or {}).get("evidence") or [])],
        "evidence_doc_ids": [item.get("doc_id") for item in ((result.get("draft") or {}).get("evidence") or [])],
        "revisions": int(result.get("revisions") or 0),
        "check_status": check.get("status"),
        "check_codes": [item.get("code") for item in (check.get("items") or [])],
    }


def _deterministic(settings: Settings) -> Any:
    from app.llm.provider import build_provider

    return build_provider(settings)


def _database(value: Any) -> DatabaseKind | None:
    if not value:
        return None
    try:
        return DatabaseKind(str(value))
    except ValueError:
        return DatabaseKind.UNKNOWN


def _snapshot(case: dict[str, Any]) -> str:
    if case.get("schema_snapshot") == "demo":
        return (APP_DIR.parent / "examples" / "agent-demo" / "schema" / "orders.sql").read_text(encoding="utf-8")
    return case.get("schema_snapshot") or ""


def _slots_payload(case: dict[str, Any]) -> dict[str, Any]:
    slots = dict(case.get("slots") or {})
    if slots.get("database"):
        slots["database"] = str(slots["database"])
    return slots


def _assert_expectations(expectation: dict[str, Any], observed: dict[str, Any]) -> list[str]:
    failures: list[str] = []
    if expectation.get("status") and observed.get("status") != expectation["status"]:
        failures.append(f"状态不符：期望 {expectation['status']}，实际 {observed.get('status')}")

    for needle in expectation.get("sql_contains") or []:
        if needle.upper() not in (observed.get("sql") or "").upper():
            failures.append(f"SQL 缺少 {needle}")
    for needle in expectation.get("rollback_contains") or []:
        if needle.upper() not in (observed.get("rollback_sql") or "").upper():
            failures.append(f"回滚 SQL 缺少 {needle}")

    if expectation.get("min_evidence") is not None:
        if len(observed.get("evidence_ids") or []) < int(expectation["min_evidence"]):
            failures.append(f"引用数量不足：需要 {expectation['min_evidence']}")
    if expectation.get("min_revisions") is not None:
        if int(observed.get("revisions") or 0) < int(expectation["min_revisions"]):
            failures.append(f"修订次数不足：需要 {expectation['min_revisions']}")

    for field in expectation.get("ask_fields") or []:
        if field not in (observed.get("ask_fields") or []):
            failures.append(f"未追问 {field}")

    for code in expectation.get("check_codes") or []:
        if code not in (observed.get("check_codes") or []):
            failures.append(f"检查项缺少 {code}")

    if expectation.get("check_status") and observed.get("check_status") != expectation["check_status"]:
        failures.append(f"检查状态不符：期望 {expectation['check_status']}，实际 {observed.get('check_status')}")

    if expectation.get("error_contains"):
        if expectation["error_contains"] not in (observed.get("error") or ""):
            failures.append(f"错误信息未包含 {expectation['error_contains']}")

    return failures


def write_report(report: dict[str, Any], provider_slug: str) -> Path:
    REPORT_DIR.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    json_path = REPORT_DIR / f"{stamp}-{provider_slug}.json"
    json_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    lines = [
        "# Agent 评估报告",
        "",
        f"- 数据集：`{report['dataset']}`",
        f"- 运行时间：{report['generated_at']}",
        f"- provider：`{report['provider']['provider']}`（model={report['provider']['model']}）",
        f"- 结果：**{report['passed']}/{report['total']} 通过**",
        "",
        "## 逐例结果",
        "",
        "| 用例 | 类型 | 状态 | 耗时(ms) | 失败原因 |",
        "| --- | --- | --- | --- | --- |",
    ]
    for item in report["cases"]:
        reason = "；".join(item["failures"]) if item["failures"] else ""
        lines.append(
            f"| {item['id']} | {item['type']} | {'PASS' if item['passed'] else 'FAIL'} | {item['latency_ms']} | {reason} |"
        )
    lines += ["", "## 局限", ""]
    lines += [f"- {item}" for item in report["limitations"]]
    md_path = REPORT_DIR / f"{stamp}-{provider_slug}.md"
    md_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return json_path


async def main_async(dataset: Path, provider_slug: str, workdir: Path) -> int:
    cases = load_cases(dataset)
    demo_dir = APP_DIR.parent / "examples" / "agent-demo"

    results = []
    for case in cases:
        results.append(await run_case(case, workdir, demo_dir))

    passed = sum(1 for item in results if item["passed"])
    settings = build_settings(workdir, demo_dir)
    provider_info = _deterministic(settings).describe()
    provider_info["llm_configured"] = settings.llm_configured

    report = {
        "dataset": dataset_version(dataset),
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "provider": provider_info,
        "config": {
            "max_revisions": settings.max_revisions,
            "execution_mode": settings.execution_mode,
            "demo_dir": str(demo_dir),
        },
        "total": len(results),
        "passed": passed,
        "failed": len(results) - passed,
        "cases": results,
        "limitations": LIMITATIONS,
    }
    path = write_report(report, provider_slug)
    print(f"报告已写入：{path}")
    print(f"通过 {passed}/{len(results)}")
    return 0 if passed == len(results) else 1


def main() -> int:
    parser = argparse.ArgumentParser(description="运行 Agent 离线评估")
    parser.add_argument("--dataset", type=Path, default=DATASET)
    parser.add_argument("--provider", default="deterministic")
    parser.add_argument("--workdir", type=Path, default=APP_DIR / "evals" / ".work")
    args = parser.parse_args()
    args.workdir.mkdir(parents=True, exist_ok=True)
    return asyncio.run(main_async(args.dataset, args.provider, args.workdir))


if __name__ == "__main__":
    raise SystemExit(main())
