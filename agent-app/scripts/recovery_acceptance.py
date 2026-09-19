"""独立进程恢复验收（两个场景）。

这不是 pytest 用例：必须**真的**起一个独立进程、真的把它杀掉，才谈得上"跨进程"。

**场景一**：跑到节点级中断 → 终止进程 → 重启 → 授权恢复 → 完成。
判据是持久化事实：重启前后 PID 不同，且 `screen_input` 事件在整条生命周期里只出现一次
（若从头重跑会出现两次）。

**场景二**：模型请求已经发出、账本已按次落盘，但**终态尚未落盘**就被杀进程。
这是 exactly-once 做不到的那一半：请求可能已经打出去了。判据同样是持久化事实——
磁盘上确实没有终态、重启后任务被显式标注为中断、而**中断前已经消耗的账目没有丢**
（`usage.requests` 仍是中断前的值）。

用法：`.venv\\Scripts\\python.exe scripts/recovery_acceptance.py`
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import httpx

APP_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = APP_DIR.parent
DEMO_DIR = REPO_ROOT / "examples" / "agent-demo"
SNAPSHOT = (DEMO_DIR / "schema" / "orders.sql").read_text(encoding="utf-8")

PORT = int(os.getenv("RECOVERY_ACCEPTANCE_PORT", "18097"))
STUB_PORT = int(os.getenv("RECOVERY_ACCEPTANCE_STUB_PORT", "18098"))
IDENTITY = {"X-Actor-Id": "usr_developer", "X-Org-Id": "org_demo"}

REQUIREMENT = "给订单表按用户和创建时间查询的场景准备一个索引变更，目标 PostgreSQL。"
QUERY_SQL = "SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50;"

# 一份**会被确定性检查打回**的草案（非并发建索引、没有 lock_timeout），
# 用来触发修订，从而让第二次模型请求发出去。
REVISABLE_DRAFT = json.dumps(
    {
        "sql": "CREATE INDEX idx_orders_user_created ON orders (user_id, created_at);",
        "rollback_sql": "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
        "assumptions": [
            {"statement": "按查询形态推断索引列为 (user_id, created_at)。", "needs_confirmation": True}
        ],
        "open_questions": [],
        "advisory_risk": "MEDIUM",
        "advice_summary": "先给出非并发建索引，等待确定性检查反馈后修订。",
    },
    ensure_ascii=False,
)

RESULTS: list[tuple[str, bool, str]] = []


class _ModelStubHandler(BaseHTTPRequestHandler):
    """第一次请求返回一份会被打回的草案，之后的请求**永不返回**。

    "永不返回"正是场景二要制造的窗口：第二次模型请求已经发出（说明第一次已经按次记账
    并落盘），进程被杀时终态还没有写。这里不模拟任何模型质量，只负责把调用挂住。
    """

    protocol_version = "HTTP/1.1"

    def do_POST(self) -> None:  # noqa: N802 - http.server 的命名约定
        length = int(self.headers.get("content-length") or 0)
        if length:
            self.rfile.read(length)
        index = self.server.calls  # type: ignore[attr-defined]
        self.server.calls = index + 1  # type: ignore[attr-defined]
        if index > 0:
            # 挂住，直到本场景结束（或进程被杀）。不返回任何响应。
            self.server.release.wait(timeout=300)  # type: ignore[attr-defined]
            self.close_connection = True
            return
        body = json.dumps(
            {
                "choices": [{"message": {"role": "assistant", "content": REVISABLE_DRAFT}}],
                "usage": {"prompt_tokens": 120, "completion_tokens": 30},
            }
        ).encode("utf-8")
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args: object) -> None:
        """静音：stub 的访问日志与验收结论无关。"""


def start_model_stub() -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(("127.0.0.1", STUB_PORT), _ModelStubHandler)
    server.daemon_threads = True  # 挂住的处理线程不得阻塞脚本退出
    server.release = threading.Event()  # type: ignore[attr-defined]
    server.calls = 0  # type: ignore[attr-defined]
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def stop_model_stub(server: ThreadingHTTPServer) -> None:
    server.release.set()  # type: ignore[attr-defined] - 放行被挂住的请求
    server.shutdown()


def read_store(path: Path) -> dict[str, Any]:
    """读任务存储文件。写入是"临时文件 + os.replace"的原子替换，因此不会读到半截内容。"""
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return payload if isinstance(payload, dict) else {}


def stored_record(path: Path, task_id: str) -> dict[str, Any]:
    for record in read_store(path).get("tasks") or []:
        if isinstance(record, dict) and record.get("task_id") == task_id:
            return record
    return {}


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


def scenario_terminal_state_lost_while_the_ledger_survives() -> None:
    """模型请求已经发出、账本已按次落盘，但终态尚未落盘就杀进程。

    这是"不宣称 exactly-once"的可核对形式：请求确实可能已经打出去了，
    因此恢复只能保证**账目**不丢、任务被**显式**标为中断，而不是假装那次调用没发生过。
    """
    workdir = Path(tempfile.mkdtemp(prefix="agent-unpersisted-"))
    checkpoint = workdir / "agent-checkpoints.sqlite"
    store = workdir / "agent-tasks.json"
    stub = start_model_stub()
    env = dict(os.environ)
    env.update(
        {
            "AGENT_DEMO_DIR": str(DEMO_DIR),
            "AGENT_TASK_STORE": str(store),
            "AGENT_CHECKPOINT_PATH": str(checkpoint),
            "AGENT_EXECUTION_MODE": "inline",
            "AGENT_ALLOW_HEADER_IDENTITY": "1",
            "AGENT_MAX_REVISIONS": "1",
            "AGENT_UPSTREAM_TOKEN": "",
            # 指向本脚本自带的 stub：它会挂住第二次调用，从而制造"请求已发出、终态未落盘"。
            "AGENT_LLM_BASE_URL": f"http://127.0.0.1:{STUB_PORT}",
            "AGENT_LLM_API_KEY": "stub-not-a-real-credential",
            "AGENT_LLM_MODEL": "stub-model",
            "AGENT_LLM_TIMEOUT": "30",
        }
    )

    print(f"\n场景二工作目录：{workdir}")
    try:
        proc, base = start_service(env)
        first_pid = proc.pid
        print(f"场景二第一次启动：pid={first_pid}")

        try:
            # inline 执行会一直卡在挂起的第二次模型调用上，因此请求放到后台线程里发。
            def create() -> None:
                try:
                    httpx.post(
                        f"{base}/api/agent/tasks",
                        json={
                            "requirement": REQUIREMENT,
                            "application": "order-service",
                            "environment": "生产",
                            "database": "postgresql",
                            "table": "orders",
                            "query_sql": QUERY_SQL,
                            "planned_at": "2026-09-18T21:30:00+08:00",
                            "planned_at_timezone": "Asia/Shanghai",
                            "schema_snapshot": SNAPSHOT,
                        },
                        headers=IDENTITY,
                        timeout=120.0,
                    )
                except Exception:  # noqa: BLE001 - 进程被强杀时连接必然断开，这不是失败
                    pass

            threading.Thread(target=create, daemon=True).start()

            observed = _wait_for_ledger(store, timeout=90.0)
        finally:
            # 强杀而不是优雅停机：要的就是"终态还没写"的那一刻。
            stop_service(proc)

        check(
            "模型请求已发出且账本已按次落盘",
            observed is not None,
            str(observed.get("usage") if observed else None),
        )
        if observed is None:
            return
        check("进程已被强制终止", proc.poll() is not None, f"pid={first_pid}")

        task_id = str(observed.get("task_id") or "")
        requests_before = int((observed.get("usage") or {}).get("requests") or 0)
        prompt_before = (observed.get("usage") or {}).get("prompt_tokens")
        check(
            "中断时记录仍在途（终态还没写）",
            observed.get("status") in {"RECEIVED", "RUNNING"},
            f"status={observed.get('status')}",
        )

        on_disk = stored_record(store, task_id)
        check(
            "磁盘上确实没有终态（结果尚未落盘）",
            on_disk.get("status") in {"RECEIVED", "RUNNING"},
            f"status={on_disk.get('status')}",
        )
        check(
            "中断前已消耗的账目仍写在磁盘上",
            int((on_disk.get("usage") or {}).get("requests") or 0) == requests_before,
            f"usage={on_disk.get('usage')}",
        )
        check("磁盘上存在检查点文件", checkpoint.exists(), str(checkpoint))

        proc, base = start_service(env)
        second_pid = proc.pid
        print(f"场景二第二次启动：pid={second_pid}")
        check("确实是新进程（PID 不同）", second_pid != first_pid, f"{first_pid} -> {second_pid}")
        try:
            restarted = httpx.get(f"{base}/api/agent/tasks/{task_id}", headers=IDENTITY, timeout=30.0)
            restarted.raise_for_status()
            task = restarted.json()
            check("重启后任务被显式标为中断", task["status"] == "FAILED", f"status={task['status']}")
            check(
                "中断策略如实标注（有检查点 → 可恢复）",
                task.get("restart_policy") == "checkpoint_available",
                str(task.get("restart_policy")),
            )
            after = int((task.get("usage") or {}).get("requests") or 0)
            check(
                "已消耗的账目没有丢（usage.requests 仍是中断前的值）",
                after == requests_before and after >= 1,
                f"before={requests_before} after={after}",
            )
            check(
                "已消耗的 token 统计同样保留",
                (task.get("usage") or {}).get("prompt_tokens") == prompt_before,
                f"usage={task.get('usage')}",
            )
        finally:
            stop_service(proc)
    finally:
        stop_model_stub(stub)


def _wait_for_ledger(path: Path, *, timeout: float) -> dict[str, Any] | None:
    """轮询任务存储，直到某条记录已经按次记上账本（说明模型请求已发出并返回过一次）。"""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for record in read_store(path).get("tasks") or []:
            if isinstance(record, dict) and int((record.get("usage") or {}).get("requests") or 0) >= 1:
                return record
        time.sleep(0.05)
    return None


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

    # ---- 场景二：请求已发出、终态未落盘 ----
    scenario_terminal_state_lost_while_the_ledger_survives()

    failed = [name for name, ok, _ in RESULTS if not ok]
    print()
    print(f"结果：{len(RESULTS) - len(failed)}/{len(RESULTS)} 通过")
    if failed:
        print("失败项：" + "；".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
