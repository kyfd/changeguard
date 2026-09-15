"""任务持久化。

文件存储 + 原子落盘（写临时文件再重命名），够单实例使用。
多实例部署需要换成数据库或外部状态存储——这一点写在文档里，不做隐含假设。
"""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from threading import Lock
from typing import Any


class TaskRepository:
    """任务仓储。"""

    def __init__(self, path: str) -> None:
        self._path = Path(path)
        self._lock = Lock()
        self._records: dict[str, dict[str, Any]] = {}
        self._load()

    def _load(self) -> None:
        if not self._path.exists():
            return
        try:
            payload = json.loads(self._path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            # 状态文件损坏时从空开始，而不是让服务起不来。
            return
        for record in payload.get("tasks", []):
            task_id = record.get("task_id")
            if task_id:
                self._records[task_id] = record

    def _persist(self) -> None:
        if not str(self._path):
            return
        self._path.parent.mkdir(parents=True, exist_ok=True)
        payload = json.dumps({"tasks": list(self._records.values())}, ensure_ascii=False, indent=2)
        handle = tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", delete=False, dir=str(self._path.parent), suffix=".tmp"
        )
        try:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        finally:
            handle.close()
        os.replace(handle.name, self._path)

    def save(self, record: dict[str, Any]) -> None:
        with self._lock:
            self._records[record["task_id"]] = record
            self._persist()

    def get(self, task_id: str) -> dict[str, Any] | None:
        with self._lock:
            record = self._records.get(task_id)
            return json.loads(json.dumps(record)) if record else None

    def list(self) -> list[dict[str, Any]]:
        with self._lock:
            return [json.loads(json.dumps(item)) for item in self._records.values()]
