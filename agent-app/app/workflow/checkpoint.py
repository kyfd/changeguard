"""LangGraph 检查点（SQLite 单实例）。

持久化必须用**真正的磁盘检查点**：`InMemorySaver` 不能用来宣称"支持进程重启"，
它随进程消失。这里用官方 `langgraph-checkpoint-sqlite` 的 `AsyncSqliteSaver`，
落在单个 SQLite 文件上。

连接是**短生命周期**的：`AsyncSqliteSaver` 绑定事件循环，而本服务的每次执行、每次恢复
以及测试都各自在独立事件循环里运行；持有一条长期连接会在跨循环复用时出错。
因此每次图调用开一条连接、用完即关。`setup()` 等价于 `CREATE TABLE IF NOT EXISTS`，可重入。

本模块**不**保存任务记录：任务仍是 JSON（见 `app/store/tasks.py`），
SQLite 只保存节点级图状态。两者职责不同，不互相覆盖。
"""

from __future__ import annotations

import sqlite3
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, AsyncIterator

from app.config import Settings


def checkpoint_path(settings: Settings) -> str:
    """解析检查点文件路径；未配置即显式失败，不静默不持久化。"""
    path = (getattr(settings, "checkpoint_path", "") or "").strip()
    if not path:
        # 与任务仓储同样的口径：静默不落盘不是可接受的降级。
        raise ValueError("检查点路径不能为空；进程重启恢复依赖磁盘检查点")
    return path


@asynccontextmanager
async def open_checkpointer(settings: Settings) -> AsyncIterator[Any]:
    """打开一个可用的 AsyncSqliteSaver，退出时关闭连接。

    每次调用都重新 `setup()`：建表是幂等的，且这样重启后无需额外的初始化步骤。
    """
    # 延迟导入：只有真正要跑工作流时才需要这个可选依赖，导入失败应当发生在调用点。
    from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

    path = checkpoint_path(settings)
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    async with AsyncSqliteSaver.from_conn_string(path) as saver:
        await saver.setup()
        yield saver


def has_checkpoint(settings: Settings, thread_id: str) -> bool:
    """同步探测某线程是否已有检查点。

    启动时的在途任务扫描是**同步**的（在事件循环之外、构造器里），因此这里用标准库
    `sqlite3` 直接读同一个文件。表名与列名取自 langgraph-checkpoint-sqlite 的 `setup()`
    （`checkpoints(thread_id, checkpoint_ns, checkpoint_id)`）。

    任何异常都按"没有检查点"处理（保守）：宁可说不可恢复，也不谎称可恢复。
    """
    try:
        path = checkpoint_path(settings)
    except ValueError:
        return False
    if not Path(path).exists():
        return False
    try:
        with sqlite3.connect(path) as conn:
            row = conn.execute(
                "SELECT 1 FROM checkpoints WHERE thread_id = ? LIMIT 1", (thread_id,)
            ).fetchone()
        return row is not None
    except sqlite3.Error:
        return False
