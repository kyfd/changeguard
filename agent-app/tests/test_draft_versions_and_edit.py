"""3.1 M2：草案版本快照、服务端版本化编辑与服务端权威关联。"""
from __future__ import annotations

import pytest

from app.schemas.drafts import (
    ConfirmRequest,
    DeterministicCheck,
    DraftEditRequest,
    LinkChangeRequest,
)
from app.service import TaskNotFound, TaskNotResumable, TaskStateUnavailable
from app.tools.registry import TrustedContext
from tests.conftest import complete_request, run

CLEAN_SQL = "SET lock_timeout = '3s';\nCREATE INDEX CONCURRENTLY idx_orders_user ON orders (user_id);"
CLEAN_ROLLBACK = "DROP INDEX CONCURRENTLY idx_orders_user;"


def _draft(**overrides):
    draft = {
        "version": 1,
        "requirement": "订单索引",
        "application": "order-service",
        "environment": "生产",
        "database": "postgresql",
        "sql": "select 1",
        "rollback_sql": "select 2",
        "deterministic_check": {"status": "PASSED", "source": "local_scan", "blocking_count": 0, "items": []},
    }
    draft.update(overrides)
    return draft


def seed(service, task_id="task_one", **overrides):
    record = dict(
        task_id=task_id,
        organization_id="org_demo",
        user_id="alice",
        requirement="订单索引",
        status="DRAFT_READY",
        slots={"application": "order-service"},
        events=[],
        source="production",
        draft=_draft(),
        draft_versions=[{
            "version": 1, "created_at": "2026-09-28T00:00:00+00:00", "origin": "agent", "actor": "agent",
            "sql": "select 1", "rollback_sql": "select 2", "check_status": "PASSED", "summary": "1 条变更语句",
        }],
    )
    record.update(overrides)
    service._repository.save(record)
    return record


def test_edit_persists_new_version_rechecks_and_diffs(service, context):
    seed(service)
    view = run(service.edit_draft("task_one", context, DraftEditRequest(
        expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK, reason="按评审意见调整")))
    assert view.draft.version == 2
    assert view.status.value == "DRAFT_READY"
    assert view.draft.deterministic_check.status == "PASSED"
    versions = run(service.draft_versions("task_one", context))
    assert [item.version for item in versions] == [1, 2]
    assert versions[-1].origin == "user_edit" and versions[-1].actor == "alice"
    assert versions[-1].reason == "按评审意见调整"
    assert versions[-1].content_hash == view.material_hash
    # 旧版本（内容与审计）保留，不被覆盖。
    assert versions[0].sql == "select 1"
    diff = run(service.draft_version_diff("task_one", context, None, 2))
    assert diff.from_version == 1 and diff.to_version == 2
    assert "-select 1" in diff.sql_diff and "+SET lock_timeout = '3s';" in diff.sql_diff
    assert service._repository.get("task_one")["draft"]["version"] == 2


def test_edit_stale_version_rejected_without_side_effect(service, context):
    seed(service)
    run(service.edit_draft("task_one", context, DraftEditRequest(
        expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))
    with pytest.raises(TaskNotResumable, match="刷新"):
        run(service.edit_draft("task_one", context, DraftEditRequest(
            expected_version=1, sql="SELECT 9;", rollback_sql="SELECT 8;")))
    latest = service._repository.get("task_one")
    assert latest["draft"]["version"] == 2
    assert latest["draft"]["sql"] == CLEAN_SQL
    assert len(latest["draft_versions"]) == 2


def test_edit_fails_closed_when_check_cannot_run(service, context, monkeypatch):
    seed(service)

    async def failing_scan(_record, _sql, _rollback):
        return DeterministicCheck(status="FAILED", source="scan_sql", error="扫描服务不可用")

    monkeypatch.setattr(service, "_scan_material", failing_scan)
    view = run(service.edit_draft("task_one", context, DraftEditRequest(
        expected_version=1, sql="DROP TABLE orders;", rollback_sql="--", reason="危险编辑")))
    # 检查失败必须失败关闭：状态是 CHECK_BLOCKED，绝不升级成 DRAFT_READY。
    assert view.status.value == "CHECK_BLOCKED"
    assert view.draft.deterministic_check.status == "FAILED"


def test_edit_invalidates_confirmation_but_keeps_versions_and_audit(service, context):
    seed(service)
    run(service.confirm("task_one", context, ConfirmRequest(
        material_hash=run(service.get_task("task_one", context)).material_hash)))
    confirmed = run(service.get_task("task_one", context))
    assert confirmed.confirmations and confirmed.confirmations[0].invalidated_at is None

    run(service.edit_draft("task_one", context, DraftEditRequest(
        expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK, reason="修订")))
    after = run(service.get_task("task_one", context))
    assert after.confirmations[0].invalidated_at is not None
    assert "编辑" in (after.confirmations[0].invalidate_reason or "")
    record = service._repository.get("task_one")
    assert len(record["draft_versions"]) == 2
    kinds = [item["kind"] for item in record["events"]]
    assert "confirmed" in kinds and "draft_edited" in kinds


@pytest.mark.parametrize("field", ["archived_at", "deleted_at"])
def test_edit_readonly_on_archived_and_trash(service, context, field):
    seed(service, **{field: "2026-09-28T00:00:00+00:00"})
    with pytest.raises(TaskNotResumable):
        run(service.edit_draft("task_one", context, DraftEditRequest(
            expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))


def test_edit_cross_owner_returns_not_found(service):
    seed(service)
    for foreign in (TrustedContext(user_id="bob", organization_id="org_demo"),
                    TrustedContext(user_id="alice", organization_id="other")):
        with pytest.raises(TaskNotFound):
            run(service.edit_draft("task_one", foreign, DraftEditRequest(
                expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))


def test_edit_persistence_failure_keeps_disk_and_memory(service, context, monkeypatch):
    seed(service)
    before = service._repository.get("task_one")

    def fail(_records):
        raise OSError("test disk failure")

    monkeypatch.setattr(service._repository, "_persist_records", fail)
    with pytest.raises(TaskStateUnavailable):
        run(service.edit_draft("task_one", context, DraftEditRequest(
            expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))
    assert service._repository.get("task_one") == before


def test_edit_cannot_overwrite_concurrent_archive(service, context, monkeypatch):
    """等待确定性扫描期间任务被归档：编辑必须失败，且归档不能被旧记录覆盖。"""
    seed(service)

    async def race_archive(_record, _sql, _rollback):
        await service.archive_task("task_one", context)
        return DeterministicCheck(status="PASSED", source="local_scan")

    monkeypatch.setattr(service, "_scan_material", race_archive)
    with pytest.raises(TaskNotResumable):
        run(service.edit_draft("task_one", context, DraftEditRequest(
            expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))
    latest = service._repository.get("task_one")
    assert latest["archived_at"], "并发归档被旧记录覆盖了"
    assert latest["draft"]["version"] == 1, "编辑本不应生效"
    assert [item["version"] for item in latest["draft_versions"]] == [1]


def test_edit_merges_concurrent_writes_instead_of_losing_them(service, context, monkeypatch):
    """等待期间由其它操作写入的字段必须保留，而不是被这次编辑的旧记录抹掉。"""
    seed(service)

    async def race_write(_record, _sql, _rollback):
        record = service._repository.get("task_one")
        record["change_links"] = [{
            "change_request_id": "chg_race", "organization_id": "org_demo",
            "linked_by": "alice", "linked_at": "2026-09-28T00:00:00+00:00",
        }]
        service._repository.save(record)
        return DeterministicCheck(status="PASSED", source="local_scan")

    monkeypatch.setattr(service, "_scan_material", race_write)
    view = run(service.edit_draft("task_one", context, DraftEditRequest(
        expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))
    assert view.draft.version == 2
    assert [item.change_request_id for item in view.change_links] == ["chg_race"]


def test_link_cannot_overwrite_concurrent_archive(service, context, monkeypatch):
    """等待治理后端核对期间任务被归档：关联必须失败，归档不能被覆盖。"""
    seed(service)

    async def race_archive(_change_id, _ctx):
        await service.archive_task("task_one", context)
        return True

    monkeypatch.setattr(service, "_change_exists", race_archive)
    with pytest.raises(TaskNotResumable):
        run(service.link_change("task_one", context, LinkChangeRequest(change_request_id="chg_1")))
    latest = service._repository.get("task_one")
    assert latest["archived_at"], "并发归档被旧记录覆盖了"
    assert not latest.get("change_links")


def test_edit_rejects_running_task(service, context):
    import asyncio
    from types import SimpleNamespace

    seed(service)
    service._executions["task_one"] = SimpleNamespace(finished=asyncio.Event())
    with pytest.raises(TaskNotResumable):
        run(service.edit_draft("task_one", context, DraftEditRequest(
            expected_version=1, sql=CLEAN_SQL, rollback_sql=CLEAN_ROLLBACK)))


def test_link_change_is_verified_idempotent_and_authoritative(service, context, monkeypatch):
    seed(service)

    async def exists(change_id, _ctx):
        return change_id.startswith("chg_")

    monkeypatch.setattr(service, "_change_exists", exists)
    first = run(service.link_change("task_one", context, LinkChangeRequest(change_request_id="chg_1")))
    assert [item.change_request_id for item in first.change_links] == ["chg_1"]
    assert first.change_links[0].organization_id == "org_demo" and first.change_links[0].linked_by == "alice"
    # 重复提交同一变更（含幂等键）不新增关联、不重复写事件。
    again = run(service.link_change("task_one", context, LinkChangeRequest(
        change_request_id="chg_1", idempotency_key="k-1")))
    assert len(again.change_links) == 1
    assert [e.kind for e in again.events].count("change_linked") == 1
    second = run(service.link_change("task_one", context, LinkChangeRequest(change_request_id="chg_2")))
    assert [item.change_request_id for item in second.change_links] == ["chg_1", "chg_2"]


def test_link_change_requires_server_verification(service, context):
    seed(service)
    # 默认没有 upstream token → 无法核对 → 失败关闭，不建立关联。
    with pytest.raises(TaskNotResumable):
        run(service.link_change("task_one", context, LinkChangeRequest(change_request_id="chg_1")))
    assert not service._repository.get("task_one").get("change_links")


def test_link_change_readonly_when_archived(service, context, monkeypatch):
    seed(service, archived_at="2026-09-28T00:00:00+00:00")

    async def exists(_change_id, _ctx):
        return True

    monkeypatch.setattr(service, "_change_exists", exists)
    with pytest.raises(TaskNotResumable):
        run(service.link_change("task_one", context, LinkChangeRequest(change_request_id="chg_1")))


def test_linked_change_blocks_recycle_bin(service, context, monkeypatch):
    seed(service, status="FAILED", archived_at="2026-09-28T00:00:00+00:00", draft=None)

    async def exists(_change_id, _ctx):
        return True

    monkeypatch.setattr(service, "_change_exists", exists)
    # 先归档再关联是矛盾的（关联要求活跃）；直接写入关联后验证删除预览被阻断。
    record = service._repository.get("task_one")
    record["change_links"] = [{"change_request_id": "chg_1", "organization_id": "org_demo",
                               "linked_by": "alice", "linked_at": "2026-09-28T00:00:00+00:00"}]
    service._repository.save(record)
    preview = run(service.delete_preview("task_one", context))
    assert not preview.allowed
    assert any("正式变更关联" in item for item in preview.blockers)


def test_api_draft_trace_versions_and_auth(tmp_path):
    from tests.test_api import HEADERS, build_client

    with build_client(tmp_path) as client:
        service = client.app.state.service
        seed(service)
        base = "/api/agent/tasks/task_one"
        assert client.get(base + "/trace", headers=HEADERS).status_code == 200
        assert client.get(base + "/draft/versions", headers=HEADERS).json()[0]["version"] == 1
        # 缺少身份 → 401。
        assert client.get(base + "/trace").status_code == 401
        # 缺少 expected_version → 422。
        assert client.post(base + "/draft", headers=HEADERS, json={"sql": "SELECT 1"}).status_code == 422
        edited = client.post(base + "/draft", headers=HEADERS, json={
            "expected_version": 1, "sql": CLEAN_SQL, "rollback_sql": CLEAN_ROLLBACK, "reason": "api"})
        assert edited.status_code == 200 and edited.json()["draft"]["version"] == 2
        # 陈旧版本 → 409。
        stale = client.post(base + "/draft", headers=HEADERS, json={
            "expected_version": 1, "sql": "SELECT 2", "rollback_sql": "SELECT 3"})
        assert stale.status_code == 409
        diff = client.get(base + "/draft/diff", headers=HEADERS, params={"from": 1, "to": 2})
        assert diff.status_code == 200 and "idx_orders_user" in diff.json()["sql_diff"]
        # trace 与 versions 对越权返回 404。
        assert client.get("/api/agent/tasks/task_missing/trace", headers=HEADERS).status_code == 404


def test_api_change_link_requires_verification_and_is_readonly_when_archived(tmp_path, monkeypatch):
    from tests.test_api import HEADERS, build_client

    with build_client(tmp_path) as client:
        service = client.app.state.service
        seed(service)
        base = "/api/agent/tasks/task_one"

        async def not_found(_change_id, _ctx):
            return False

        monkeypatch.setattr(service, "_change_exists", not_found)
        # 服务端无法核对正式变更单 → 409，且不写入关联。
        denied = client.post(base + "/change-links", headers=HEADERS, json={"change_request_id": "chg_x"})
        assert denied.status_code == 409 and not service._repository.get("task_one").get("change_links")

        async def exists(_change_id, _ctx):
            return True

        monkeypatch.setattr(service, "_change_exists", exists)
        ok = client.post(base + "/change-links", headers=HEADERS, json={"change_request_id": "chg_x"})
        assert ok.status_code == 200 and ok.json()["change_links"][0]["change_request_id"] == "chg_x"

        # 归档任务只读：新接口不能绕过生命周期限制。
        archived = service._repository.get("task_one")
        archived["archived_at"] = "2026-09-28T00:00:00+00:00"
        service._repository.save(archived)
        assert client.post(base + "/change-links", headers=HEADERS, json={"change_request_id": "chg_y"}).status_code == 409
