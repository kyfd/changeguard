"""加载合成语料并构建检索器。

语料范围严格限定为：变更规范、回滚手册、合成历史案例。
**不爬取任意网站，也不解析复杂 PDF**——首版用 Markdown 把质量做出来。
"""

from __future__ import annotations

import re
from pathlib import Path

from app.retrieval.base import Chunk, chunk_markdown
from app.retrieval.keyword import HybridRetriever, KeywordRetriever
from app.retrieval.base import VectorScorer

_H1 = re.compile(r"^#\s+(.*)$", re.MULTILINE)


def load_corpus(demo_dir: Path) -> list[Chunk]:
    """把演示目录下的 Markdown 文档切分为片段。

    跳过说明性 README，避免它污染规范检索结果。
    """
    chunks: list[Chunk] = []
    if not demo_dir.exists():
        return chunks

    for path in sorted(demo_dir.rglob("*.md")):
        relative = path.relative_to(demo_dir).as_posix()
        if relative.startswith("README") or relative.endswith("/README.md"):
            continue
        text = path.read_text(encoding="utf-8")
        title_match = _H1.search(text)
        title = title_match.group(1).strip() if title_match else path.stem
        doc_id = relative[: -len(".md")]
        chunks.extend(
            chunk_markdown(
                text=text,
                doc_id=doc_id,
                title=title,
                source=relative,
            )
        )
    return chunks


def build_retriever(demo_dir: Path, vector: VectorScorer | None = None) -> HybridRetriever:
    """构建检索器。

    未注入向量打分器时行为等价于关键词基线——这正是首版需要的对照基线。
    """
    retriever = HybridRetriever(vector=vector)
    retriever.add(load_corpus(demo_dir))
    return retriever


def keyword_retriever(demo_dir: Path) -> KeywordRetriever:
    """纯关键词检索器，用于评估报告中的基线对照。"""
    retriever = KeywordRetriever()
    retriever.add(load_corpus(demo_dir))
    return retriever
