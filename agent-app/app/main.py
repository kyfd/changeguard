"""FastAPI 应用装配。

本服务是**内部后端**：界面与登录都由 ChangeGuard 治理服务提供，
浏览器不直接访问这里（治理服务在 `/api/agent/*` 上做同源反向代理）。
"""

from __future__ import annotations

from typing import Any, Mapping, Sequence

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse

from app.api.routes import AgentNotConfigured, agent_not_configured_handler, router
from app.config import Settings
from app.service import AgentService
from app.usage import UsageLimits, UsageLimiter

#: 请求字段的中文名。校验消息要能直接给用户看，不能吐 pydantic 的英文原文。
_FIELD_LABELS = {
    "requirement": "需求",
    "application": "应用",
    "environment": "环境",
    "database": "数据库类型",
    "table": "表名",
    "query_sql": "查询 SQL",
    "planned_at": "计划时间",
    "planned_at_timezone": "时区",
    "schema_snapshot": "表结构快照",
    "note": "补充说明",
    "material_hash": "材料哈希",
}


def _field_label(loc: Sequence[Any] | None) -> str:
    """从 pydantic 的 `loc` 里取出人可读的字段名。"""
    parts = [
        str(item)
        for item in (loc or ())
        if str(item) not in {"body", "query", "path", "header", "cookie"}
    ]
    tail = parts[-1] if parts else "请求"
    return _FIELD_LABELS.get(tail, tail)


def describe_request_error(errors: Sequence[Mapping[str, Any]]) -> str:
    """把结构化校验错误压成**一句人话**。

    两条口径：

    - **绝不回显提交内容**。pydantic 的校验错误自带 `input`，里面是用户原文——
      需求正文里可能有库表名、内部标识，把它拼进错误消息等于把隐私又抄一遍送回去。
      这里只允许用它算长度（`string_too_long` 那条），不拼进消息。
    - 说清"哪里不对、怎么改"，而不是把英文原文丢给调用方。
    """
    if not errors:
        return "请求格式不合法。"

    first = errors[0]
    kind = str(first.get("type") or "")
    field = _field_label(first.get("loc"))
    context = first.get("ctx") or {}
    raw = first.get("input")

    if kind == "string_too_long":
        limit = context.get("max_length")
        message = f"{field}最多 {limit} 字"
        if isinstance(raw, str):
            message += f"，当前 {len(raw)} 字"
        message += "。请精简后重试。"
    elif kind == "string_too_short":
        message = f"{field}不能为空。"
    elif kind == "missing":
        message = f"缺少必填字段：{field}。"
    elif kind in {"enum", "literal_error"}:
        message = f"{field}的取值不在允许范围内。"
    elif kind.endswith(("_parsing", "_type")) or "datetime" in kind or "time" in kind:
        message = f"{field}的格式不合法。"
    else:
        message = f"{field}取值不合法。"

    if len(errors) > 1:
        message += f"（共 {len(errors)} 处校验问题）"
    return message


def create_app(settings: Settings | None = None) -> FastAPI:
    resolved = settings or Settings.from_env()
    application = FastAPI(
        title="ChangeGuard 数据库变更准备 Agent",
        version="0.1.0",
        description=(
            "需求澄清、规范检索、结构化草案与确定性检查。"
            "审批与发布仍由原有 Go 治理后端控制，模型不参与放行判定。"
            "界面由治理服务同源提供（/agent/），本服务只暴露接口。"
        ),
    )
    application.state.settings = resolved
    application.state.service = AgentService(resolved)
    # 用量闸门：路由层在每次会真正调用模型的请求前检查（超出返回 429）。
    application.state.usage = UsageLimiter(
        UsageLimits(
            per_minute=resolved.rate_per_minute,
            per_user_daily=resolved.user_daily_limit,
            global_daily=resolved.global_daily_limit,
        )
    )
    application.include_router(router)

    @application.exception_handler(AgentNotConfigured)
    async def _agent_not_configured(request: Request, error: AgentNotConfigured) -> JSONResponse:
        """"未启用"的 503 需要机器可读错误码；其它异常走 FastAPI 默认处理。"""
        return await agent_not_configured_handler(request, error)

    @application.exception_handler(RequestValidationError)
    async def _validation_error(_request: Request, error: RequestValidationError) -> JSONResponse:
        """不用默认响应：它会把 `input` 原样回显，且结构是嵌套数组。

        默认体的形状决定了调用方只能把它 `JSON.stringify` 成一大坨 JSON 摊在用户面前
        （工作台当初就是这么显示的）。这里统一压成一句人话——对所有调用方都生效，
        不只是工作台。
        """
        return JSONResponse(
            status_code=422,
            content={"detail": describe_request_error(error.errors())},
        )

    @application.get("/", include_in_schema=False)
    async def root() -> dict[str, str]:
        # 刻意不提供界面：合并部署后只有治理服务对外，避免出现第二个入口。
        return {
            "service": "changeguard-agent",
            "role": "internal backend",
            "ui": "由 ChangeGuard 治理服务提供（/agent/）",
            "docs": "/docs",
        }

    return application


app = create_app()
