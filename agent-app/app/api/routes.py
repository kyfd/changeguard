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
from fastapi.responses import JSONResponse

from app.schemas.drafts import ClarifyRequest, ConfirmRequest, CreateTaskRequest, TaskView
from app.service import (
    AgentService,
    TaskCancelRejected,
    TaskNotConfirmable,
    TaskNotFound,
    TaskNotResumable,
    TaskStateUnavailable,
)
from app.tools.registry import TrustedContext
from app.usage import QuotaExceeded

#: 机器可读错误码：与治理服务 `writeError` 的响应形状（`error`/`code`/`message`）保持一致，
#: 工作台 `app.js` 靠它区分"功能未启用"（不可重试）与"应用层 503"（可重试）。
SERVICE_UNAVAILABLE_CODE = "SERVICE_UNAVAILABLE"


class AgentNotConfigured(HTTPException):
    """头身份模式缺少上游密钥：功能**未启用**，不是可重试的瞬时故障。

    刻意用独立异常类：只有这一种 503 会带上 `SERVICE_UNAVAILABLE` 错误码。
    应用层的 503（例如状态没能落盘、取消尚未生效）**故意不带**该码，
    否则前端会把一次存储抖动显示成"功能未启用"并禁用整个面板。
    """

    def __init__(self) -> None:
        super().__init__(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail="头身份模式必须配置 AGENT_UPSTREAM_TOKEN",
        )


def verify_upstream(request: Request) -> None:
    """头身份模式必须有上游共享密钥；缺失时失败关闭（含本地开发）。"""
    expected = request.app.state.settings.upstream_token
    if not expected:
        if request.app.state.settings.allow_header_identity:
            raise AgentNotConfigured()
        return
    provided = (request.headers.get("X-Agent-Upstream-Token") or "").strip()
    if not provided or not secrets.compare_digest(provided, expected):
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="上游凭据无效")


router = APIRouter(prefix="/api/agent", tags=["agent"], dependencies=[Depends(verify_upstream)])


async def agent_not_configured_handler(_request: Request, error: AgentNotConfigured) -> JSONResponse:
    """给"未启用"这一种 503 补上治理服务约定形状的机器可读错误码。

    治理服务原样透传下游响应体，工作台靠 `code === "SERVICE_UNAVAILABLE"` 判定
    "功能未启用"（app.js）。只有 `detail` 时它会显示"可稍后重试"，
    把配置缺失误诊成瞬时故障，用户会一直重试而不去配密钥。

    响应同时保留 `detail`，不破坏既有依赖该字段的调用方。
    """
    message = error.detail if isinstance(error.detail, str) else str(error.detail)
    return JSONResponse(
        status_code=error.status_code,
        content={"error": message, "code": SERVICE_UNAVAILABLE_CODE, "message": message, "detail": message},
        headers=error.headers,
    )


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
    # 上下文必须传进 service：过滤在那里生效，路由层不做业务过滤。
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
    try:
        await _service(request).get_task(task_id, context)
        _enforce_usage(request, context)
        return await _service(request).clarify(task_id, payload, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(error)) from error
    except TaskStateUnavailable as error:
        # 存储不可用导致状态无法确定：不能返回可能是过期的视图。
        raise HTTPException(status_code=status.HTTP_503_SERVICE_UNAVAILABLE, detail=str(error)) from error


@router.post("/tasks/{task_id}/cancel", response_model=TaskView)
async def cancel(task_id: str, request: Request) -> TaskView:
    context = await resolve_context(request)
    try:
        return await _service(request).cancel(task_id, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error
    except (TaskCancelRejected, TaskStateUnavailable) as error:
        # 取消未生效、任务仍在运行：明确告诉调用方可以重试，
        # 而不是返回一个"看起来已取消"的结果。
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE, detail=str(error), headers={"Retry-After": "1"}
        ) from error


@router.post("/tasks/{task_id}/resume", response_model=TaskView)
async def resume(task_id: str, request: Request, payload: ClarifyRequest | None = None) -> TaskView:
    """从检查点恢复。

    恢复前由服务层重新校验归属、执行代际与输入/材料版本；校验不过返回 409，
    **不做**"尽力继续"。请求体可选：不提供时按记录中已有的输入继续。
    """
    context = await resolve_context(request)
    try:
        await _service(request).get_task(task_id, context)
        _enforce_usage(request, context)
        return await _service(request).resume(task_id, context, payload)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(error)) from error
    except TaskStateUnavailable as error:
        raise HTTPException(status_code=status.HTTP_503_SERVICE_UNAVAILABLE, detail=str(error)) from error


@router.post("/tasks/{task_id}/confirm", response_model=TaskView)
async def confirm(task_id: str, request: Request, payload: ConfirmRequest | None = None) -> TaskView:
    """记录一次人工**材料确认**。

    材料确认 ≠ 治理审批 ≠ 执行许可：这里只记录"谁在什么时候确认了哪一版材料"，
    不改变放行判定，也不授予执行权利。重复确认同一版本是幂等的。
    """
    context = await resolve_context(request)
    try:
        return await _service(request).confirm(task_id, context, payload)
    except TaskNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="任务不存在") from error
    except TaskNotConfirmable as error:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(error)) from error
