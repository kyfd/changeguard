"""检索基础类型与文档切分。

首版只做**关键词基线**（见 `keyword.py`）。向量检索通过 `VectorScorer`
接口预留，用于后续对照实验，不在首版默认开启。

切分策略：先按标题切，再把过长的小节按段落二次切分，并保留
"文档 ID / 版本 / 标题 / 章节路径 / 生效状态"元数据。

**组织标记只来自显式的 `organization_id` 入参**（由语料加载方声明该目录是公开合成语料
还是某个租户的私有资料），不从文档正文推断。文档里的"适用范围"讲的是适用场景，
不是租户身份——把两者混为一谈，会让一份写着"适用范围：某客户"的文档被误判成租户资料，
或者反过来把租户资料当成公开内容。
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Protocol

MAX_CHUNK_CHARS = 700
MIN_CHUNK_CHARS = 80

_HEADING = re.compile(r"^(#{1,6})\s+(.*)$")
_VERSION = re.compile(r"文档版本[：:]\s*([^\s　]+)")
# **适用范围**（适用数据库/环境/场景）。这与 P0 修掉的 `_SCOPE` 用途不同：
# 那个是把正文里的"适用范围"误当成**租户身份**；这里把它当作**适用性元数据**，
# 用来判断一条规范是否适用于当前目标（数据库/环境），不参与任何组织隔离。
_APPLICABILITY = re.compile(r"适用范围[：:]\s*([^\n　]+)")


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
    # 适用性原文（例如"PostgreSQL 生产库"）。空串表示**文档没写**，即无法核对适用性。
    applicability: str = ""

    def as_snippet(self, limit: int = 160) -> str:
        text = " ".join(self.content.split())
        return text if len(text) <= limit else text[:limit] + "…"


@dataclass
class ScoredChunk:
    chunk: Chunk
    score: float


def organization_visible(chunk_organization: str, organizations: tuple[str, ...]) -> bool:
    """判断一个片段是否落在调用方的可见范围内。

    约定：**没有组织标记的片段视为公开合成语料**，对所有调用方可见；
    带组织标记的片段必须由调用方显式列入 `organizations` 才可见。

    这是一个失败关闭的默认值：不传 `organizations` 时只能看到公开语料，
    而不是"不过滤、全都能看"。私有语料接入前边界就已经存在，不会事后补漏。
    """
    if not chunk_organization:
        return True
    return chunk_organization in organizations


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
    version = version_match.group(1) if version_match else ""
    applicability_match = _APPLICABILITY.search(text[:600])
    applicability = applicability_match.group(1).strip() if applicability_match else ""
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
                    applicability=previous.applicability,
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
                    organization_id=organization_id,
                    applicability=applicability,
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
