"""检索层测试：切分元数据、关键词命中、无命中不编造。"""

from __future__ import annotations

from app.retrieval.corpus import build_retriever, load_corpus
from tests.conftest import DEMO_DIR


def test_corpus_is_chunked_with_metadata() -> None:
    chunks = load_corpus(DEMO_DIR)
    assert len(chunks) > 10, "合成语料应产生足够多的片段"

    norms = [item for item in chunks if item.doc_id.startswith("norms/")]
    cases = [item for item in chunks if item.doc_id.startswith("cases/")]
    assert norms and cases
    assert all(item.evidence_id and item.section and item.source for item in chunks)
    assert any(item.version for item in norms), "应解析出文档版本元数据"


def test_scoped_search_returns_norms() -> None:
    retriever = build_retriever(DEMO_DIR)
    hits = retriever.search("热表 并发建索引 lock_timeout", limit=5, prefixes=("norms/",))

    assert hits, "规范检索必须能命中规范文档"
    assert all(item.chunk.doc_id.startswith("norms/") for item in hits)
    assert hits[0].score > 0


def test_scope_is_applied_before_ranking() -> None:
    """作用域必须在排序之前生效。

    否则"先全局排序、再按前缀过滤"会让规范被更短、关键词更密集的案例挤出结果，
    规范检索就会误报"没有依据"。
    """
    retriever = build_retriever(DEMO_DIR)
    query = "热表 并发建索引 lock_timeout"

    scoped = retriever.search(query, limit=1, prefixes=("norms/",))
    assert scoped, "限定作用域后仍应有结果"
    assert scoped[0].chunk.doc_id.startswith("norms/")

    case_scoped = retriever.search(query, limit=2, prefixes=("cases/",))
    assert case_scoped
    assert all(item.chunk.doc_id.startswith("cases/") for item in case_scoped)


def test_search_results_are_deterministic() -> None:
    retriever = build_retriever(DEMO_DIR)
    first = [item.chunk.evidence_id for item in retriever.search("回滚 索引 并发", limit=5)]
    second = [item.chunk.evidence_id for item in retriever.search("回滚 索引 并发", limit=5)]
    assert first == second, "相同输入必须得到相同结果"


def test_unrelated_query_returns_nothing() -> None:
    retriever = build_retriever(DEMO_DIR)
    hits = retriever.search("zzzz 完全不相关的主题 qqqq", limit=5)
    assert hits == []


def test_scope_filter_separates_norms_from_cases() -> None:
    retriever = build_retriever(DEMO_DIR)
    hits = retriever.search("索引 事故 命令", limit=10)
    case_hits = [item for item in hits if item.chunk.doc_id.startswith("cases/")]
    norm_hits = [item for item in hits if item.chunk.doc_id.startswith("norms/")]
    assert case_hits, "事故案例应可被检索到"
    assert norm_hits, "规范应可被检索到"
