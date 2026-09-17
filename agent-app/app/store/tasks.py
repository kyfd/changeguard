"""任务持久化。

文件存储 + 原子落盘（写临时文件、fsync、再重命名），够单实例使用。
多实例部署需要换成数据库或外部状态存储——这一点写在文档里，不做隐含假设。

两条诚实性要求（改造前都不成立）：

1. **损坏的状态文件必须失败关闭**。以前 JSON 解析失败或读取失败会被静默跳过、
   从空库启动：已有任务凭空消失，而且下一次落盘会把损坏内容永久覆盖掉。
   现在区分"文件不存在"（全新部署，正常）与"文件损坏/不可读"（拒绝启动）。
2. **先落盘、成功后再提交内存**。以前先改内存再落盘，一旦落盘失败，
   内存里就留下一份磁盘上并不存在的状态，后续读写都基于它继续，
   而进程重启后它又会消失——同一份状态出现两个互相矛盾的事实。
"""

from __future__ import annotations

import json
import os
import tempfile
from contextlib import suppress
from pathlib import Path
from threading import Lock
from typing import Any


class TaskRepositoryCorrupted(Exception):
    """状态文件存在但无法安全加载。

    这是一个**失败关闭**信号：调用方不应把它降级成"当作空库继续跑"。
    """


class TaskRepository:
    """任务仓储。"""

    def __init__(self, path: str) -> None:
        if not str(path or "").strip():
            # 以前这里是 `if not str(self._path): return` —— 恒为假的死代码，
            # 意味着"没有配置路径"会静默地不落盘。静默不持久化不是可接受的降级，
            # 所以改成显式拒绝。
            raise ValueError("任务仓储需要非空的存储路径；不接受静默不落盘")
        self._path = Path(path)
        self._lock = Lock()
        self._records: dict[str, dict[str, Any]] = {}
        self._load()

    def _load(self) -> None:
        if not self._path.exists():
            # 文件不存在 = 全新部署，是正常状态。
            return
        try:
            raw = self._path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError) as error:
            raise TaskRepositoryCorrupted(f"任务状态文件不可读：{self._path}（{error}）") from error
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError as error:
            raise TaskRepositoryCorrupted(f"任务状态文件不是合法 JSON：{self._path}") from error
        if not isinstance(payload, dict):
            raise TaskRepositoryCorrupted(f"任务状态文件顶层必须是对象：{self._path}")
        tasks = payload.get("tasks", [])
        if not isinstance(tasks, list):
            raise TaskRepositoryCorrupted(f"任务状态文件的 tasks 字段必须是数组：{self._path}")
        for record in tasks:
            if not isinstance(record, dict):
                raise TaskRepositoryCorrupted(f"任务状态文件包含非对象记录：{self._path}")
            task_id = record.get("task_id")
            if task_id:
                self._records[task_id] = record

    def _persist_records(self, records: dict[str, dict[str, Any]]) -> None:
        """把给定的一份状态写进磁盘。失败时抛错，且不留下半截临时文件。"""
        self._path.parent.mkdir(parents=True, exist_ok=True)
        payload = json.dumps({"tasks": list(records.values())}, ensure_ascii=False, indent=2)
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
        """先落盘，成功后才提交到内存。"""
        task_id = record.get("task_id")
        if not task_id:
            raise ValueError("任务记录必须带 task_id")
        with self._lock:
            candidate = dict(self._records)
            candidate[task_id] = json.loads(json.dumps(record))
            # 顺序是关键：磁盘写成功才替换内存快照，
            # 这样内存永远不会领先于磁盘。
            self._persist_records(candidate)
            self._records = candidate

    def get(self, task_id: str) -> dict[str, Any] | None:
        with self._lock:
            record = self._records.get(task_id)
            return json.loads(json.dumps(record)) if record is not None else None

    def list(self) -> list[dict[str, Any]]:
        with self._lock:
            return [json.loads(json.dumps(item)) for item in self._records.values()]
