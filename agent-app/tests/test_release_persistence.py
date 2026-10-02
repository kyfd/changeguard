"""3.1.3 restart regressions, not a production backup/restore rehearsal.

Use real JSON/SQLite storage and deterministic generation. Only the governance
application lookup is stubbed; authorization on task ownership is not bypassed.
"""
from __future__ import annotations

import hashlib

import pytest

from app.schemas.drafts import ClarifyRequest
from app.service import AgentService, ApplicationNotAuthorized, TaskInputInvalid, TaskNotFound
from app.tools.registry import TrustedContext
from tests.conftest import bare_request, complete_request, run
from tests.test_application_binding import SCHEMA_BODY, grant_applications, import_snapshot


def create_bound_task(service, context, monkeypatch):
    grant_applications(service, monkeypatch, allowed={"app-orders"}, names={"app-orders": "订单服务"})
    snapshot = import_snapshot(service, context, application_id="app-orders")
    view, _ = run(service.create_task(
        bare_request(application_id="app-orders", snapshot_knowledge_id=snapshot.knowledge_id), context))
    return view, snapshot


def test_binding_and_snapshot_survive_service_restart(settings, service, context, monkeypatch):
    view, snapshot = create_bound_task(service, context, monkeypatch)
    before = service._repository.get(view.task_id)
    restarted = AgentService(settings)
    loaded = run(restarted.get_task(view.task_id, context))
    assert loaded.slots.application_id == "app-orders"
    assert loaded.slots.application == "订单服务"
    assert loaded.selected_snapshot.knowledge_id == snapshot.knowledge_id
    assert loaded.selected_snapshot.content_hash == snapshot.content_hash
    assert loaded.snapshot_source_kind == "knowledge"
    assert restarted._repository.get(view.task_id) == before
    assert restarted._repository.get(view.task_id)["schema_snapshot"] == SCHEMA_BODY


@pytest.mark.parametrize("operation", ["clarify", "resume"])
def test_restart_does_not_turn_old_authorization_into_a_credential(
    settings, service, context, monkeypatch, operation
):
    view, _ = create_bound_task(service, context, monkeypatch)
    restarted = AgentService(settings)
    grant_applications(restarted, monkeypatch, allowed=set())
    before = restarted._repository.get(view.task_id)
    with pytest.raises(ApplicationNotAuthorized):
        if operation == "clarify":
            run(restarted.clarify(view.task_id, ClarifyRequest(note="继续"), context))
        else:
            run(restarted.resume(view.task_id, context))
    assert restarted._repository.get(view.task_id) == before


@pytest.mark.parametrize("change", ["deprecated", "content_updated"])
@pytest.mark.parametrize("operation", ["clarify", "resume"])
def test_restart_revalidates_snapshot_without_overwriting_historical_source(
    settings, service, context, monkeypatch, change, operation
):
    view, snapshot = create_bound_task(service, context, monkeypatch)
    before = service._repository.get(view.task_id)
    if change == "deprecated":
        run(service.deprecate_knowledge(snapshot.knowledge_id, context))
    else:
        # Simulate an externally updated knowledge record; no public update API is implied.
        record = service._knowledge.get(snapshot.knowledge_id)
        record["body"] += "\nALTER TABLE orders ADD COLUMN status text;"
        record["content_hash"] = hashlib.sha256(record["body"].encode()).hexdigest()
        service._knowledge.save(record)
    restarted = AgentService(settings)
    grant_applications(restarted, monkeypatch, allowed={"app-orders"})
    with pytest.raises(TaskInputInvalid, match="重新选择"):
        if operation == "clarify":
            run(restarted.clarify(view.task_id, ClarifyRequest(note="继续"), context))
        else:
            run(restarted.resume(view.task_id, context))
    assert restarted._repository.get(view.task_id) == before
    assert run(restarted.get_task(view.task_id, context)).selected_snapshot.content_hash == snapshot.content_hash


@pytest.mark.parametrize("outsider", [
    TrustedContext(user_id="alice", organization_id="another-org"),
    TrustedContext(user_id="another-user", organization_id="org_demo"),
])
def test_restart_preserves_task_owner_and_organization_boundary(
    settings, service, context, monkeypatch, outsider
):
    view, _ = create_bound_task(service, context, monkeypatch)
    restarted = AgentService(settings)
    with pytest.raises(TaskNotFound):
        run(restarted.get_task(view.task_id, outsider))
    assert run(restarted.list_tasks(outsider, source="all", workspace="all")) == []


def test_generated_draft_provenance_survives_restart_and_snapshot_deprecation(
    settings, service, context, monkeypatch
):
    grant_applications(service, monkeypatch, allowed={"app-orders"})
    snapshot = import_snapshot(service, context, application_id="app-orders")
    view, _ = run(service.create_task(complete_request(
        application_id="app-orders", schema_snapshot="", snapshot_knowledge_id=snapshot.knowledge_id), context))
    assert view.draft is not None
    versions = run(service.draft_versions(view.task_id, context))
    assert versions and versions[-1].snapshot_source["knowledge_id"] == snapshot.knowledge_id
    run(service.deprecate_knowledge(snapshot.knowledge_id, context))
    restarted = AgentService(settings)
    assert run(restarted.draft_versions(view.task_id, context)) == versions
    assert run(restarted.get_task(view.task_id, context)).material_hash == view.material_hash


def test_legacy_name_only_task_is_not_silently_bound_after_restart(
    settings, service, context, monkeypatch
):
    view, _ = run(service.create_task(bare_request(application="订单服务"), context))
    restarted = AgentService(settings)
    grant_applications(restarted, monkeypatch, allowed={"app-orders"}, names={"app-orders": "订单服务"})
    loaded = run(restarted.get_task(view.task_id, context))
    assert loaded.application_binding == "legacy"
    assert loaded.slots.application_id == ""
    updated = run(restarted.clarify(view.task_id, ClarifyRequest(application_id="app-orders"), context))
    assert updated.slots.application_id == "app-orders"
    assert updated.application_binding == "authorized"
