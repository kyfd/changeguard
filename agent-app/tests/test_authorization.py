"""任务授权与检索租户隔离。

这些用例锁定改造前真实存在的缺陷：`GET /tasks`、`GET /tasks/{id}`、`clarify`、`cancel`
只做认证、不校验归属——路由层调用了 `resolve_context` 却丢弃返回值，
于是任何已认证调用方都能读到或操作**其他组织**（以及同组织其他用户）的任务。

另一组锁定检索层：`Chunk.organization_id` 字段虽然存在，但 `search` 只按 doc_id 前缀过滤，
从不比对组织，因此带组织标记的私有语料对所有调用方可见。

判定口径：**无权与不存在必须不可区分**（同为 404 且响应体相同），
否则调用方可以用状态码差异探测其他组织是否存在某个任务。
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app.config import Settings
from app.main import create_app
from app.retrieval.base import Chunk
from app.retrieval.keyword import HybridRetriever, KeywordRetriever
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from tests.conftest import DEMO_DIR, REQUIREMENT, run

ALICE = {"X-Actor-Id": "alice", "X-Org-Id": "org_demo"}
BOB_SAME_ORG = {"X-Actor-Id": "bob", "X-Org-Id": "org_demo"}
MALLORY_OTHER_ORG = {"X-Actor-Id": "mallory", "X-Org-Id": "org_attacker"}


def build_client(tmp_path: Path, *, store_path: Path | None = None) -> TestClient:
    settings = Settings(
        agent_demo_dir=str(DEMO_DIR),
        task_store_path=str(store_path or (tmp_path / "agent-tasks.json")),
        execution_mode="inline",
        allow_header_identity=True,
        upstream_token="unit-test-secret",
    )
    return TestClient(create_app(settings), headers={"X-Agent-Upstream-Token": "unit-test-secret"})


def create_task(client: TestClient, headers: dict[str, str]) -> str:
    response = client.post("/api/agent/tasks", headers=headers, json={"requirement": REQUIREMENT})
    assert response.status_code == 202
    return response.json()["task_id"]


# ---------------------------------------------------------------------------
# 任务归属
# ---------------------------------------------------------------------------


def test_task_list_is_scoped_to_the_creator(tmp_path: Path) -> None:
    client = build_client(tmp_path)
    task_id = create_task(client, ALICE)

    own = client.get("/api/agent/tasks", headers=ALICE)
    assert own.status_code == 200
    assert [item["task_id"] for item in own.json()] == [task_id]

    # 同组织其他用户与跨组织用户都看不到：默认策略是仅创建者可操作。
    assert client.get("/api/agent/tasks", headers=BOB_SAME_ORG).json() == []
    assert client.get("/api/agent/tasks", headers=MALLORY_OTHER_ORG).json() == []


@pytest.mark.parametrize("headers", [BOB_SAME_ORG, MALLORY_OTHER_ORG])
def test_other_callers_cannot_read_a_task(tmp_path: Path, headers: dict[str, str]) -> None:
    client = build_client(tmp_path)
    task_id = create_task(client, ALICE)

    assert client.get(f"/api/agent/tasks/{task_id}", headers=headers).status_code == 404
    assert client.get(f"/api/agent/tasks/{task_id}", headers=ALICE).status_code == 200


@pytest.mark.parametrize("headers", [BOB_SAME_ORG, MALLORY_OTHER_ORG])
def test_other_callers_cannot_clarify_a_task(tmp_path: Path, headers: dict[str, str]) -> None:
    client = build_client(tmp_path)
    task_id = create_task(client, ALICE)

    response = client.post(
        f"/api/agent/tasks/{task_id}/clarify", headers=headers, json={"table": "orders"}
    )
    assert response.status_code == 404


@pytest.mark.parametrize("headers", [BOB_SAME_ORG, MALLORY_OTHER_ORG])
def test_other_callers_cannot_cancel_a_task(tmp_path: Path, headers: dict[str, str]) -> None:
    client = build_client(tmp_path)
    task_id = create_task(client, ALICE)

    assert client.post(f"/api/agent/tasks/{task_id}/cancel", headers=headers).status_code == 404
    # 关键点：被拒绝的取消不能改变原任务的状态。
    assert client.get(f"/api/agent/tasks/{task_id}", headers=ALICE).json()["status"] != "CANCELLED"


def test_unauthorized_and_missing_are_indistinguishable(tmp_path: Path) -> None:
    """无权与不存在必须返回同一个结果，否则可以用状态码差异探测资源是否存在。"""
    client = build_client(tmp_path)
    task_id = create_task(client, ALICE)

    hidden = client.get(f"/api/agent/tasks/{task_id}", headers=MALLORY_OTHER_ORG)
    missing = client.get("/api/agent/tasks/task_does_not_exist", headers=MALLORY_OTHER_ORG)

    assert hidden.status_code == missing.status_code == 404
    assert hidden.json() == missing.json()


def test_legacy_record_without_owner_fails_closed(tmp_path: Path) -> None:
    """旧记录缺少组织或创建者信息时失败关闭，而不是对所有人开放。"""
    store_path = tmp_path / "agent-tasks.json"
    store_path.write_text(
        json.dumps(
            {
                "tasks": [
                    {
                        "task_id": "task_legacy",
                        "requirement": REQUIREMENT,
                        "status": "DRAFT_READY",
                        "slots": {},
                        "events": [],
                    }
                ]
            },
            ensure_ascii=False,
        ),
        encoding="utf-8",
    )
    client = build_client(tmp_path, store_path=store_path)

    assert client.get("/api/agent/tasks/task_legacy", headers=ALICE).status_code == 404
    assert client.get("/api/agent/tasks", headers=ALICE).json() == []
    assert client.post("/api/agent/tasks/task_legacy/cancel", headers=ALICE).status_code == 404


# ---------------------------------------------------------------------------
# 检索层租户隔离
# ---------------------------------------------------------------------------


def _chunk(evidence_id: str, content: str, *, organization_id: str = "") -> Chunk:
    return Chunk(
        evidence_id=evidence_id,
        doc_id=f"norms/{evidence_id}",
        title="索引变更规范",
        section="并发建索引",
        content=content,
        source=f"norms/{evidence_id}.md",
        organization_id=organization_id,
    )


def test_public_corpus_is_visible_to_every_organization() -> None:
    retriever = KeywordRetriever()
    retriever.add([_chunk("public#1", "并发建索引 的公开规范原文")])

    assert retriever.search("并发建索引", organizations=("org_a",))
    assert retriever.search("并发建索引", organizations=("org_b",))


def test_tenant_corpus_requires_explicit_organization() -> None:
    retriever = KeywordRetriever()
    retriever.add([_chunk("private#1", "并发建索引 的租户私有规范原文", organization_id="org_a")])

    assert retriever.search("并发建索引", organizations=("org_a",))
    assert retriever.search("并发建索引", organizations=("org_b",)) == []
    # 默认（不传组织）只能看到公开语料——失败关闭，而不是"不过滤、全都能看"。
    assert retriever.search("并发建索引") == []


def test_vector_rerank_cannot_surface_unauthorized_chunks() -> None:
    """向量只在已授权的关键词候选上重排，因此不存在"向量把无权片段召回来"的路径。"""

    class AlwaysHighest:
        def score(self, _query: str, _chunk: Chunk) -> float:
            return 1.0

    retriever = HybridRetriever(vector=AlwaysHighest())
    retriever.add(
        [
            _chunk("private#1", "并发建索引 租户私有规范", organization_id="org_a"),
            _chunk("public#1", "并发建索引 公开规范"),
        ]
    )

    hits = retriever.search("并发建索引", organizations=("org_b",))
    assert hits, "公开片段必须仍然可见"
    assert all(item.chunk.organization_id == "" for item in hits), "无权片段不得出现在结果里"


def test_tool_layer_scopes_search_to_the_caller_organization(tmp_path: Path) -> None:
    """工具层的租户范围来自 TrustedContext，工具参数里写不出来。"""
    settings = Settings(agent_demo_dir=str(DEMO_DIR), task_store_path=str(tmp_path / "agent-tasks.json"))
    retriever = HybridRetriever()
    retriever.add(
        [
            _chunk("public#1", "并发建索引 公开规范"),
            _chunk("private#1", "并发建索引 租户私有规范", organization_id="org_a"),
        ]
    )
    registry = Toolbox(settings=settings, retriever=retriever, schema_snapshot="").build()

    owner = run(registry.call("search_norms", {"query": "并发建索引"}, TrustedContext("u1", "org_a")))
    assert owner.ok
    assert "private#1" in owner.evidence_ids

    outsider = run(registry.call("search_norms", {"query": "并发建索引"}, TrustedContext("u2", "org_b")))
    assert outsider.ok
    assert "public#1" in outsider.evidence_ids
    assert "private#1" not in outsider.evidence_ids, "不同组织不得检索到他人私有语料"


def test_demo_corpus_stays_public_after_scope_change() -> None:
    """演示语料是公开合成数据，加了租户过滤后仍必须可检索。

    否则"给语料加隔离"就会变成"演示环境检索不到任何规范"，把修复做成了回归。
    """
    chunks = KeywordRetriever()
    from app.retrieval.corpus import load_corpus

    loaded = load_corpus(DEMO_DIR)
    chunks.add(loaded)
    assert all(item.organization_id == "" for item in loaded), "公开语料不应带组织标记"
    assert chunks.search("热表 并发建索引", prefixes=("norms/",)), "公开语料必须仍可命中"
