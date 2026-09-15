"""运行配置。

所有配置都来自环境变量，默认值保证**不配置模型也能跑**：
没有模型时使用确定性草案生成器，这是可运行状态，不是错误状态。
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


def _default_repo_root() -> Path:
    # app/config.py -> app -> agent-app -> 仓库根
    return Path(__file__).resolve().parents[2]


@dataclass(frozen=True)
class Settings:
    """服务配置。"""

    # 治理后端（Go）。Agent 的所有业务证据都必须经过它。
    governance_base_url: str = "http://127.0.0.1:8080"
    governance_timeout_seconds: float = 10.0

    # 模型配置。base_url 或 api_key 为空时走确定性兜底。
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "gpt-4o-mini"
    llm_timeout_seconds: float = 20.0
    llm_max_attempts: int = 2
    llm_max_tokens: int = 1200

    # 工作流预算：有限循环，不允许无上限修订。
    max_revisions: int = 2
    task_timeout_seconds: float = 120.0

    # 证据来源
    agent_demo_dir: str = ""
    task_store_path: str = "data/agent-tasks.json"

    # 执行模式：
    #   background（默认）—— 接口立即返回，执行交给后台任务，可取消；
    #   inline            —— 接口内执行完毕再返回，便于测试与单步调试。
    execution_mode: str = "background"

    # 身份来源。默认**关闭**：不接受由调用方自行声明的组织身份。
    # 合并部署下由 ChangeGuard 治理服务在服务端解析会话后注入，并显式打开本项。
    allow_header_identity: bool = False

    # 上游（ChangeGuard 治理服务）共享密钥。
    # 设置后，所有 /api/agent 请求必须携带匹配的 X-Agent-Upstream-Token。
    # 意义是：本服务即使被误暴露，也不能被直接调用冒充治理服务。
    upstream_token: str = ""

    # 用量限额（app/usage.py）。模型调用按量计费，这三层是防滥用的闸门；
    # 0 表示不启用对应层。账单硬顶仍由模型服务商控制台的用量封顶负责。
    rate_per_minute: int = 6
    user_daily_limit: int = 60
    global_daily_limit: int = 400

    @property
    def llm_configured(self) -> bool:
        return bool(self.llm_base_url.strip()) and bool(self.llm_api_key.strip())

    @property
    def demo_dir(self) -> Path:
        if self.agent_demo_dir.strip():
            return Path(self.agent_demo_dir)
        return _default_repo_root() / "examples" / "agent-demo"

    @classmethod
    def from_env(cls) -> "Settings":
        defaults = cls()
        return cls(
            governance_base_url=os.getenv("AGENT_GOVERNANCE_BASE_URL", defaults.governance_base_url).rstrip("/"),
            governance_timeout_seconds=float(os.getenv("AGENT_GOVERNANCE_TIMEOUT", "10")),
            llm_base_url=os.getenv("AGENT_LLM_BASE_URL", "").strip(),
            llm_api_key=os.getenv("AGENT_LLM_API_KEY", "").strip(),
            llm_model=os.getenv("AGENT_LLM_MODEL", defaults.llm_model).strip(),
            llm_timeout_seconds=float(os.getenv("AGENT_LLM_TIMEOUT", "20")),
            llm_max_attempts=int(os.getenv("AGENT_LLM_MAX_ATTEMPTS", "2")),
            llm_max_tokens=int(os.getenv("AGENT_LLM_MAX_TOKENS", "1200")),
            max_revisions=int(os.getenv("AGENT_MAX_REVISIONS", "2")),
            task_timeout_seconds=float(os.getenv("AGENT_TASK_TIMEOUT", "120")),
            agent_demo_dir=os.getenv("AGENT_DEMO_DIR", "").strip(),
            task_store_path=os.getenv("AGENT_TASK_STORE", defaults.task_store_path),
            execution_mode=os.getenv("AGENT_EXECUTION_MODE", defaults.execution_mode).strip() or defaults.execution_mode,
            allow_header_identity=os.getenv("AGENT_ALLOW_HEADER_IDENTITY", "0").strip()
            in {"1", "true", "on", "yes"},
            upstream_token=os.getenv("AGENT_UPSTREAM_TOKEN", "").strip(),
            rate_per_minute=int(os.getenv("AGENT_RATE_PER_MINUTE", str(defaults.rate_per_minute))),
            user_daily_limit=int(os.getenv("AGENT_USER_DAILY_LIMIT", str(defaults.user_daily_limit))),
            global_daily_limit=int(os.getenv("AGENT_GLOBAL_DAILY_LIMIT", str(defaults.global_daily_limit))),
        )
