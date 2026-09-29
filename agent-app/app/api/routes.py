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
from typing import Literal

from fastapi import APIRouter, Depends, HTTPException, Query, Request, status
from fastapi.responses import JSONResponse

from app.schemas.drafts import (
    ClarifyRequest, ConfirmRequest, CreateTaskRequest, DeleteTaskPreview,
    DeleteTaskRequest, DraftEditRequest, DraftVersion, DraftVersionDiff,
    LinkChangeRequest, TaskStatus, TaskTrace, TaskView,
)
from app.schemas.evals import EvalComparison, EvalJobRequest, EvalJobView, EvalReport
from app.schemas.knowledge import (
    KnowledgeDetail,
    KnowledgeImportRequest,
    KnowledgeSearchHit,
    KnowledgeView,
)
from app.service import (
    AgentService,
    EvalInvalid,
    EvalNotFound,
    EvalStateUnavailable,
    KnowledgeForbidden,
    KnowledgeInvalid,
    KnowledgeNotFound,
    KnowledgeStateUnavailable,
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
async def list_tasks(
    request: Request, q: str = Query(default="", max_length=200),
    status: TaskStatus | None = None,
    source: Literal["default", "all", "production", "evaluation", "demo", "legacy"] = "default",
    workspace: Literal["active", "archived", "trash", "all"] = "active",
) -> list[TaskView]:
    # 上下文必须传进 service：过滤在那里生效，路由层不做业务过滤。
    context = await resolve_context(request)
    return await _service(request).list_tasks(context, q=q, status=status, source=source, workspace=workspace)


@router.get("/tasks/{task_id}/delete-preview", response_model=DeleteTaskPreview)
async def delete_preview(task_id: str, request: Request) -> DeleteTaskPreview:
    context = await resolve_context(request)
    try:
        return await _service(request).delete_preview(task_id, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error


@router.post("/tasks/{task_id}/archive", response_model=TaskView)
async def archive_task(task_id: str, request: Request) -> TaskView:
    return await _manage_task(task_id, request, "archive_task")


@router.post("/tasks/{task_id}/restore", response_model=TaskView)
async def restore_task(task_id: str, request: Request) -> TaskView:
    return await _manage_task(task_id, request, "restore_task")


@router.post("/tasks/{task_id}/delete", response_model=TaskView)
async def delete_task(task_id: str, payload: DeleteTaskRequest, request: Request) -> TaskView:
    return await _manage_task(task_id, request, "delete_task", record_version=payload.record_version)


async def _manage_task(task_id: str, request: Request, operation: str, **kwargs: str) -> TaskView:
    context = await resolve_context(request)
    try:
        return await getattr(_service(request), operation)(task_id, context, **kwargs)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=409, detail=str(error)) from error
    except TaskStateUnavailable as error:
        raise HTTPException(status_code=503, detail=str(error)) from error


@router.get("/tasks/{task_id}/trace", response_model=TaskTrace)
async def task_trace(task_id: str, request: Request) -> TaskTrace:
    """执行轨迹：真实步骤、状态、耗时、引用、token、费用与失败原因（不含模型思维链）。"""
    context = await resolve_context(request)
    try:
        return await _service(request).task_trace(task_id, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error


@router.get("/tasks/{task_id}/draft/versions", response_model=list[DraftVersion])
async def draft_versions(task_id: str, request: Request) -> list[DraftVersion]:
    context = await resolve_context(request)
    try:
        return await _service(request).draft_versions(task_id, context)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error


@router.get("/tasks/{task_id}/draft/diff", response_model=DraftVersionDiff)
async def draft_diff(
    task_id: str, request: Request,
    from_version: int | None = Query(default=None, alias="from"),
    to_version: int | None = Query(default=None, alias="to"),
) -> DraftVersionDiff:
    context = await resolve_context(request)
    try:
        return await _service(request).draft_version_diff(task_id, context, from_version, to_version)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=409, detail=str(error)) from error


@router.post("/tasks/{task_id}/draft", response_model=TaskView)
async def edit_draft(task_id: str, payload: DraftEditRequest, request: Request) -> TaskView:
    """服务端版本化编辑草案：校验归属/状态/预期版本，重新执行确定性检查，失败关闭。"""
    context = await resolve_context(request)
    try:
        return await _service(request).edit_draft(task_id, context, payload)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=409, detail=str(error)) from error
    except TaskStateUnavailable as error:
        raise HTTPException(status_code=503, detail=str(error)) from error


@router.post("/tasks/{task_id}/change-links", response_model=TaskView)
async def link_change(task_id: str, payload: LinkChangeRequest, request: Request) -> TaskView:
    """把任务关联到一个已存在的正式变更单（人工动作；服务端校验 + 幂等）。"""
    context = await resolve_context(request)
    try:
        return await _service(request).link_change(task_id, context, payload)
    except TaskNotFound as error:
        raise HTTPException(status_code=404, detail="任务不存在") from error
    except TaskNotResumable as error:
        raise HTTPException(status_code=409, detail=str(error)) from error
    except TaskStateUnavailable as error:
        raise HTTPException(status_code=503, detail=str(error)) from error


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
    except TaskNotResumable as error:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail=str(error)) from error
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


# -- M3：项目知识 -----------------------------------------------------------
# 可见范围由服务端按 (组织, 应用, 生效状态) 判定；跨组织一律 404，不提供存在性探测。


@router.post("/knowledge", response_model=KnowledgeView, status_code=status.HTTP_201_CREATED)
async def import_knowledge(payload: KnowledgeImportRequest, request: Request) -> KnowledgeView:
    context = await resolve_context(request)
    try:
        return await _service(request).import_knowledge(payload, context)
    except KnowledgeInvalid as error:
        raise HTTPException(status_code=status.HTTP_422_UNPROCESSABLE_ENTITY, detail=str(error)) from error
    except KnowledgeForbidden as error:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail=str(error)) from error
    except KnowledgeStateUnavailable as error:
        raise HTTPException(status_code=status.HTTP_503_SERVICE_UNAVAILABLE, detail=str(error)) from error


@router.get("/knowledge", response_model=list[KnowledgeView])
async def list_knowledge(
    request: Request,
    kind: Literal["norms", "cases", "schema"] | None = None,
    status_filter: Literal["active", "deprecated"] | None = Query(default=None, alias="status"),
    application_id: str | None = None,
) -> list[KnowledgeView]:
    context = await resolve_context(request)
    try:
        return await _service(request).list_knowledge(
            context, kind=kind, status=status_filter, application_id=application_id
        )
    except KnowledgeForbidden as error:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail=str(error)) from error


@router.get("/knowledge/search", response_model=list[KnowledgeSearchHit])
async def search_knowledge(
    request: Request,
    q: str = Query(min_length=1, max_length=200),
    kind: Literal["norms", "cases", "schema"] | None = None,
    application_id: str = "",
    limit: int = Query(default=8, ge=1, le=20),
) -> list[KnowledgeSearchHit]:
    """在**服务端权限过滤之后**检索本组织可见的项目知识。"""
    context = await resolve_context(request)
    try:
        return await _service(request).search_knowledge(
            context, q, kind=kind, application_id=application_id, limit=limit
        )
    except KnowledgeForbidden as error:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail=str(error)) from error


@router.get("/knowledge/{knowledge_id}", response_model=KnowledgeDetail)
async def get_knowledge(knowledge_id: str, request: Request) -> KnowledgeDetail:
    context = await resolve_context(request)
    try:
        return await _service(request).get_knowledge(knowledge_id, context)
    except KnowledgeNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="知识不存在") from error
    except KnowledgeForbidden as error:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail=str(error)) from error


@router.post("/knowledge/{knowledge_id}/deprecate", response_model=KnowledgeView)
async def deprecate_knowledge(knowledge_id: str, request: Request) -> KnowledgeView:
    context = await resolve_context(request)
    try:
        return await _service(request).deprecate_knowledge(knowledge_id, context)
    except KnowledgeNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="知识不存在") from error
    except KnowledgeForbidden as error:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail=str(error)) from error
    except KnowledgeStateUnavailable as error:
        raise HTTPException(status_code=status.HTTP_503_SERVICE_UNAVAILABLE, detail=str(error)) from error


# -- M3：独立评测中心 -------------------------------------------------------
# 作业、报告与对照按组织隔离；评测在独立目录运行，不触碰正式业务存储。

@router.post("/evals", response_model=EvalJobView, status_code=status.HTTP_201_CREATED)
async def create_eval_job(payload: EvalJobRequest, request: Request) -> EvalJobView:
    context = await resolve_context(request)
    try:
        return await _service(request).create_eval_job(payload, context)
    except EvalInvalid as error:
        raise HTTPException(status_code=status.HTTP_422_UNPROCESSABLE_ENTITY, detail=str(error)) from error
    except EvalStateUnavailable as error:
        raise HTTPException(status_code=status.HTTP_503_SERVICE_UNAVAILABLE, detail=str(error)) from error


@router.get("/evals", response_model=list[EvalJobView])
async def list_eval_jobs(request: Request) -> list[EvalJobView]:
    context = await resolve_context(request)
    return await _service(request).list_eval_jobs(context)


@router.get("/evals/compare", response_model=EvalComparison)
async def compare_eval_jobs(
    request: Request, base: str = Query(min_length=1), target: str = Query(min_length=1)
) -> EvalComparison:
    """并排对照两次作业，只陈述实测差异，不预设提升比例。"""
    context = await resolve_context(request)
    try:
        return await _service(request).compare_eval_jobs(base, target, context)
    except EvalNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="评测作业不存在") from error


@router.get("/evals/{job_id}", response_model=EvalJobView)
async def get_eval_job(job_id: str, request: Request) -> EvalJobView:
    context = await resolve_context(request)
    try:
        return await _service(request).get_eval_job(job_id, context)
    except EvalNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="评测作业不存在") from error


@router.get("/evals/{job_id}/report", response_model=EvalReport)
async def eval_report(job_id: str, request: Request) -> EvalReport:
    context = await resolve_context(request)
    try:
        return await _service(request).eval_report(job_id, context)
    except EvalNotFound as error:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="评测作业不存在") from error
