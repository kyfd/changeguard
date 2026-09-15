"""关键词检索基线与混合检索。

为什么先做关键词：SQL 标识符、错误码、规则名这类内容**不能只靠语义相似度**。
先用关键词把基线做出来，再评估向量检索是否真的带来提升——
这是评估报告里"三组对照"的其中两组。
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass, field

from app.retrieval.base import Chunk, RetrievalReport, ScoredChunk, VectorScorer

_ASCII_TOKEN = re.compile(r"[a-z0-9_]{2,}")
_CJK = re.compile(r"[\u4e00-\u9fff]")


def tokenize(text: str) -> list[str]:
    """把文本切成可比较的 token。

    英文/数字按词切；中文没有分词器，退化为**字符二元组**，
    对"并发建索引"这类短语有足够区分度。
    """
    lowered = (text or "").lower()
    tokens = _ASCII_TOKEN.findall(lowered)
    chinese = [char for char in lowered if _CJK.match(char)]
    tokens.extend(f"{chinese[index]}{chinese[index + 1]}" for index in range(len(chinese) - 1))
    return tokens


@dataclass
class KeywordRetriever:
    """TF-IDF 风格的关键词检索。"""

    name: str = "keyword"
    _chunks: list[Chunk] = field(default_factory=list)
    _index: list[dict[str, float]] = field(default_factory=list)
    _document_frequency: dict[str, int] = field(default_factory=dict)

    def add(self, chunks: list[Chunk]) -> None:
        for chunk in chunks:
            self._chunks.append(chunk)
        self._reindex()

    def size(self) -> int:
        """已索引的片段数量，用于健康检查展示语料规模。"""
        return len(self._chunks)

    def _reindex(self) -> None:
        self._index = []
        self._document_frequency = {}
        for chunk in self._chunks:
            weighted = tokenize(f"{chunk.title} {chunk.section} {chunk.content}")
            # 标题与章节命中更重要，重复计入
            weighted.extend(tokenize(chunk.title))
            weighted.extend(tokenize(chunk.section))
            counts: dict[str, float] = {}
            for token in weighted:
                counts[token] = counts.get(token, 0.0) + 1.0
            self._index.append(counts)
            for token in counts:
                self._document_frequency[token] = self._document_frequency.get(token, 0) + 1

    def search(self, query: str, limit: int = 5, prefixes: tuple[str, ...] | None = None) -> list[ScoredChunk]:
        """检索。

        `prefixes` 是**作用域**：只在该范围内打分。
        作用域必须在排序之前生效——先全局排序再过滤，会让规范被案例挤出结果。
        """
        query_tokens = tokenize(query)
        if not query_tokens or not self._chunks:
            return []
        total = len(self._chunks)
        unique_query = set(query_tokens)
        scored: list[ScoredChunk] = []
        for position, counts in enumerate(self._index):
            if prefixes and not self._chunks[position].doc_id.startswith(prefixes):
                continue
            score = 0.0
            for token in unique_query:
                frequency = counts.get(token)
                if not frequency:
                    continue
                inverse = math.log((total + 1) / (self._document_frequency.get(token, 0) + 1)) + 1.0
                score += (1.0 + math.log(frequency)) * inverse
            if score > 0:
                scored.append(ScoredChunk(chunk=self._chunks[position], score=round(score, 6)))
        # 分数相同时按 evidence_id 排序，保证结果可复现
        scored.sort(key=lambda item: (-item.score, item.chunk.evidence_id))
        return scored[:limit]

    def report(self, query: str, returned: int, top_score: float) -> RetrievalReport:
        return RetrievalReport(retriever=self.name, query=query, returned=returned, top_score=top_score)


@dataclass
class HybridRetriever:
    """关键词 + 可选向量打分的混合检索。

    向量打分通过 `VectorScorer` 注入。未注入时行为等价于关键词基线，
    因此可以安全地作为默认实现使用。
    """

    vector: VectorScorer | None = None
    weight: float = 0.5
    name: str = "hybrid"
    _keyword: KeywordRetriever = field(default_factory=KeywordRetriever)

    def add(self, chunks: list[Chunk]) -> None:
        self._keyword.add(chunks)

    def size(self) -> int:
        return self._keyword.size()

    def search(self, query: str, limit: int = 5, prefixes: tuple[str, ...] | None = None) -> list[ScoredChunk]:
        base = self._keyword.search(query, limit=max(limit * 3, 10), prefixes=prefixes)
        if self.vector is None or not base:
            return base[:limit]

        rescored: list[ScoredChunk] = []
        top = base[0].score or 1.0
        for item in base:
            normalized = item.score / top
            blended = normalized * (1.0 - self.weight) + self.vector.score(query, item.chunk) * self.weight
            rescored.append(ScoredChunk(chunk=item.chunk, score=round(blended, 6)))
        rescored.sort(key=lambda item: (-item.score, item.chunk.evidence_id))
        return rescored[:limit]
