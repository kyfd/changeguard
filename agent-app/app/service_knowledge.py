"""AgentService 项目知识库与结构快照 Mixin。"""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any

from app.guard import detect_injection
from app.knowledge import chunks_for, import_record, visible_chunks
from app.retrieval.keyword import HybridRetriever
from app.schemas.drafts import ClarifyRequest
from app.schemas.knowledge import (
    KIND_PREFIX,
    KnowledgeDetail,
    KnowledgeImportRequest,
    KnowledgeSearchHit,
    KnowledgeSnippet,
    KnowledgeView,
)
from app.service_common import (
    KnowledgeForbidden,
    KnowledgeInvalid,
    KnowledgeNotFound,
    KnowledgeStateUnavailable,
    TaskInputInvalid,
)
from app.tools.registry import TrustedContext
from app.workflow.state import event


class KnowledgeServiceMixin:
    """项目知识导入、检索与结构快照选用。"""

    async def _select_snapshot(
        self, knowledge_id: str, context: TrustedContext, task_application_id: str
    ) -> tuple[dict[str, Any], str]:
        """校验并解析一份可供任务选用的知识库结构快照，返回 (元数据, 正文)。

        校验（失败关闭，逐项给出可执行提示）：
        - 存在且属于调用方组织（不存在与跨组织同样按"不存在"处理，不提供探测）；
        - kind 必须是 schema、status 必须是生效中；
        - 快照绑定应用时，任务必须绑定同一应用（先选应用再选快照）；
        - 快照的应用授权在任务绑定环节已通过治理服务核对（同一 ID，不重复请求）。
        正文按不可信数据处理：只进入生成输入，不改变工具权限或治理规则。
        """
        record = self._require_knowledge(knowledge_id, context)
        if str(record.get("kind") or "") != "schema":
            raise TaskInputInvalid("所选知识不是结构快照（kind=schema），请从结构快照中选择")
        if str(record.get("status") or "") != "active":
            raise TaskInputInvalid("所选结构快照已失效，请选择生效中的快照或改用手填快照")
        knowledge_application = str(record.get("application_id") or "")
        task_application = (task_application_id or "").strip()
        if knowledge_application and knowledge_application != task_application:
            if not task_application:
                raise TaskInputInvalid("该结构快照属于特定应用：请先为任务选择应用，再选用快照")
            raise TaskInputInvalid("所选结构快照属于其他应用，请选择当前应用下的快照")
        return (
            {
                "knowledge_id": str(record.get("knowledge_id") or ""),
                "title": str(record.get("title") or ""),
                "version": str(record.get("version") or ""),
                "content_hash": str(record.get("content_hash") or ""),
                "application_id": knowledge_application,
                "selected_at": datetime.now(timezone.utc).isoformat(),
                "selected_by": context.user_id,
            },
            str(record.get("body") or ""),
        )

    def _validate_selected_snapshot(self, record: dict[str, Any]) -> None:
        """再次执行前复核已选用的知识快照仍然有效（存在、生效、内容未变）。

        快照失效不是"尽力继续"的理由：生成输入将包含这份结构，失效快照会让生成
        建立在过期事实上。失败时给出可执行的下一步（重新选择或改手填）。
        """
        meta = record.get("selected_snapshot")
        if not meta:
            return
        knowledge_id = str(meta.get("knowledge_id") or "")
        body = self._knowledge.get(knowledge_id)
        if (
            body is None
            or str(body.get("status") or "") != "active"
            or str(body.get("content_hash") or "") != str(meta.get("content_hash") or "")
        ):
            raise TaskInputInvalid(
                f"所选知识库结构快照已失效或内容已更新（knowledge_id={knowledge_id}）。"
                "请重新选择生效中的快照，或改用手填快照后再继续"
            )

    async def _apply_clarify_snapshot(
        self,
        record: dict[str, Any],
        request: ClarifyRequest,
        context: TrustedContext,
        application_id: str,
    ) -> None:
        """补充/恢复阶段的快照生命周期：切换、清除或替换知识快照与手填快照。

        冲突一律显式拒绝，绝不静默覆盖或拼接：
        - `snapshot_knowledge_id` 非空：切换为知识快照。已有手填快照时必须同时把
          schema_snapshot 显式置空，表示"确认替换"；
        - `snapshot_knowledge_id` 为空串：清除选用；正文来自知识快照时一并清空；
        - `schema_snapshot` 非空：覆盖手填正文；当前正文来自知识快照时先清除选用；
        - `schema_snapshot` 空串：清空手填正文。
        """
        snapshot_knowledge_id = (request.snapshot_knowledge_id or "").strip()
        schema_snapshot = request.schema_snapshot
        current_kind = str(record.get("snapshot_source_kind") or "")
        events = list(record.get("events") or [])

        if snapshot_knowledge_id:
            if schema_snapshot is not None and schema_snapshot.strip():
                raise TaskInputInvalid(
                    "结构快照只能二选一：不能同时提供 snapshot_knowledge_id 和 schema_snapshot"
                )
            if (
                current_kind == "manual"
                and str(record.get("schema_snapshot") or "").strip()
                and schema_snapshot is None
            ):
                raise TaskInputInvalid(
                    "当前已有一份手填快照：如确认改用知识库快照，请在提交 snapshot_knowledge_id 的同时"
                    "把 schema_snapshot 显式置空（表示确认替换）；或继续使用手填快照"
                )
            meta, body = await self._select_snapshot(snapshot_knowledge_id, context, application_id)
            record["selected_snapshot"] = meta
            record["snapshot_source_kind"] = "knowledge"
            record["schema_snapshot"] = body
            events.append(
                event(
                    "snapshot_selected",
                    f"选用知识库结构快照 {meta.get('title') or ''}"
                    f"（版本 {meta.get('version') or ''}，knowledge_id={meta.get('knowledge_id')}）",
                )
            )
            record["events"] = events
            return

        if request.snapshot_knowledge_id is not None:
            # 空串：显式清除选用；不能再同时提供手填正文（分两步，避免歧义）。
            if schema_snapshot is not None and schema_snapshot.strip():
                raise TaskInputInvalid(
                    "已请求清除知识快照选用，不能同时提供手填 schema_snapshot；请分两步提交"
                )
            if current_kind == "knowledge":
                record["schema_snapshot"] = ""
                record["snapshot_source_kind"] = ""
            record["selected_snapshot"] = None
            events.append(event("snapshot_cleared", "已清除知识库快照选用"))
            record["events"] = events
            return

        if schema_snapshot is None:
            return
        if current_kind == "knowledge":
            raise TaskInputInvalid(
                "当前正文来自知识库快照：请先清除选用（snapshot_knowledge_id 提交空串），再提交手填快照"
            )
        record["schema_snapshot"] = schema_snapshot
        record["snapshot_source_kind"] = "manual" if schema_snapshot.strip() else ""
        if not schema_snapshot.strip():
            record["selected_snapshot"] = None
        record["events"] = events


    async def import_knowledge(self, request: KnowledgeImportRequest, context: TrustedContext) -> KnowledgeView:
        """导入一份项目知识（规范 / 历史案例 / 结构快照）。

        组织来自可信上下文；文档内容按**不可信数据**处理——只做提示注入筛查并如实
        记录，既不据此提升权限，也不据此放行或拒绝一份可能正当的安全文档。
        """
        if not context.organization_id or not context.user_id:
            raise KnowledgeInvalid("缺少可信身份，拒绝导入")
        title = request.title.strip()
        body = request.body
        if not title:
            raise KnowledgeInvalid("标题不能为空")
        if not body.strip():
            raise KnowledgeInvalid("正文不能为空")
        # 应用 ID 由调用方声明，必须由治理后端核对授权；不能把声明当权限。
        application_id = request.application_id.strip()
        if application_id and not await self._application_authorized(context, application_id):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对），不能导入应用专属知识")
        hits = sorted(set(detect_injection(f"{title}\n{body}")))
        record = import_record(
            knowledge_id=f"kb_{uuid.uuid4().hex[:12]}",
            organization_id=context.organization_id,
            imported_by=context.user_id,
            kind=request.kind,
            title=title,
            body=body,
            version=request.version.strip(),
            source=request.source.strip(),
            application_id=application_id,
            status=request.status,
            injection_hits=hits,
        )
        try:
            self._knowledge.save(record)
        except OSError as error:
            raise KnowledgeStateUnavailable("知识未落盘，请重试") from error
        return self._knowledge_view(record)

    async def list_knowledge(
        self,
        context: TrustedContext,
        *,
        kind: str | None = None,
        status: str | None = None,
        application_id: str | None = None,
    ) -> list[KnowledgeView]:
        """列出**本组织**导入的知识。其他组织的数据完全不出现。

        指定 `application_id` 时必须先由治理后端核对授权：筛选参数只是请求条件，
        不是权限依据。未授权（或无法核对）直接拒绝，而不是返回该应用的知识。
        """
        if not context.organization_id:
            return []
        if application_id and not await self._application_authorized(context, application_id):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对）")
        result: list[KnowledgeView] = []
        for record in self._knowledge.list():
            if str(record.get("organization_id") or "") != context.organization_id:
                continue
            record_application = str(record.get("application_id") or "")
            if application_id:
                # 已核对过该应用授权：返回组织通用 + 该应用的知识。
                if record_application not in ("", application_id):
                    continue
            elif record_application:
                # 未指定应用时不返回应用专属知识——无法逐条核对授权。失败关闭。
                continue
            if kind and str(record.get("kind") or "") != kind:
                continue
            if status and str(record.get("status") or "active") != status:
                continue
            result.append(self._knowledge_view(record))
        result.sort(key=lambda item: item.knowledge_id, reverse=True)
        return result

    async def get_knowledge(self, knowledge_id: str, context: TrustedContext) -> KnowledgeDetail:
        record = self._require_knowledge(knowledge_id, context)
        await self._ensure_knowledge_application(record, context)
        return self._knowledge_detail(record)

    async def deprecate_knowledge(self, knowledge_id: str, context: TrustedContext) -> KnowledgeView:
        """把一份知识标记为失效（保留记录，不物理删除）。失效后不再进入检索。"""
        record = self._require_knowledge(knowledge_id, context)
        await self._ensure_knowledge_application(record, context)
        if str(record.get("status") or "active") == "deprecated":
            return self._knowledge_view(record)
        record["status"] = "deprecated"
        record["deprecated_at"] = datetime.now(timezone.utc).isoformat()
        try:
            self._knowledge.save(record)
        except OSError as error:
            raise KnowledgeStateUnavailable("知识状态未落盘，请重试") from error
        return self._knowledge_view(record)

    async def search_knowledge(
        self,
        context: TrustedContext,
        query: str,
        *,
        kind: str | None = None,
        application_id: str = "",
        limit: int = 8,
    ) -> list[KnowledgeSearchHit]:
        """在服务端权限过滤之后检索项目知识。

        只会在"本组织 + 应用匹配 + 生效中"的片段里排序，因此跨组织、未授权应用或已失效的
        知识不可能出现在结果里。
        """
        organization_id = context.organization_id
        if not organization_id or not (query or "").strip():
            return []
        # 请求参数不是权限依据：应用范围必须先由治理后端核对授权。
        if application_id and not await self._application_authorized(context, application_id):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对）")
        chunks = visible_chunks(self._knowledge.list(), organization_id, application_id)
        if not chunks:
            return []
        retriever = HybridRetriever()
        retriever.add(chunks)
        prefixes = (KIND_PREFIX[kind],) if kind in KIND_PREFIX else None
        selected = retriever.search(
            query, limit=max(1, min(int(limit), 20)), prefixes=prefixes, organizations=(organization_id,)
        )
        hits: list[KnowledgeSearchHit] = []
        for item in selected:
            knowledge_id = item.chunk.doc_id.split("/", 1)[-1]
            hits.append(
                KnowledgeSearchHit(
                    knowledge_id=knowledge_id,
                    doc_id=item.chunk.doc_id,
                    title=item.chunk.title,
                    section=item.chunk.section,
                    version=item.chunk.version,
                    status=item.chunk.status,
                    snippet=item.chunk.as_snippet(),
                    score=item.score,
                )
            )
        return hits

    def _require_knowledge(self, knowledge_id: str, context: TrustedContext) -> dict[str, Any]:
        if not context.organization_id or not context.user_id:
            raise KnowledgeNotFound(knowledge_id)
        record = self._knowledge.get(knowledge_id)
        # 不存在与不属于本组织同样返回 404：不提供跨组织存在性探测。
        if record is None or str(record.get("organization_id") or "") != context.organization_id:
            raise KnowledgeNotFound(knowledge_id)
        return record

    async def _ensure_knowledge_application(self, record: dict[str, Any], context: TrustedContext) -> None:
        """应用专属知识：必须由治理后端确认调用方对该应用有授权。"""
        application = str(record.get("application_id") or "")
        if application and not await self._application_authorized(context, application):
            raise KnowledgeForbidden("缺少该应用的授权（或无法核对），不能访问应用专属知识")

    @staticmethod
    def _knowledge_view(record: dict[str, Any]) -> KnowledgeView:
        snippets = chunks_for(record)
        return KnowledgeView(
            knowledge_id=str(record.get("knowledge_id") or ""),
            organization_id=str(record.get("organization_id") or ""),
            application_id=str(record.get("application_id") or ""),
            kind=str(record.get("kind") or "norms"),
            title=str(record.get("title") or ""),
            version=str(record.get("version") or ""),
            source=str(record.get("source") or ""),
            status=str(record.get("status") or "active"),
            content_hash=str(record.get("content_hash") or ""),
            imported_by=str(record.get("imported_by") or ""),
            imported_at=record.get("imported_at"),
            deprecated_at=record.get("deprecated_at"),
            injection_hits=[str(item) for item in (record.get("injection_hits") or [])],
            snippet_count=len(snippets),
        )

    def _knowledge_detail(self, record: dict[str, Any]) -> KnowledgeDetail:
        view = self._knowledge_view(record)
        snippets = chunks_for(record)
        return KnowledgeDetail(
            **view.model_dump(),
            snippets=[
                KnowledgeSnippet(
                    evidence_id=chunk.evidence_id,
                    doc_id=chunk.doc_id,
                    section=chunk.section,
                    version=chunk.version,
                    status=chunk.status,
                    snippet=chunk.as_snippet(),
                )
                for chunk in snippets
            ],
        )
