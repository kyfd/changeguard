"""3.1 task workspace: authorization, reversible deletion and persistence failures."""
from __future__ import annotations

import argparse
import asyncio
from dataclasses import replace

import pytest

from app.config import Settings
from app.service import AgentService, TaskNotFound, TaskNotResumable, TaskStateUnavailable
from app.tools.registry import TrustedContext
from evals.run_eval import build_settings
from tests.conftest import bare_request, run
from tests.test_api import HEADERS, build_client


def seed(service, task_id="task_one", **overrides):
    record = dict(task_id=task_id, organization_id="org_demo", user_id="alice",
                  requirement="订单索引", status="FAILED", slots={"application": "order-service"},
                  events=[], source="production")
    record.update(overrides)
    service._repository.save(record)
    return record


def test_source_is_server_owned_and_legacy_not_guessed(settings, context):
    service = AgentService(replace(settings, task_source="evaluation"))
    task, _ = run(service.create_task(bare_request(), context))
    assert task.source == "evaluation" and task.created_at is not None
    assert run(service.list_tasks(context)) == []
    assert len(run(service.list_tasks(context, source="evaluation"))) == 1
    legacy = seed(service, "legacy", source=None, requirement="[eval] title is not trusted")
    assert run(service.get_task(legacy['task_id'], context)).source == "legacy"
    assert len(run(service.list_tasks(context))) == 1


def test_filters_do_not_leak_other_owners(service, context):
    seed(service)
    seed(service, "foreign", organization_id="another")
    seed(service, "colleague", user_id="bob")
    seed(service, "eval", source="evaluation")
    seed(service, "demo", source="demo")
    seed(service, "archived", archived_at="2026-09-28T00:00:00Z")
    seed(service, "trash", archived_at="2026-09-28T00:00:00Z", deleted_at="2026-09-28T00:01:00Z")
    assert [v.task_id for v in run(service.list_tasks(context))] == ["task_one"]
    assert len(run(service.list_tasks(context, source="all", workspace="all"))) == 5
    assert [v.task_id for v in run(service.list_tasks(context, q="ORDER-SERVICE", status="FAILED"))] == ["task_one"]
    assert run(service.list_tasks(context, q="no match")) == []
    assert [v.task_id for v in run(service.list_tasks(context, workspace="trash"))] == ["trash"]
    assert run(service.list_tasks(TrustedContext(user_id="", organization_id="org_demo"), source="all", workspace="all")) == []


def test_archive_delete_restore_survives_restart(settings, service, context):
    seed(service, usage={"requests": 3})
    assert not run(service.delete_preview("task_one", context)).allowed
    archived = run(service.archive_task("task_one", context))
    assert archived.archived_at
    assert len(run(service.archive_task("task_one", context)).events) == 1
    preview = run(service.delete_preview("task_one", context))
    assert preview.allowed and preview.effect == "recycle_bin"
    trashed = run(service.delete_task("task_one", context, preview.record_version))
    assert trashed.deleted_at and trashed.usage == {"requests": 3}
    service = AgentService(settings)
    assert run(service.list_tasks(context)) == []
    assert len(run(service.list_tasks(context, workspace="trash"))) == 1
    restored = run(service.restore_task("task_one", context))
    assert restored.archived_at and not restored.deleted_at
    restored = run(service.restore_task("task_one", context))
    assert not restored.archived_at
    assert [e.kind for e in restored.events] == ['archived', 'moved_to_trash', 'restored', 'restored']
    assert len(run(service.list_tasks(context))) == 1


@pytest.mark.parametrize("field,value", [
    ("draft", {"sql": "select 1"}), ("confirmations", [{"id": "confirmation"}]),
    ("change_request_id", "chg_real"), ("events", [{"kind": "generate_draft"}]),
    ("status", "NEEDS_INFO"), ("status", "DRAFT_READY"), ("status", "RUNNING"),
])
def test_linked_material_and_unsafe_status_cannot_be_deleted(service, context, field, value):
    seed(service, archived_at="2026-09-28T00:00:00Z", **{field: value})
    preview = run(service.delete_preview("task_one", context))
    assert not preview.allowed
    with pytest.raises(TaskNotResumable):
        run(service.delete_task("task_one", context, preview.record_version))


def test_stale_preview_rejected_without_side_effect(service, context):
    seed(service)
    run(service.archive_task("task_one", context))
    preview = run(service.delete_preview("task_one", context))
    changed = service._repository.get("task_one")
    changed['requirement'] = "updated after preview"
    service._repository.save(changed)
    with pytest.raises(TaskNotResumable, match="重新预览"):
        run(service.delete_task("task_one", context, preview.record_version))
    assert service._repository.get("task_one") == changed


@pytest.mark.parametrize("operation", ["archive_task", "restore_task", "delete_preview", "delete_task"])
def test_management_cross_org_and_owner_return_not_found(service, operation):
    seed(service)
    for context in (TrustedContext(user_id="alice", organization_id="other"), TrustedContext(user_id="bob", organization_id="org_demo")):
        args = ("task_one", context, "0" * 64) if operation == "delete_task" else ("task_one", context)
        with pytest.raises(TaskNotFound):
            run(getattr(service, operation)(*args))


@pytest.mark.parametrize("operation", ["archive_task", "restore_task", "delete_task"])
def test_failed_management_write_keeps_disk_and_memory(service, context, monkeypatch, operation):
    seed(service, archived_at=None if operation == "archive_task" else "2026-09-28T00:00:00Z")
    before = service._repository.get("task_one")
    disk = service._repository._path.read_bytes()
    preview = run(service.delete_preview("task_one", context))
    def fail(_records):
        raise OSError("test disk failure")
    monkeypatch.setattr(service._repository, "_persist_records", fail)
    args = ("task_one", context, preview.record_version) if operation == "delete_task" else ("task_one", context)
    with pytest.raises(TaskStateUnavailable):
        run(getattr(service, operation)(*args))
    assert service._repository.get("task_one") == before
    assert service._repository._path.read_bytes() == disk


def test_running_archive_rejected(service, context):
    seed(service, status="RUNNING")
    with pytest.raises(TaskNotResumable):
        run(service.archive_task("task_one", context))


def test_resume_checkpoint_race_cannot_bypass_archive(service, context, monkeypatch):
    seed(service)
    async def inspect(_task_id):
        await service.archive_task("task_one", context)
        return {"interrupt": True, "next": [], "input_version": ""}
    monkeypatch.setattr(service, "_inspect_checkpoint", inspect)
    with pytest.raises(TaskNotResumable, match="归档"):
        run(service.resume("task_one", context))
    assert service._repository.get("task_one")["archived_at"]
    assert not service._executions


def test_live_execution_blocks_archive_even_with_terminal_record(service, context):
    from types import SimpleNamespace
    seed(service)
    service._executions['task_one'] = SimpleNamespace(finished=asyncio.Event())
    with pytest.raises(TaskNotResumable):
        run(service.archive_task("task_one", context))


def test_checkpoint_events_cannot_erase_management_audit(service, context):
    from app.service import _annotate_recovery
    seed(service)
    run(service.archive_task("task_one", context))
    run(service.restore_task("task_one", context))
    record = service._repository.get("task_one")
    record['events'] = []  # old workflow checkpoint predates management operations
    _annotate_recovery(record)
    _annotate_recovery(record)
    assert [e['kind'] for e in record['events']] == ['archived', 'restored']


def test_api_lifecycle_auth_validation_and_readonly(tmp_path):
    with build_client(tmp_path) as client:
        service = client.app.state.service
        seed(service)
        base = "/api/agent/tasks/task_one"
        assert client.get('/api/agent/tasks', headers=HEADERS, params={'source': 'invalid'}).status_code == 422
        assert client.get('/api/agent/tasks', headers=HEADERS, params={'q': 'x' * 201}).status_code == 422
        assert client.post(base + '/archive').status_code == 401
        assert client.post(base + '/archive', headers=HEADERS).status_code == 200
        for op, body in [('clarify', {}), ('resume', {}), ('cancel', None), ('confirm', {})]:
            assert client.post(base + '/' + op, headers=HEADERS, json=body).status_code == 409
        preview = client.get(base + '/delete-preview', headers=HEADERS).json()
        assert preview['allowed']
        assert client.post(base + '/delete', headers=HEADERS, json={}).status_code == 422
        assert client.post(base + '/delete', headers=HEADERS, json={'record_version': preview['record_version']}).status_code == 200
        assert client.get('/api/agent/tasks', headers=HEADERS).json() == []
        assert len(client.get('/api/agent/tasks?workspace=trash', headers=HEADERS).json()) == 1
        assert client.post(base + '/restore', headers=HEADERS).json()['archived_at']


def test_eval_overrides_cannot_target_production_store(tmp_path):
    args = argparse.Namespace(provider='scripted', strategy='bounded_agent')
    settings = build_settings(tmp_path, args, {'task_store_path': 'production.json', 'checkpoint_path': 'prod.sqlite', 'task_source': 'production'})
    assert settings.task_source == 'evaluation'
    assert str(tmp_path) in settings.task_store_path and 'eval-tasks-' in settings.task_store_path
    assert str(tmp_path) in settings.checkpoint_path and 'eval-checkpoint-' in settings.checkpoint_path
    with pytest.raises(ValueError):
        Settings(task_source='legacy')


def test_api_cannot_spoof_task_source(tmp_path):
    with build_client(tmp_path) as client:
        response = client.post('/api/agent/tasks', headers={**HEADERS, 'X-Task-Source': 'evaluation'}, json={
            'requirement': '准备索引变更', 'source': 'evaluation', 'archived_at': '2026-09-28T00:00:00Z',
        })
        assert response.status_code == 202
        assert response.json()['source'] == 'production'
        assert response.json()['archived_at'] is None
