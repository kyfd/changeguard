"""4.0 单实例守卫回归。

任务 JSON、知识/评测 JSON 与检查点 SQLite 只支持单实例语义，所以第二个进程
必须在启动期失败关闭。这里同时覆盖锁本身与 FastAPI lifespan 的真实接线。
"""

from __future__ import annotations

from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app.config import Settings
from app.instance_lock import InstanceLockError, acquire_instance_lock
from app.main import create_app


def _settings(tmp_path: Path) -> Settings:
    return Settings(task_store_path=str(tmp_path / "agent-tasks.json"))


def test_second_instance_fails_closed(tmp_path: Path) -> None:
    store = tmp_path / "agent-tasks.json"
    first = acquire_instance_lock(store)
    try:
        with pytest.raises(InstanceLockError) as failure:
            acquire_instance_lock(store)
        message = str(failure.value)
        assert "agent-tasks.json.instance.lock" in message
        assert "AGENT_TASK_STORE" in message
        assert (tmp_path / "agent-tasks.json.instance.lock").exists()
    finally:
        first.release()


def test_release_is_idempotent_and_allows_a_new_instance(tmp_path: Path) -> None:
    store = tmp_path / "agent-tasks.json"
    lock = acquire_instance_lock(store)
    lock.release()
    lock.release()
    again = acquire_instance_lock(store)
    again.release()


def test_locks_are_per_store_path(tmp_path: Path) -> None:
    first = acquire_instance_lock(tmp_path / "one.json")
    try:
        other = acquire_instance_lock(tmp_path / "two.json")
        other.release()
    finally:
        first.release()


def test_empty_store_path_is_rejected() -> None:
    with pytest.raises(InstanceLockError):
        acquire_instance_lock("   ")


def test_lifespan_holds_the_lock_for_the_running_app(tmp_path: Path) -> None:
    settings = _settings(tmp_path)
    application = create_app(settings)
    with TestClient(application):
        with pytest.raises(InstanceLockError):
            acquire_instance_lock(settings.task_store_path)
    # lifespan 退出后锁必须释放，否则重启会永久失败。
    released = acquire_instance_lock(settings.task_store_path)
    released.release()


def test_two_running_apps_cannot_share_one_store(tmp_path: Path) -> None:
    settings = _settings(tmp_path)
    with TestClient(create_app(settings)):
        with pytest.raises(InstanceLockError):
            with TestClient(create_app(settings)):
                pass
