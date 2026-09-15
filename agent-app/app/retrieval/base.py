"""检索基础类型与文档切分。

首版只做**关键词基线**（见 `keyword.py`）。向量检索通过 `VectorScorer`
接口预留，用于后续对照实验，不在首版默认开启。

切分策略：先按标题切，再把过长的小节按段落二次切分，并保留
"文档 ID / 版本 / 标题 / 章节路径 / 生效状态 / 适用范围"元数据。
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Protocol

MAX_CHUNK_CHARS = 700
MIN_CHUNK_CHARS = 80

_HEADING = re.compile(r"^(#{1,6})\s+(.*)$")
_VERSION = re.compile(r"文档版本[：:]\s*([^\s　]+)")
_SCOPE = re.compile(r"适用范围[：:]\s*([^\n]+)")


@dataclass(frozen=True)
class Chunk:
    """一个可检索的文档片段。"""

    evidence_id: str
    doc_id: str
    title: str
    section: str
    content: str
    source: str
    version: str = ""
    status: str = "unknown"  # active / deprecated / unknown
    organization_id: str = ""

    def as_snippet(self, limit: int = 160) -> str:
        text = " ".join(self.content.split())
        return text if len(text) <= limit else text[:limit] + "…"


@dataclass
class ScoredChunk:
    chunk: Chunk
    score: float


class Retriever(Protocol):
    """检索器契约。"""

    name: str

    def add(self, chunks: list[Chunk]) -> None:
        ...

    def search(self, query: str, limit: int = 5) -> list[ScoredChunk]:
        ...


class VectorScorer(Protocol):
    """向量打分接口。首版不实现，用于后续混合检索对照。"""

    def score(self, query: str, chunk: Chunk) -> float:
        ...


@dataclass
class RetrievalReport:
    """一次检索的可观测结果。"""

    retriever: str
    query: str
    returned: int
    top_score: float = 0.0
    used_vector: bool = False
    notes: list[str] = field(default_factory=list)


def chunk_markdown(text: str, doc_id: str, title: str, source: str, organization_id: str = "") -> list[Chunk]:
    """把一份 Markdown 文档切成带章节路径的片段。"""
    version_match = _VERSION.search(text)
    scope_match = _SCOPE.search(text)
    version = version_match.group(1) if version_match else ""
    status = "deprecated" if re.search(r"(已废弃|已失效|deprecated)", text[:400], re.IGNORECASE) else "active"

    sections: list[tuple[str, list[str]]] = []
    heading_stack: list[tuple[int, str]] = []
    current_path = title
    current_lines: list[str] = []

    def flush() -> None:
        if current_lines and any(line.strip() for line in current_lines):
            sections.append((current_path, list(current_lines)))

    for line in text.splitlines():
        match = _HEADING.match(line)
        if match:
            flush()
            current_lines = []
            level = len(match.group(1))
            heading_stack = [(depth, name) for depth, name in heading_stack if depth < level]
            heading_stack.append((level, match.group(2).strip()))
            current_path = " / ".join(name for _, name in heading_stack)
            continue
        current_lines.append(line)
    flush()

    chunks: list[Chunk] = []
    index = 0
    for path, lines in sections:
        body = "\n".join(lines).strip()
        if not body:
            continue
        for piece in _split_paragraphs(body):
            if len(piece) < MIN_CHUNK_CHARS and chunks and doc_id == chunks[-1].doc_id and path == chunks[-1].section:
                # 过短的尾段并入上一个片段，避免产生无意义碎片
                previous = chunks[-1]
                chunks[-1] = Chunk(
                    evidence_id=previous.evidence_id,
                    doc_id=previous.doc_id,
                    title=previous.title,
                    section=previous.section,
                    content=f"{previous.content}\n{piece}",
                    source=previous.source,
                    version=previous.version,
                    status=previous.status,
                    organization_id=previous.organization_id,
                )
                continue
            index += 1
            chunks.append(
                Chunk(
                    evidence_id=f"{doc_id}#{index}",
                    doc_id=doc_id,
                    title=title,
                    section=path,
                    content=piece,
                    source=source,
                    version=version,
                    status=status,
                    organization_id=organization_id or (scope_match.group(1).strip() if scope_match else ""),
                )
            )
    return chunks


def _split_paragraphs(body: str) -> list[str]:
    """按段落聚合，避免片段过长；超长段落按句子边界再切。"""
    paragraphs = [item.strip() for item in re.split(r"\n\s*\n", body) if item.strip()]
    pieces: list[str] = []
    buffer = ""
    for paragraph in paragraphs:
        if len(buffer) + len(paragraph) + 2 <= MAX_CHUNK_CHARS:
            buffer = f"{buffer}\n\n{paragraph}" if buffer else paragraph
            continue
        if buffer:
            pieces.append(buffer)
        if len(paragraph) <= MAX_CHUNK_CHARS:
            buffer = paragraph
        else:
            for start in range(0, len(paragraph), MAX_CHUNK_CHARS):
                pieces.append(paragraph[start : start + MAX_CHUNK_CHARS])
            buffer = ""
    if buffer:
        pieces.append(buffer)
    return pieces
