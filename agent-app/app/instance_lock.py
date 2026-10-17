"""4.0 单实例守卫。

任务 JSON、知识/评测 JSON 与检查点 SQLite 都是单实例语义：两个进程同时写会
互相覆盖。4.0 起把这个边界变成启动期硬约束——拿不到实例锁的进程直接启动失败，
而不是只在文档里提醒"不要多开"。锁由操作系统在进程退出（含崩溃）时释放。
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import IO

LOCK_SUFFIX = ".instance.lock"

if os.name == "nt":
    import msvcrt

    def _lock_handle(handle: IO[bytes]) -> None:
        handle.seek(0)
        msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)

    def _unlock_handle(handle: IO[bytes]) -> None:
        handle.seek(0)
        msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)

else:
    import fcntl

    def _lock_handle(handle: IO[bytes]) -> None:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)

    def _unlock_handle(handle: IO[bytes]) -> None:
        fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


class InstanceLockError(RuntimeError):
    """另一个 Agent 进程已经持有同一份存储。"""


class InstanceLock:
    """持有 <store>.instance.lock 的独占锁。"""

    def __init__(self, path: Path) -> None:
        self.path = path
        self._handle: IO[bytes] | None = None

    def acquire(self) -> "InstanceLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        handle = open(self.path, "a+b")
        try:
            _lock_handle(handle)
        except OSError as error:
            handle.close()
            raise InstanceLockError(
                f"another ChangeGuard Agent instance already holds {self.path}: {error}; "
                "task JSON files and checkpoints are single-instance, so stop the other "
                "process or give this one its own AGENT_TASK_STORE"
            ) from error
        self._handle = handle
        return self

    def release(self) -> None:
        handle, self._handle = self._handle, None
        if handle is None:
            return
        try:
            _unlock_handle(handle)
        finally:
            handle.close()

    def __enter__(self) -> "InstanceLock":
        return self.acquire()

    def __exit__(self, *_exc: object) -> None:
        self.release()


def acquire_instance_lock(store_path: str | os.PathLike[str]) -> InstanceLock:
    """按任务存储路径取独占锁；路径为空（纯内存场景）时失败关闭。"""
    if not str(store_path).strip():
        raise InstanceLockError("AGENT_TASK_STORE is required to acquire the instance lock")
    target = Path(store_path)
    return InstanceLock(target.with_name(target.name + LOCK_SUFFIX)).acquire()
