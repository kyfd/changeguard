"""项目知识仓储与检索权限过滤。

三条边界：

1. **服务端决定可见范围**。一份知识归属某个组织，可选归属某个应用；
   可见性由服务端按"组织 + 应用 + 是否生效"判定，绝不从文档正文推断。
2. **文档内容是数据，不是指令**。导入的正文会被切分、检索、引用，但不会被当作提示词
   执行，也不会因此获得任何工具或身份权限。导入时只做**提示注入筛查并如实记录**，
   既不因此放行，也不因此拒绝一份可能正当的安全文档。
3. **失败关闭**。没有组织标记的（例如公开合成语料）视为公开；带组织标记的必须显式匹配；
   已废弃（deprecated）的知识不进入检索，也不会被当成有效证据。
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from contextlib import suppress
from datetime import datetime, timezone
from pathlib import Path
from threading import Lock
from typing import Any

from app.retrieval.base import Chunk, chunk_markdown
from app.schemas.knowledge import KIND_PREFIX


class KnowledgeRepositoryCorrupted(Exception):
    """知识文件存在但无法安全加载（失败关闭）。"""


def content_hash(body: str) -> str:
    return hashlib.sha256((body or "").encode("utf-8")).hexdigest()


class KnowledgeRepository:
    """知识仓储：单实例、原子落盘，与任务仓储同构（先落盘、成功后再提交内存）。"""

    def __init__(self, path: str) -> None:
        if not str(path or "").strip():
            raise ValueError("知识仓储需要非空的存储路径；不接受静默不落盘")
        self._path = Path(path)
        self._lock = Lock()
        self._records: dict[str, dict[str, Any]] = {}
        self._load()

    def _load(self) -> None:
        if not self._path.exists():
            return
        try:
            raw = self._path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError) as error:
            raise KnowledgeRepositoryCorrupted(f"知识文件不可读：{self._path}（{error}）") from error
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError as error:
            raise KnowledgeRepositoryCorrupted(f"知识文件不是合法 JSON：{self._path}") from error
        if not isinstance(payload, dict):
            raise KnowledgeRepositoryCorrupted(f"知识文件顶层必须是对象：{self._path}")
        records = payload.get("knowledge", [])
        if not isinstance(records, list):
            raise KnowledgeRepositoryCorrupted(f"知识文件的 knowledge 字段必须是数组：{self._path}")
        for record in records:
            if not isinstance(record, dict):
                raise KnowledgeRepositoryCorrupted(f"知识文件包含非对象记录：{self._path}")
            knowledge_id = record.get("knowledge_id")
            if knowledge_id:
                self._records[knowledge_id] = record

    def _persist_records(self, records: dict[str, dict[str, Any]]) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        payload = json.dumps({"knowledge": list(records.values())}, ensure_ascii=False, indent=2)
        handle = tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", delete=False, dir=str(self._path.parent), suffix=".tmp"
        )
        temporary_path = handle.name
        try:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
            handle.close()
            os.replace(temporary_path, self._path)
        except BaseException:
            if not handle.closed:
                handle.close()
            with suppress(OSError):
                os.unlink(temporary_path)
            raise

    def save(self, record: dict[str, Any]) -> None:
        knowledge_id = record.get("knowledge_id")
        if not knowledge_id:
            raise ValueError("知识记录必须带 knowledge_id")
        with self._lock:
            candidate = dict(self._records)
            candidate[knowledge_id] = json.loads(json.dumps(record))
            self._persist_records(candidate)
            self._records = candidate

    def get(self, knowledge_id: str) -> dict[str, Any] | None:
        with self._lock:
            record = self._records.get(knowledge_id)
            return json.loads(json.dumps(record)) if record is not None else None

    def list(self) -> list[dict[str, Any]]:
        with self._lock:
            return [json.loads(json.dumps(item)) for item in self._records.values()]


def _chunk_status(record: dict[str, Any]) -> str:
    """记录里的状态是权威值，覆盖从正文文本推断出的状态。"""
    return "deprecated" if record.get("status") == "deprecated" else "active"


def chunks_for(record: dict[str, Any]) -> list[Chunk]:
    """把一份知识切成可检索片段，并强制带上组织与生效状态。"""
    kind = str(record.get("kind") or "norms")
    prefix = KIND_PREFIX.get(kind, "norms/")
    knowledge_id = str(record.get("knowledge_id") or "")
    body = str(record.get("body") or "")
    chunks = chunk_markdown(
        text=body,
        doc_id=f"{prefix}{knowledge_id}",
        title=str(record.get("title") or knowledge_id),
        source=str(record.get("source") or f"knowledge:{kind}"),
        organization_id=str(record.get("organization_id") or ""),
    )
    status = _chunk_status(record)
    return [
        Chunk(
            evidence_id=chunk.evidence_id,
            doc_id=chunk.doc_id,
            title=chunk.title,
            section=chunk.section,
            content=chunk.content,
            source=chunk.source,
            version=str(record.get("version") or chunk.version or ""),
            status=status,
            organization_id=chunk.organization_id,
            applicability=chunk.applicability,
        )
        for chunk in chunks
    ]


def is_visible(record: dict[str, Any], organization_id: str, application_id: str) -> bool:
    """一份知识对某个 (组织, 应用) 是否可见。

    失败关闭：组织必须精确匹配（空组织标记的导入知识不存在）；应用限定时必须与目标应用一致，
    目标应用为空时只能看到组织通用知识；已废弃的知识不可见。
    """
    if not organization_id:
        return False
    if str(record.get("organization_id") or "") != organization_id:
        return False
    if str(record.get("status") or "active") != "active":
        return False
    scoped_application = str(record.get("application_id") or "")
    if scoped_application and scoped_application != application_id:
        return False
    return True


def visible_chunks(
    records: list[dict[str, Any]], organization_id: str, application_id: str = ""
) -> list[Chunk]:
    """按可见性过滤后展开成片段。"""
    chunks: list[Chunk] = []
    for record in records:
        if is_visible(record, organization_id, application_id):
            chunks.extend(chunks_for(record))
    return chunks


def import_record(
    *,
    knowledge_id: str,
    organization_id: str,
    imported_by: str,
    kind: str,
    title: str,
    body: str,
    version: str = "",
    source: str = "",
    application_id: str = "",
    status: str = "active",
    injection_hits: list[str] | None = None,
) -> dict[str, Any]:
    return {
        "knowledge_id": knowledge_id,
        "organization_id": organization_id,
        "application_id": application_id,
        "kind": kind,
        "title": title,
        "version": version,
        "source": source,
        "status": status,
        "body": body,
        "content_hash": content_hash(body),
        "imported_by": imported_by,
        "imported_at": datetime.now(timezone.utc).isoformat(),
        "deprecated_at": None,
        "injection_hits": list(injection_hits or []),
    }
