"""HTTP 接口。

部署形态：本服务是**内部后端**，只有 ChangeGuard 治理服务会调用它，
界面由治理服务同源提供（`internal/httpapi/web/agent/`）。因此这里有两道边界：

- **上游凭据**：配置 `AGENT_UPSTREAM_TOKEN` 后，所有 `/api/agent` 请求必须携带
  匹配的 `X-Agent-Upstream-Token`（常量时间比较）。这样即使本服务被误暴露，
  也不能被直接调用来冒充治理服务。
- **身份**：`X-Actor-Id` / `X-Org-Id` 由治理服务在**服务端**解析会话后注入。
  组织范围只用于在服务端构造 `TrustedContext`，**不出现在请求体里，也不是工具参数**；
  未开启 `AGENT_ALLOW_HEADER_IDENTITY` 时一律 401 —— 不安全的默认值是关闭的。
"""

from __future__ import annotations

import secrets

from fastapi import APIRouter, Depends, HTTPException, Request, status

from app.schemas.drafts import ClarifyRequest, CreateTaskRequest, TaskView
from app.service import AgentService, TaskNotFound, TaskNotResumable
from app.tools.registry import TrustedContext
from app.usage import QuotaExceeded


def verify_upstream(request: Request) -> None:
    """校验上游共享密钥。未配置时不启用该检查（仅适用于本机开发）。"""
    expected = request.app.state.settings.upstream_token
    if not expected:
        return
    provided = (request.headers.get("X-Agent-Upstream-Token") or "").strip()
    if not provided or not secrets.compare_digest(provided, expected):
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="上游凭据无效")


router = APIRouter(prefix="/api/agent", tags=["agent"], dependencies=[Depends(verify_upstream)])

IDENTITY_HINT = (
    "未配置身份来源：默认拒绝。"
    "合并部署下请设置 AGENT_ALLOW_HEADER_IDENTITY=1，由 ChangeGuard 治理服务"
    "在服务端解析会话后注入 X-Actor-Id / X-Org-Id；"
    "本服务不应被浏览器直接访问。"
)


def _service(request: Request) -> AgentService:
    return request.app.state.service


def _enforce_usage(request: Request, context: TrustedContext) -> None:
    """模型调用前的用量闸门。限额键是治理服务注入的 X-Actor-Id，不可伪造。"""
    try:
        request.app.state.usage.check(context.user_id)
    except QuotaExceeded as error:
        raise HTTPException(status_code=status.HTTP_429_TOO_MANY_REQUESTS, detail=error.detail) from error


async def resolve_context(request: Request) -> TrustedContext:
    """把已认证请求解析成可信上下文。"""
    settings = request.app.state.settings
    if not settings.allow_header_identity:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail=IDENTITY_HINT)

    user_id = (request.headers.get("X-Actor-Id") or "").strip()
    organization_id = (request.headers.get("X-Org-Id") or "").strip()
    if not user_id or not organization_id:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="缺少 X-Actor-Id 或 X-Org-Id",
        )
    return TrustedContext(user_id=user_id, organization_id=organization_id)


@router.get("/healthz")
async def healthz(request: Request) -> dict:
    return await _service(request).health()


@router.get("/provider")
async def provider(request: Request) -> dict:
    return _service(request).provider_info()


@router.get("/tools")
async def tools(request: Request) -> dict:
    # 工具清单只有名称与 schema，不含任何业务数据，因此不需要身份。
    service = _service(request)
    registry = service.build_tool_registry_for_introspection()
    return {"tools": registry.specs()}


@router.post("/tasks", response_model=TaskView, status_code=status.HTTP_202_ACCEPTED)
async def create_task(payload: CreateTaskRequest, request: Request) -> TaskView:
    context = await resolve_context(request)
    _enforce_usage(request, context)
    view, _ = await _service(request).create_task(payload, context)
    return view


@router.get("/tasks", response_model=list[TaskView])
async def list_tasks(request: Request) -> list[TaskView]:
    context = await resolve_context(request)
    return await _service(request).list_tasks(context)


@router.get("/tasks/{task_id}", response_model=TaskView)
async def get_task(task_id: str, request: Request) -> TaskView:
    context = await resolve_context(request)
    try:
        return await _service(request).get_task(task_id, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error


@router.post("/tasks/{task_id}/clarify", response_model=TaskView)
async def clarify(task_id: str, payload: ClarifyRequest, request: Request) -> TaskView:
    context = await resolve_context(request)
    # clarify 会恢复工作流并再次调用模型，所以与 create 共用同一套用量闸门。
    _enforce_usage(request, context)
    try:
        return await _service(request).clarify(task_id, payload, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(error)) from error


@router.post("/tasks/{task_id}/cancel", response_model=TaskView)
async def cancel(task_id: str, request: Request) -> TaskView:
    context = await resolve_context(request)
    try:
        return await _service(request).cancel(task_id, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error
