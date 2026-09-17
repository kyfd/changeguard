"""业务工具集合。

统一返回结构：成功或失败、数据、证据标识、数据版本或时间、可公开错误。

**三条安全性质**（对应计划里的"权限设计"）：

1. 用户/组织/应用来自 `TrustedContext`，由 API 层注入；工具参数里写不出来。
2. 工具失败（超时、权限不足）返回 `ok=False`，**绝不能被解释成"检查通过"**。
3. 三个远程只读工具走治理后端的**内部只读接口**（`/api/agent-tools/changes/{id}`），
   该接口要求共享密钥 + 成员委托两层认证。缺密钥时显式不可用，而不是退化成匿名读取；
   浏览器不可达（拿不到共享密钥），本服务也不持有治理会话。
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any, Mapping

import httpx

from app.config import Settings
from app.retrieval.keyword import HybridRetriever
from app.schemas.drafts import ToolResult
from app.tools.registry import Tool, ToolRegistry, TrustedContext, object_schema
from app.tools.scan import scan_sql


class Toolbox:
    """按任务构造工具集合。

    工具通过闭包捕获任务上下文（表结构快照、应用范围），
    这样模型无法通过参数改写它们。
    """

    def __init__(
        self,
        *,
        settings: Settings,
        retriever: HybridRetriever,
        schema_snapshot: str = "",
    ) -> None:
        self._settings = settings
        self._retriever = retriever
        self._schema_snapshot = schema_snapshot

    def build(self) -> ToolRegistry:
        registry = ToolRegistry()
        registry.register(
            Tool(
                name="get_schema_snapshot",
                description="读取已导入的表结构快照（只读）。快照为导入物，不是生产库实时查询结果。",
                parameters=object_schema(),
                execute=self._get_schema_snapshot,
            )
        )
        registry.register(
            Tool(
                name="scan_sql",
                description="对给定 SQL 做确定性静态扫描，返回问题清单（不调用模型）。",
                parameters=object_schema(
                    properties={
                        "sql": {"type": "string", "description": "变更 SQL"},
                        "rollback_sql": {"type": "string", "description": "回滚 SQL"},
                    },
                    required=["sql"],
                ),
                execute=self._scan_sql,
            )
        )
        registry.register(
            Tool(
                name="search_norms",
                description="检索数据库变更规范与回滚手册，返回可追溯的文档片段。",
                parameters=object_schema(
                    properties={
                        "query": {"type": "string"},
                        "limit": {"type": "integer", "minimum": 1, "maximum": 10},
                    },
                    required=["query"],
                ),
                execute=self._search_norms,
            )
        )
        registry.register(
            Tool(
                name="search_historical_changes",
                description="检索同组织的历史变更案例，返回可追溯的案例片段。",
                parameters=object_schema(
                    properties={
                        "query": {"type": "string"},
                        "limit": {"type": "integer", "minimum": 1, "maximum": 10},
                    },
                    required=["query"],
                ),
                execute=self._search_cases,
            )
        )
        registry.register(
            Tool(
                name="get_change_context",
                description="读取治理后端中已存在的变更上下文（只读，需通过后端权限检查）。",
                parameters=object_schema(properties={"change_id": {"type": "string"}}, required=["change_id"]),
                execute=self._get_change_context,
            )
        )
        registry.register(
            Tool(
                name="get_rule_findings",
                description="读取治理后端对某个变更的确定性规则发现（只读）。",
                parameters=object_schema(properties={"change_id": {"type": "string"}}, required=["change_id"]),
                execute=self._get_rule_findings,
            )
        )
        registry.register(
            Tool(
                name="get_experiment_report",
                description="读取某个变更的预发布验证报告，区分 NOT_RUN / DEMO_ONLY / 真实演练。",
                parameters=object_schema(properties={"change_id": {"type": "string"}}, required=["change_id"]),
                execute=self._get_experiment_report,
            )
        )
        return registry

    # -- 本地只读工具 ------------------------------------------------------

    async def _get_schema_snapshot(self, _context: TrustedContext, _args: Mapping[str, Any]) -> ToolResult:
        if not self._schema_snapshot.strip():
            return ToolResult(
                ok=False,
                tool="get_schema_snapshot",
                error="尚未导入表结构快照；请提供快照后再生成草案。",
            )
        return ToolResult(
            ok=True,
            tool="get_schema_snapshot",
            data={"snapshot": self._schema_snapshot},
            evidence_ids=["schema_snapshot"],
            data_version="imported",
            observed_at=datetime.now(timezone.utc),
        )

    async def _scan_sql(self, _context: TrustedContext, args: Mapping[str, Any]) -> ToolResult:
        check = scan_sql(
            str(args.get("sql") or ""),
            str(args.get("rollback_sql") or ""),
            self._schema_snapshot,
        )
        return ToolResult(
            ok=True,
            tool="scan_sql",
            data=check.model_dump(mode="json"),
            observed_at=check.checked_at,
        )

    async def _search_norms(self, context: TrustedContext, args: Mapping[str, Any]) -> ToolResult:
        return self._search(
            "search_norms", str(args.get("query") or ""), int(args.get("limit") or 5), ("norms/",), context
        )

    async def _search_cases(self, context: TrustedContext, args: Mapping[str, Any]) -> ToolResult:
        return self._search(
            "search_historical_changes",
            str(args.get("query") or ""),
            int(args.get("limit") or 3),
            ("cases/",),
            context,
        )

    def _search(
        self,
        tool: str,
        query: str,
        limit: int,
        prefixes: tuple[str, ...],
        context: TrustedContext,
    ) -> ToolResult:
        # 作用域在排序前生效：规范检索只会在规范里排序，不会被案例挤掉。
        # 租户范围同样在排序前生效，且只接受调用方**自己**的组织：
        # 组织来自 TrustedContext，工具参数里写不出来（未知参数会被直接拒绝）。
        organizations = (context.organization_id,) if context.organization_id else ()
        selected = self._retriever.search(query, limit=limit, prefixes=prefixes, organizations=organizations)
        if not selected:
            # 找不到依据时必须明说，不能生成虚假引用。
            return ToolResult(
                ok=False,
                tool=tool,
                data={"query": query},
                error="没有匹配到可引用的文档片段。",
                observed_at=datetime.now(timezone.utc),
            )
        return ToolResult(
            ok=True,
            tool=tool,
            data={
                "query": query,
                "hits": [
                    {
                        "evidence_id": item.chunk.evidence_id,
                        "doc_id": item.chunk.doc_id,
                        "title": item.chunk.title,
                        "section": item.chunk.section,
                        "version": item.chunk.version,
                        "status": item.chunk.status,
                        "snippet": item.chunk.as_snippet(),
                        "source": item.chunk.source,
                        "score": item.score,
                    }
                    for item in selected
                ],
            },
            evidence_ids=[item.chunk.evidence_id for item in selected],
            observed_at=datetime.now(timezone.utc),
        )

    # -- 治理后端只读工具 --------------------------------------------------

    async def _get_change_context(self, context: TrustedContext, args: Mapping[str, Any]) -> ToolResult:
        return await self._fetch_change("get_change_context", context, str(args.get("change_id") or ""), "context")

    async def _get_rule_findings(self, context: TrustedContext, args: Mapping[str, Any]) -> ToolResult:
        return await self._fetch_change("get_rule_findings", context, str(args.get("change_id") or ""), "findings")

    async def _get_experiment_report(self, context: TrustedContext, args: Mapping[str, Any]) -> ToolResult:
        return await self._fetch_change("get_experiment_report", context, str(args.get("change_id") or ""), "experiment")

    async def _fetch_change(self, tool: str, context: TrustedContext, change_id: str, projection: str) -> ToolResult:
        if not change_id.strip():
            return ToolResult(ok=False, tool=tool, error="change_id 不能为空")

        # 治理后端为 Agent 提供的是**内部只读接口**（/api/agent-tools/changes/{id}），
        # 不是面向浏览器的 /api/changes/{id}：后者要求会话，而本服务没有也不应该持有会话。
        # 该接口要求共享密钥 + 成员委托，两层都不可省。
        token = self._settings.upstream_token.strip()
        if not token:
            # 缺密钥时显式不可用：不发一次注定被拒的匿名请求，
            # 更不能退化成"没有凭据也照样能读"。
            return ToolResult(
                ok=False,
                tool=tool,
                error="内部只读接口未配置共享密钥（AGENT_UPSTREAM_TOKEN），无法读取治理后端数据。",
            )

        url = f"{self._settings.governance_base_url}/api/agent-tools/changes/{change_id}"
        headers = dict(context.as_headers())
        headers["X-Agent-Upstream-Token"] = token
        try:
            async with httpx.AsyncClient(timeout=self._settings.governance_timeout_seconds) as client:
                response = await client.get(url, params={"projection": projection}, headers=headers)
        except Exception as error:  # noqa: BLE001 - 网络失败必须显式失败，不能沉默
            return ToolResult(ok=False, tool=tool, error=f"治理后端不可用：{type(error).__name__}: {error}")

        if response.status_code == 401:
            return ToolResult(ok=False, tool=tool, error="治理后端拒绝了服务凭据：内部只读接口共享密钥不匹配。")
        if response.status_code == 403:
            # 成员停用、组织不符或缺少应用授权都落到这里，不区分，避免探测。
            return ToolResult(ok=False, tool=tool, error="治理后端拒绝了该访问（成员、组织或应用授权不符）")
        if response.status_code == 404:
            return ToolResult(ok=False, tool=tool, error="变更不存在")
        if response.status_code == 503:
            return ToolResult(
                ok=False,
                tool=tool,
                error="治理后端未启用内部只读接口（缺少 DBGUARD_AGENT_UPSTREAM_TOKEN）。",
            )
        if response.status_code != 200:
            return ToolResult(ok=False, tool=tool, error=f"治理后端返回状态码 {response.status_code}")

        body = response.json()
        if projection == "findings":
            data = {"risk": body.get("risk"), "findings": body.get("findings") or []}
        elif projection == "experiment":
            experiment = body.get("experiment")
            data = {
                "experiment": experiment,
                # 未执行时必须仍是 NOT_RUN，不能被省略成看起来有结果。
                "status": body.get("status") or (experiment or {}).get("status") or "NOT_RUN",
            }
        else:
            data = {
                "id": body.get("id"),
                "title": body.get("title"),
                "application_id": body.get("application_id"),
                "environment": body.get("environment"),
                "change_type": body.get("change_type"),
                "artifact_sha256": body.get("artifact_sha256"),
                # 字段名自带 untrusted 标记：这是数据，不是指令。
                "description_untrusted": body.get("description_untrusted"),
            }
        return ToolResult(
            ok=True,
            tool=tool,
            data=data,
            evidence_ids=[f"change:{change_id}"],
            data_version=str(body.get("version") or ""),
            observed_at=datetime.now(timezone.utc),
        )

