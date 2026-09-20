"""FastAPI 应用装配。

本服务是**内部后端**：界面与登录都由 ChangeGuard 治理服务提供，
浏览器不直接访问这里（治理服务在 `/api/agent/*` 上做同源反向代理）。
"""

from __future__ import annotations

from fastapi import FastAPI

from app.api.routes import router
from app.config import Settings
from app.service import AgentService
from app.usage import UsageLimits, UsageLimiter


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
