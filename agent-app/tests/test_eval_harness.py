"""评测运行器：`--provider` / `--strategy` 必须真正改变执行路径，报告不得谎报。

这里断言的是**执行行为**与**报告内容**，不是"脚本能跑完"：
- 不适用当前 provider/strategy 的用例记 `SKIPPED`，而不是改个名字照跑；
- live 无凭据记 `NOT_RUN`，不计入通过，且用独立退出码避免被当成绿灯；
- 报告包含数据集哈希、提交、逐例结果、耗时 P50/P95、模型请求数与 usage 的**未知**语义。
"""

from __future__ import annotations

import argparse
import asyncio
import json
from pathlib import Path

import pytest

from evals import run_eval


def run_harness(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    *,
    dataset: Path | None = None,
    split: str = "dev",
    provider: str = "scripted",
    strategy: str = "bounded_agent",
) -> tuple[int, dict]:
    reports = tmp_path / "reports"
    monkeypatch.setattr(run_eval, "REPORT_DIR", reports)
    args = argparse.Namespace(
        dataset=dataset,
        split=split,
        provider=provider,
        strategy=strategy,
        workdir=tmp_path / "work",
    )
    code = asyncio.run(run_eval.main_async(args))
    newest = max(reports.glob("*.json"), key=lambda item: item.stat().st_mtime)
    return code, json.loads(newest.read_text(encoding="utf-8"))


def outcomes(report: dict) -> dict[str, str]:
    return {item["id"]: item["outcome"] for item in report["cases"]}


# ---------------------------------------------------------------------------
# CLI 参数必须真正生效
# ---------------------------------------------------------------------------


def test_provider_and_strategy_change_which_cases_execute(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    _, scripted = run_harness(tmp_path, monkeypatch, provider="scripted", strategy="bounded_agent")
    _, deterministic = run_harness(tmp_path, monkeypatch, provider="deterministic", strategy="bounded_agent")
    _, fixed = run_harness(tmp_path, monkeypatch, provider="scripted", strategy="fixed_workflow")

    # provider 生效：需要脚本输出的用例在 deterministic 下被显式跳过（不是照跑后改名）。
    assert outcomes(deterministic)["fabricated-evidence"] == run_eval.SKIPPED
    assert outcomes(scripted)["fabricated-evidence"] == run_eval.PASSED
    assert deterministic["summary"]["skipped"] > scripted["summary"]["skipped"]

    # strategy 生效：预算耗尽用例只在 bounded_agent 下有意义。
    assert outcomes(fixed)["budget-exhausted-no-evidence"] == run_eval.SKIPPED
    assert outcomes(scripted)["budget-exhausted-no-evidence"] == run_eval.PASSED
    assert fixed["summary"]["skipped"] > scripted["summary"]["skipped"]


def test_scripted_run_covers_the_allowed_splits(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    code, report = run_harness(tmp_path, monkeypatch)
    assert code == 0
    assert report["summary"]["failed"] == 0
    assert report["summary"]["not_run"] == 0
    assert report["summary"]["skipped"] == 0
    assert report["summary"]["executed"] == report["summary"]["total"]


# ---------------------------------------------------------------------------
# live 无凭据：NOT_RUN，不算通过
# ---------------------------------------------------------------------------


def test_live_without_credentials_is_not_run_and_has_its_own_exit_code(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    code, report = run_harness(tmp_path, monkeypatch, provider="live")
    summary = report["summary"]

    assert code == 2, "NOT_RUN 必须用独立退出码，不能被 CI 当成绿灯"
    assert summary["not_run"] > 0
    assert summary["executed"] + summary["not_run"] + summary["skipped"] == summary["total"]
    not_run = [item for item in report["cases"] if item["outcome"] == run_eval.NOT_RUN]
    assert all(item["reason"] for item in not_run), "NOT_RUN 必须写明原因"
    assert "passed" not in {item["outcome"] for item in not_run}


# ---------------------------------------------------------------------------
# 报告内容
# ---------------------------------------------------------------------------


def test_report_carries_dataset_hash_metrics_and_unknown_usage(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _, report = run_harness(tmp_path, monkeypatch)

    assert len(report["dataset"]["sha256"]) == 64, "必须记录数据集的完整摘要"
    assert report["dataset"]["version"]
    assert report["commit"], "必须记录提交"
    assert report["provider"] == "scripted" and report["strategy"] == "bounded_agent"

    summary = report["summary"]
    assert summary["latency_ms"]["p50"] is not None and summary["latency_ms"]["p95"] is not None
    assert summary["model_latency_ms"]["p50"] is not None
    assert summary["model_requests_total"] == 0, "确定性/脚本 provider 不应有模型请求"
    assert summary["usage"]["known"] is False
    assert summary["usage"]["prompt_tokens"] is None, "usage 未知时不得填 0"
    assert summary["usage"]["cost_estimate"] is None
    assert report["limitations"]

    for case in report["cases"]:
        assert case["outcome"] in {run_eval.PASSED, run_eval.FAILED, run_eval.SKIPPED, run_eval.NOT_RUN}
        assert "model_requests" in case["observed"]


def test_failures_are_preserved_and_no_improvement_is_prefilled(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    dataset = tmp_path / "broken.jsonl"
    dataset.write_text(
        json.dumps(
            {
                "id": "deliberately-wrong",
                "type": "normal",
                "harness": "service",
                "requirement": "给订单表按用户和创建时间查询的场景准备一个索引变更，目标 PostgreSQL。",
                "slots": {
                    "application": "order-service",
                    "environment": "生产",
                    "database": "postgresql",
                    "table": "orders",
                    "planned_at": "2026-09-18T21:30:00+08:00",
                    "planned_at_timezone": "Asia/Shanghai",
                },
                "schema_snapshot": "demo",
                "expect": {"status": "DRAFT_READY", "sql_contains": ["THIS_SHOULD_NOT_APPEAR"]},
            },
            ensure_ascii=False,
        )
        + "\n",
        encoding="utf-8",
    )

    code, report = run_harness(tmp_path, monkeypatch, dataset=dataset)

    assert code == 1
    case = report["cases"][0]
    assert case["outcome"] == run_eval.FAILED
    assert case["failures"], "失败原因必须保留在报告里"
    assert "THIS_SHOULD_NOT_APPEAR" in " ".join(case["failures"])
    assert "提升" not in json.dumps(report, ensure_ascii=False)


# ---------------------------------------------------------------------------
# 数据集摘要
# ---------------------------------------------------------------------------


def test_dataset_digest_is_content_addressed(tmp_path: Path) -> None:
    first = tmp_path / "a.jsonl"
    first.write_text('{"id":"x"}\n', encoding="utf-8")
    second = tmp_path / "b.jsonl"
    second.write_text('{"id":"y"}\n', encoding="utf-8")

    version_a, digest_a = run_eval.dataset_digest([("dev", first)])
    version_b, digest_b = run_eval.dataset_digest([("dev", first)])
    version_c, digest_c = run_eval.dataset_digest([("dev", second)])

    assert (version_a, digest_a) == (version_b, digest_b)
    assert digest_a != digest_c and version_a != version_c


def test_percentile_handles_empty_input() -> None:
    assert run_eval.percentile([], 50) is None
    assert run_eval.percentile([10, 20, 30, 40], 50) == 20
    assert run_eval.percentile([10, 20, 30, 40], 95) == 40
