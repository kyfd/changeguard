"""项目知识的契约。

导入内容一律是**不可信数据**：它们会被检索、会被引用，但不会被当成指令执行，
也不会因为"文档里这么说"而获得任何权限。组织与应用的可见范围由服务端决定，
不从文档正文推断。
"""

from __future__ import annotations

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field

MAX_KNOWLEDGE_BODY_CHARS = 200_000

# 三类可导入的项目知识。检索作用域与之一一对应：
#   norms  → norms/ 前缀（规范与回滚手册）
#   cases  → cases/ 前缀（历史案例）
#   schema → schema/ 前缀（结构快照，供人查阅与后续工具使用）
KnowledgeKind = Literal["norms", "cases", "schema"]
KnowledgeStatus = Literal["active", "deprecated"]

KIND_PREFIX: dict[str, str] = {"norms": "norms/", "cases": "cases/", "schema": "schema/"}


class KnowledgeImportRequest(BaseModel):
    """导入一份项目知识。"""

    kind: KnowledgeKind
    title: str = Field(min_length=1, max_length=200)
    body: str = Field(min_length=1, max_length=MAX_KNOWLEDGE_BODY_CHARS)
    version: str = Field(default="", max_length=80)
    # 来源说明（例如仓库路径、负责人）。它是**说明文本**，不是凭据。
    source: str = Field(default="", max_length=300)
    # 可选：限定到某个应用。留空表示组织内通用。
    application_id: str = Field(default="", max_length=120)
    status: KnowledgeStatus = "active"


class KnowledgeSnippet(BaseModel):
    """一份知识里的可引用片段。"""

    evidence_id: str
    doc_id: str
    section: str = ""
    version: str = ""
    status: str = "unknown"
    snippet: str = ""


class KnowledgeView(BaseModel):
    """对外暴露的项目知识视图。"""

    knowledge_id: str
    organization_id: str
    application_id: str = ""
    kind: KnowledgeKind
    title: str
    version: str = ""
    source: str = ""
    status: KnowledgeStatus = "active"
    content_hash: str = ""
    imported_by: str = ""
    imported_at: datetime | None = None
    deprecated_at: datetime | None = None
    # 导入时对正文做的**提示注入筛查**结果（仅作为数据风险提示，不阻止导入，
    # 也不因此提升或降低任何权限）。
    injection_hits: list[str] = Field(default_factory=list)
    snippet_count: int = 0


class KnowledgeDetail(KnowledgeView):
    """详情视图：附带可核对片段。"""

    snippets: list[KnowledgeSnippet] = Field(default_factory=list)


class KnowledgeSearchHit(BaseModel):
    """服务端在权限过滤之后返回的一条检索命中。"""

    knowledge_id: str
    doc_id: str
    title: str
    section: str = ""
    version: str = ""
    status: str = "unknown"
    snippet: str = ""
    score: float = 0.0
