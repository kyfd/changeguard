import asyncio
from dataclasses import replace
import httpx
import pytest

from app.budget import LedgerPersistError, UsageLedger, usage_scope
from app.llm.provider import OpenAICompatibleProvider
from app.service import AgentService
from app.store.tasks import TaskRepository
from tests.conftest import complete_request, run
from tests.test_usage_budget import configured, CONTEXT


def test_reservation_persist_failure_never_sends(settings, monkeypatch):
    sent = []
    async def forbidden(*args, **kwargs):
        sent.append(True)
        raise AssertionError('network must not run')
    monkeypatch.setattr(httpx.AsyncClient, 'post', forbidden)
    ledger = UsageLedger()
    def fail(_):
        raise OSError('disk unavailable')
    ledger.on_update = fail
    async def scenario():
        with usage_scope(ledger):
            with pytest.raises(LedgerPersistError):
                await OpenAICompatibleProvider(configured(settings))._post('http://unused', {})
    run(scenario())
    assert not sent


def test_service_cancel_preserves_durable_pending_and_recovery_budget(settings, monkeypatch):
    started = asyncio.Event()
    cancelled = asyncio.Event()
    async def handler(request):
        started.set()
        try:
            await asyncio.Event().wait()
        finally:
            cancelled.set()
    transport = httpx.MockTransport(handler)
    original = httpx.AsyncClient
    monkeypatch.setattr(httpx, 'AsyncClient', lambda *a, **kw: original(*a, **{**kw, 'transport': transport}))
    scoped = configured(settings, execution_mode='background', max_task_requests=1)
    service = AgentService(scoped, provider=OpenAICompatibleProvider(scoped))
    async def scenario():
        view, _ = await service.create_task(complete_request(), CONTEXT)
        await asyncio.wait_for(started.wait(), 10)
        # Independent repository reads disk while HTTP is still in flight.
        pending = TaskRepository(scoped.task_store_path).get(view.task_id)
        assert pending['usage']['requests'] == 1
        assert pending['usage']['calls'][0]['outcome'] == 'pending'
        recovered = UsageLedger(max_requests=1)
        recovered.seed_from_prior(pending['usage'])
        assert recovered.exceeded()
        assert recovered.calls[0].execution_id == pending['execution_id']
        result = await service.cancel(view.task_id, CONTEXT)
        await asyncio.wait_for(cancelled.wait(), 10)
        persisted = TaskRepository(scoped.task_store_path).get(view.task_id)
        assert persisted['status'] == 'CANCELLED'
        assert persisted['usage']['requests'] == 1
        assert persisted['usage']['missing_responses'] == 1
        assert persisted['usage']['calls'][0]['outcome'] == 'pending'
        assert result.status.value == 'CANCELLED'
    run(scenario())


def test_nested_reservation_settlement_counts_once(settings, monkeypatch):
    async def response(*args, **kwargs):
        return httpx.Response(200, json={'usage': {'prompt_tokens': 2, 'completion_tokens': 3}})
    monkeypatch.setattr(httpx.AsyncClient, 'post', response)
    outer = UsageLedger(max_requests=1, unknown_charge_tokens=10)
    inner = UsageLedger(max_requests=1, unknown_charge_tokens=10)
    async def scenario():
        with usage_scope(outer), usage_scope(inner):
            await OpenAICompatibleProvider(configured(settings))._post('http://unused', {})
    run(scenario())
    for ledger in (outer, inner):
        assert ledger.requests == ledger.reported == 1
        assert ledger.counted_tokens == 5
        assert ledger.calls[0].outcome == 'ok'
