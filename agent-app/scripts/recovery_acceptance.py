"""独立进程恢复验收：跑到中断 → 终止进程 → 重启 → 授权恢复 → 完成。

这不是 pytest 用例：必须**真的**起一个独立进程、真的把它杀掉，才能证明恢复来自磁盘检查点，
而不是同一进程里的函数调用。判据是持久化事实：重启前后 PID 不同，且 `screen_input`
事件在整条生命周期里只出现一次（若从头重跑会出现两次）。

用法：`.venv\\Scripts\\python.exe scripts/recovery_acceptance.py`
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import httpx

APP_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = APP_DIR.parent
DEMO_DIR = REPO_ROOT / "examples" / "agent-demo"
SNAPSHOT = (DEMO_DIR / "schema" / "orders.sql").read_text(encoding="utf-8")

PORT = int(os.getenv("RECOVERY_ACCEPTANCE_PORT", "18097"))
IDENTITY = {"X-Actor-Id": "usr_developer", "X-Org-Id": "org_demo"}

REQUIREMENT = "给订单表按用户和创建时间查询的场景准备一个索引变更，目标 PostgreSQL。"

RESULTS: list[tuple[str, bool, str]] = []


def check(name: str, ok: bool, detail: str = "") -> None:
    RESULTS.append((name, bool(ok), detail))
    mark = "PASS" if ok else "FAIL"
    print(f"[{mark}] {name}" + (f" — {detail}" if detail else ""))


def start_service(env: dict[str, str]) -> tuple[subprocess.Popen[str], str]:
    proc = subprocess.Popen(
        [sys.executable, "-m", "uvicorn", "app.main:app", "--host", "127.0.0.1", "--port", str(PORT)],
        cwd=str(APP_DIR),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    base = f"http://127.0.0.1:{PORT}"
    for _ in range(300):
        if proc.poll() is not None:
            output = proc.stdout.read() if proc.stdout else ""
            raise RuntimeError(f"服务提前退出（code={proc.returncode}）：\n{output}")
        try:
            if httpx.get(f"{base}/api/agent/healthz", timeout=1.0).status_code == 200:
                return proc, base
        except Exception:  # noqa: BLE001 - 未就绪
            pass
        time.sleep(0.1)
    raise RuntimeError("服务未在预期时间内就绪")


def stop_service(proc: subprocess.Popen[str]) -> None:
    proc.terminate()
    try:
        proc.wait(timeout=15)
    except subprocess.TimeoutExpired:  # pragma: no cover - 兜底
        proc.kill()
        proc.wait(timeout=15)


def event_kinds(task: dict) -> list[str]:
    return [item.get("kind") for item in (task.get("events") or [])]


def main() -> int:
    workdir = Path(tempfile.mkdtemp(prefix="agent-recovery-"))
    checkpoint = workdir / "agent-checkpoints.sqlite"
    env = dict(os.environ)
    env.update(
        {
            "AGENT_DEMO_DIR": str(DEMO_DIR),
            "AGENT_TASK_STORE": str(workdir / "agent-tasks.json"),
            "AGENT_CHECKPOINT_PATH": str(checkpoint),
            "AGENT_EXECUTION_MODE": "inline",
            "AGENT_ALLOW_HEADER_IDENTITY": "1",
            "AGENT_MAX_REVISIONS": "0",
            "AGENT_UPSTREAM_TOKEN": "",
        }
    )

    print(f"工作目录：{workdir}")
    proc, base = start_service(env)
    first_pid = proc.pid
    print(f"第一次启动：pid={first_pid}")

    try:
        created = httpx.post(
            f"{base}/api/agent/tasks",
            json={"requirement": REQUIREMENT},
            headers=IDENTITY,
            timeout=60.0,
        )
        created.raise_for_status()
        task = created.json()
        task_id = task["task_id"]
        check("创建后在节点级中断上等待补充", task["status"] == "NEEDS_INFO", f"status={task['status']}")
        check("标记为 awaiting_input", task.get("awaiting_input") is True)
        check("追问包含缺失项", bool(task.get("questions")))
        check(
            "入口节点只执行过一次",
            event_kinds(task).count("screen_input") == 1,
            f"screen_input={event_kinds(task).count('screen_input')}",
        )
    finally:
        stop_service(proc)
    check("进程已终止（PID 不再存活）", proc.poll() is not None, f"pid={first_pid}")

    check("磁盘上存在检查点文件", checkpoint.exists(), str(checkpoint))

    proc, base = start_service(env)
    second_pid = proc.pid
    print(f"第二次启动：pid={second_pid}")
    check("确实是新进程（PID 不同）", second_pid != first_pid, f"{first_pid} -> {second_pid}")

    try:
        restarted = httpx.get(f"{base}/api/agent/tasks/{task_id}", headers=IDENTITY, timeout=30.0)
        restarted.raise_for_status()
        task = restarted.json()
        check("重启后任务仍可查询且等待补充", task["status"] == "NEEDS_INFO", f"status={task['status']}")

        resumed = httpx.post(
            f"{base}/api/agent/tasks/{task_id}/resume",
            json={
                "application": "order-service",
                "environment": "生产",
                "database": "postgresql",
                "table": "orders",
                "query_sql": "SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50;",
                "planned_at": "2026-09-18T21:30:00+08:00",
                "planned_at_timezone": "Asia/Shanghai",
                "schema_snapshot": SNAPSHOT,
            },
            headers=IDENTITY,
            timeout=120.0,
        )
        resumed.raise_for_status()
        task = resumed.json()
        check("授权恢复后完成到 DRAFT_READY", task["status"] == "DRAFT_READY", f"status={task['status']}")
        check("恢复模式标注为 interrupt", task.get("resume_mode") == "interrupt", str(task.get("resume_mode")))
        kinds = event_kinds(task)
        check("入口节点仍未重复执行（未从头重跑）", kinds.count("screen_input") == 1, f"screen_input={kinds.count('screen_input')}")
        check("留下了 resumed 事件", "resumed" in kinds)
        check("产出了草案", bool(task.get("draft")))
    finally:
        stop_service(proc)

    failed = [name for name, ok, _ in RESULTS if not ok]
    print()
    print(f"结果：{len(RESULTS) - len(failed)}/{len(RESULTS)} 通过")
    if failed:
        print("失败项：" + "；".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
