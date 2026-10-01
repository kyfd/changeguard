"""3.1.2 A/B：统一应用身份与结构快照生命周期。

- 应用绑定只认 canonical ID：授权由治理服务核对，名称仅作展示；
- 绑定在创建/补充/恢复时都重新核对——撤权或治理不可达一律阻断（失败关闭）；
- 知识快照选用后正文物化为任务材料；二选一冲突显式拒绝；失效阻断并提示重新选择。
"""

from __future__ import annotations

import pytest

from app.schemas.drafts import ClarifyRequest
from app.schemas.knowledge import KnowledgeImportRequest
from app.service import (
    ApplicationNotAuthorized,
    TaskInputInvalid,
    _record_draft_version,
)
from app.tools.registry import TrustedContext
from tests.conftest import SCHEMA_SNAPSHOT, bare_request, run
from tests.test_knowledge import NORM_BODY

CONTEXT = TrustedContext(user_id="alice", organization_id="org_demo")

SCHEMA_BODY = """-- orders 表结构快照（合成数据）
CREATE TABLE orders (id bigint primary key, user_id bigint not null, created_at timestamptz);
CREATE INDEX idx_orders_created ON orders (created_at);
"""

MANUAL_BODY = "CREATE TABLE legacy_t (id int);"


def grant_applications(service, monkeypatch, allowed=None, names=None):
    """模拟治理服务 `GET /api/agent-tools/applications/{id}` 的核对结果。

    `allowed=None` 表示治理服务不可达（核对一律失败）；集合表示已授权清单。
    """
    names = names or {}


    async def verify(_context, application_id):
        application = (application_id or "").strip()
        if allowed is None or not application or application not in allowed:
            return None
        return {"id": application, "name": names.get(application) or application}

    async def check(_context, application_id):
        application = (application_id or "").strip()
        return allowed is not None and bool(application) and application in allowed

    monkeypatch.setattr(service, "_verify_application", verify)
    monkeypatch.setattr(service, "_application_authorized", check)


def import_snapshot(service, context, **overrides):
    payload = dict(
        kind="schema", title="orders 结构快照", body=SCHEMA_BODY, version="v1", source="examples/schema"
    )
    payload.update(overrides)
    return run(service.import_knowledge(KnowledgeImportRequest(**payload), context))


def seed(service, task_id="task_old", **overrides):
    record = dict(
        task_id=task_id, organization_id="org_demo", user_id="alice",
        requirement="订单索引", status="NEEDS_INFO", slots={}, events=[], source="production",
    )
    record.update(overrides)
    service._repository.save(record)
    return record


# ---------------------------------------------------------------------------
# A：应用绑定（canonical ID）
# ---------------------------------------------------------------------------


def test_create_binds_verified_application_and_backfills_name(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"}, names={"app-orders": "订单服务"})
    view, _ = run(
        service.create_task(
            bare_request(application_id="app-orders", application="随便填的名称"), context
        )
    )

    assert view.application_binding == "authorized"
    assert view.authorized_application == "app-orders"
    assert view.slots.application_id == "app-orders"
    # 展示名称以治理服务返回为准，不信任客户端提交的名称。
    assert view.slots.application == "订单服务"

    stored = service._repository.get(view.task_id)
    assert stored["application_binding"] == "authorized"
    assert stored["authorized_application"] == "app-orders"


def test_create_fails_closed_when_application_cannot_be_verified(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=None)  # 治理服务不可达
    with pytest.raises(ApplicationNotAuthorized):
        run(service.create_task(bare_request(application_id="app-orders"), context))
    # 未落盘：核对失败的任务不能留下"看起来已创建"的记录。
    assert run(service.list_tasks(context)) == []


def test_create_with_name_only_is_legacy_not_authorized(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    view, _ = run(service.create_task(bare_request(application="order-service"), context))

    assert view.application_binding == "legacy"
    assert view.authorized_application == ""
    assert view.slots.application_id == ""
    assert view.slots.application == "order-service"


def test_create_without_any_application_is_none(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    view, _ = run(service.create_task(bare_request(), context))
    assert view.application_binding == "none"
    assert view.authorized_application == ""


def test_clarify_blocked_when_binding_revoked(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    view, _ = run(service.create_task(bare_request(application_id="app-orders"), context))
    assert view.application_binding == "authorized"

    # 授权被回收：任何补充都必须先解决授权，而不是带着旧授权继续。
    grant_applications(service, monkeypatch, allowed=None)
    with pytest.raises(ApplicationNotAuthorized):
        run(service.clarify(view.task_id, ClarifyRequest(note="补充说明"), context))

    after = run(service.get_task(view.task_id, context))
    assert after.slots.application_id == "app-orders"
    assert after.authorized_application == "app-orders"


def test_clarify_rebinds_application(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    view, _ = run(service.create_task(bare_request(), context))
    assert view.application_binding == "none"

    grant_applications(service, monkeypatch, allowed={"app-orders"}, names={"app-orders": "订单服务"})
    updated = run(
        service.clarify(view.task_id, ClarifyRequest(application_id="app-orders"), context)
    )
    assert updated.application_binding == "authorized"
    assert updated.slots.application_id == "app-orders"
    assert updated.slots.application == "订单服务"


def test_clarify_empty_application_id_is_rejected(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    view, _ = run(service.create_task(bare_request(), context))
    with pytest.raises(TaskInputInvalid):
        run(service.clarify(view.task_id, ClarifyRequest(application_id=""), context))


def test_clarify_legacy_task_keeps_org_scoped_retrieval(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    view, _ = run(service.create_task(bare_request(application="order-service"), context))
    updated = run(service.clarify(view.task_id, ClarifyRequest(note="补充说明"), context))
    assert updated.application_binding == "legacy"
    assert updated.authorized_application == ""


def test_resume_reverifies_binding_before_continuing(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    view, _ = run(service.create_task(bare_request(application_id="app-orders"), context))

    grant_applications(service, monkeypatch, allowed=None)
    with pytest.raises(ApplicationNotAuthorized):
        run(service.resume(view.task_id, context, None))


def test_view_derives_binding_for_records_without_it(service, context):
    """旧记录没有显式绑定状态：按已核对应用与槽位回退推导（只影响展示）。"""
    seed(service)  # 全空 → none
    seed(service, "task_name", slots={"application": "order-service"})  # 仅名称 → legacy
    seed(service, "task_id_only", slots={"application_id": "app-1"})  # 有 ID 无授权 → unauthorized
    seed(
        service,
        "task_verified",
        slots={"application_id": "app-1", "application": "x"},
        authorized_application="app-1",
    )  # 一致 → authorized

    bindings = {
        view.task_id: view.application_binding
        for view in run(service.list_tasks(context, source="all", workspace="all"))
    }
    assert bindings == {
        "task_old": "none",
        "task_name": "legacy",
        "task_id_only": "unauthorized",
        "task_verified": "authorized",
    }


# ---------------------------------------------------------------------------
# B：结构快照生命周期
# ---------------------------------------------------------------------------


def test_create_selects_knowledge_snapshot_and_materializes_body(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snap = import_snapshot(service, context, application_id="app-orders")

    view, _ = run(
        service.create_task(
            bare_request(application_id="app-orders", snapshot_knowledge_id=snap.knowledge_id), context
        )
    )

    assert view.selected_snapshot is not None
    assert view.selected_snapshot.knowledge_id == snap.knowledge_id
    assert view.selected_snapshot.content_hash == snap.content_hash
    assert view.selected_snapshot.application_id == "app-orders"
    assert view.selected_snapshot.selected_by == "alice"

    stored = service._repository.get(view.task_id)
    # 正文物化为任务材料：此后由任务记录持有，知识库后续失效不影响已生成版本的追溯。
    assert stored["schema_snapshot"] == SCHEMA_BODY
    assert stored["snapshot_source_kind"] == "knowledge"
    assert any(event["kind"] == "snapshot_selected" for event in stored["events"])


def test_create_rejects_knowledge_snapshot_plus_manual_snapshot(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    snap = import_snapshot(service, context)
    with pytest.raises(TaskInputInvalid):
        run(
            service.create_task(
                bare_request(
                    snapshot_knowledge_id=snap.knowledge_id, schema_snapshot=SCHEMA_SNAPSHOT
                ),
                context,
            )
        )


def test_create_rejects_non_schema_and_inactive_snapshot(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    norm = run(
        service.import_knowledge(
            KnowledgeImportRequest(kind="norms", title="规范", body=NORM_BODY, version="v1", source="x"),
            context,
        )
    )
    with pytest.raises(TaskInputInvalid):
        run(service.create_task(bare_request(snapshot_knowledge_id=norm.knowledge_id), context))

    snap = import_snapshot(service, context)
    run(service.deprecate_knowledge(snap.knowledge_id, context))
    with pytest.raises(TaskInputInvalid):
        run(service.create_task(bare_request(snapshot_knowledge_id=snap.knowledge_id), context))


def test_create_rejects_application_scoped_snapshot_without_matching_binding(
    service, context, monkeypatch
):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snap = import_snapshot(service, context, application_id="app-orders")

    # 任务未绑定应用：先选应用再选快照。
    with pytest.raises(TaskInputInvalid):
        run(service.create_task(bare_request(snapshot_knowledge_id=snap.knowledge_id), context))

    # 绑定了其他应用：快照必须属于当前应用。
    grant_applications(service, monkeypatch, allowed={"app-billing"})
    with pytest.raises(TaskInputInvalid):
        run(
            service.create_task(
                bare_request(application_id="app-billing", snapshot_knowledge_id=snap.knowledge_id),
                context,
            )
        )


def test_clarify_switch_manual_to_knowledge_requires_explicit_clear(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snap = import_snapshot(service, context, application_id="app-orders")
    view, _ = run(
        service.create_task(bare_request(application_id="app-orders", schema_snapshot=MANUAL_BODY), context)
    )

    # 已有手填快照：不显式确认替换就拒绝，绝不静默覆盖。
    with pytest.raises(TaskInputInvalid):
        run(
            service.clarify(
                view.task_id, ClarifyRequest(snapshot_knowledge_id=snap.knowledge_id), context
            )
        )

    # schema_snapshot 显式置空 = 确认替换。
    run(
        service.clarify(
            view.task_id,
            ClarifyRequest(snapshot_knowledge_id=snap.knowledge_id, schema_snapshot=""),
            context,
        )
    )
    stored = service._repository.get(view.task_id)
    assert stored["schema_snapshot"] == SCHEMA_BODY
    assert stored["snapshot_source_kind"] == "knowledge"
    assert stored["selected_snapshot"]["knowledge_id"] == snap.knowledge_id


def test_clarify_clear_knowledge_selection_also_clears_body(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snap = import_snapshot(service, context, application_id="app-orders")
    view, _ = run(
        service.create_task(
            bare_request(application_id="app-orders", snapshot_knowledge_id=snap.knowledge_id), context
        )
    )

    run(service.clarify(view.task_id, ClarifyRequest(snapshot_knowledge_id=""), context))
    stored = service._repository.get(view.task_id)
    assert stored["selected_snapshot"] is None
    assert stored["schema_snapshot"] == "", "正文来自知识快照时，清除选用必须一并清空"
    assert stored["snapshot_source_kind"] == ""
    assert any(event["kind"] == "snapshot_cleared" for event in stored["events"])


def test_clarify_manual_body_over_knowledge_snapshot_is_rejected(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snap = import_snapshot(service, context, application_id="app-orders")
    view, _ = run(
        service.create_task(
            bare_request(application_id="app-orders", snapshot_knowledge_id=snap.knowledge_id), context
        )
    )
    with pytest.raises(TaskInputInvalid):
        run(
            service.clarify(
                view.task_id, ClarifyRequest(schema_snapshot="CREATE TABLE x (id int);"), context
            )
        )
    # 拒绝后正文不被部分改写。
    stored = service._repository.get(view.task_id)
    assert stored["schema_snapshot"] == SCHEMA_BODY
    assert stored["snapshot_source_kind"] == "knowledge"


def test_clarify_blocked_when_selected_snapshot_invalidated(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snap = import_snapshot(service, context, application_id="app-orders")
    view, _ = run(
        service.create_task(
            bare_request(application_id="app-orders", snapshot_knowledge_id=snap.knowledge_id), context
        )
    )

    run(service.deprecate_knowledge(snap.knowledge_id, context))
    with pytest.raises(TaskInputInvalid):
        run(service.clarify(view.task_id, ClarifyRequest(note="继续"), context))


def test_input_version_tracks_snapshot_changes(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed=set())
    view, _ = run(service.create_task(bare_request(schema_snapshot=MANUAL_BODY), context))
    first = service._repository.get(view.task_id)["input_version"]

    run(service.clarify(view.task_id, ClarifyRequest(schema_snapshot="CREATE TABLE other (id int);"), context))
    second = service._repository.get(view.task_id)["input_version"]
    assert first != second, "快照变化必须改变输入版本（并使旧草案/确认失效）"


def test_draft_versions_carry_snapshot_provenance():
    record = {
        "draft": {"sql": "CREATE INDEX idx ON t (c);", "rollback_sql": "DROP INDEX idx;", "version": 1},
        "selected_snapshot": {"knowledge_id": "k1", "version": "v1", "content_hash": "abc"},
    }
    assert _record_draft_version(record, origin="agent", actor="alice", reason="生成") is True
    entry = record["draft_versions"][0]
    assert entry["snapshot_source"]["knowledge_id"] == "k1"
    assert entry["snapshot_source"]["content_hash"] == "abc"
