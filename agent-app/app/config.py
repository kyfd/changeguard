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
    # provider 层：只针对传输失败与暂时性服务端故障（限流、超时、5xx）重试。
    llm_max_attempts: int = 2
    # 工作流层：只针对"模型输出无法解析成草案"重试，与上面是两个独立开关。
    # 一次生成最多打 `llm_max_attempts × draft_parse_attempts` 次模型调用——
    # 以前两层共用同一个值，实际次数被悄悄平方。
    draft_parse_attempts: int = 1
    llm_max_tokens: int = 1200

    # 任务级 token / 费用预算。0 = 不限制。
    #   max_task_tokens             任务累计 token（prompt + completion）上限
    #   max_task_prompt_tokens      任务累计 prompt token 上限
    #   max_task_cost_estimate      任务费用上限；**没有定价数据时失败关闭**，不会假装已生效
    #   unknown_usage_charge_tokens 未提供 usage 的响应在**预算判定**中按这个值保守计入
    #                               （0 = 用 llm_max_tokens 作为单次响应的上界）
    #
    # 注意：这只在"能算出来"的前提下限制。显示 unknown 不等于实现了预算——
    # 缺 usage 时按保守值计入判定，缺定价时费用上限直接失败关闭。
    max_task_tokens: int = 0
    max_task_prompt_tokens: int = 0
    max_task_cost_estimate: float = 0.0
    unknown_usage_charge_tokens: int = 0
    llm_price_prompt_per_1k: float = 0.0
    llm_price_completion_per_1k: float = 0.0

    # 工作流预算：有限循环，不允许无上限修订。
    max_revisions: int = 2
    task_timeout_seconds: float = 120.0

    # 调查策略：
    #   fixed_workflow（默认）—— 现有的固定检索顺序，行为不变；
    #   bounded_agent          —— 受约束调查循环，边界由代码强制（见 app/workflow/investigate.py）。
    # 默认保持 fixed_workflow：新循环在验证充分前不应改变既有行为。
    investigation_strategy: str = "fixed_workflow"
    # 决策者：rule 是**确定性规则**决策者（不冒充模型）；provider 要求模型具备原生动作能力，
    # 不具备时会显式报告不可用，而不是用规则顶替并宣称是模型决策。
    investigation_planner: str = "rule"
    # 调查预算。轮次与累计工具调用是两个独立上限：只限轮次挡不住一轮里调很多次。
    max_investigation_rounds: int = 4
    max_total_tool_calls: int = 8
    # 单工具超时：超时按失败处理，不让循环挂住，也不当成"没问题"。
    tool_timeout_seconds: float = 5.0

    # 终态落盘失败时的**有上限**重试：一次存储抖动不应该让已经跑完的任务变成孤儿，
    # 但也不允许无限重试。用尽后按降级处理（健康状态报告 degraded，见 AgentService.health）。
    persist_max_attempts: int = 3
    persist_retry_backoff_seconds: float = 0.05

    # 证据来源
    agent_demo_dir: str = ""
    task_store_path: str = "data/agent-tasks.json"
    # LangGraph 检查点（SQLite 单实例）。任务记录仍存 JSON；这里只保存**节点级**图状态，
    # 使中断后能从检查点继续，而不是从头重跑整条流程。
    #
    # 留空表示**与任务存储同目录**（默认即 data/agent-checkpoints.sqlite，与固定默认一致）。
    # 这样"改了任务存储路径"的调用方（尤其是测试）不会继续共用仓库里那一个检查点文件——
    # 共享同一个 SQLite 文件会在并发打开时产生锁冲突，也会让用例之间互相污染状态。
    checkpoint_path: str = ""

    # 执行模式：
    #   background（默认）—— 接口立即返回，执行交给后台任务，可取消；
    #   inline            —— 接口内执行完毕再返回，便于测试与单步调试。
    execution_mode: str = "background"

    # 身份来源。默认**关闭**：不接受由调用方自行声明的组织身份。
    # 合并部署下由 ChangeGuard 治理服务在服务端解析会话后注入，并显式打开本项。
    allow_header_identity: bool = False

    # 与 ChangeGuard 治理服务的共享密钥，**双向**都要求它：
    #   - 治理服务 → 本服务：所有 /api/agent 请求必须携带匹配的 X-Agent-Upstream-Token，
    #     这样本服务即使被误暴露，也不能被直接调用来冒充治理服务；
    #   - 本服务 → 治理服务：三个远程只读工具调用内部只读接口
    #     /api/agent-tools/changes/{id} 时同样携带它。
    # 未配置时，远程只读工具**显式不可用**，不会退化成匿名读取。
    upstream_token: str = ""

    @property
    def llm_configured(self) -> bool:
        return bool(self.llm_base_url.strip()) and bool(self.llm_api_key.strip())

    @property
    def checkpoint_file(self) -> str:
        """解析检查点文件路径。

        `checkpoint_path` 显式配置时以它为准；否则与任务存储放在同一目录，
        避免"换了任务存储却仍共用默认检查点文件"这一类共享状态问题。
        """
        explicit = (self.checkpoint_path or "").strip()
        if explicit:
            return explicit
        store = (self.task_store_path or "").strip()
        if not store:
            return ""
        return str(Path(store).with_name("agent-checkpoints.sqlite"))

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
            draft_parse_attempts=int(os.getenv("AGENT_DRAFT_PARSE_ATTEMPTS", "1")),
            llm_max_tokens=int(os.getenv("AGENT_LLM_MAX_TOKENS", "1200")),
            max_task_tokens=int(os.getenv("AGENT_MAX_TASK_TOKENS", "0")),
            max_task_prompt_tokens=int(os.getenv("AGENT_MAX_TASK_PROMPT_TOKENS", "0")),
            max_task_cost_estimate=float(os.getenv("AGENT_MAX_TASK_COST", "0")),
            unknown_usage_charge_tokens=int(os.getenv("AGENT_UNKNOWN_USAGE_CHARGE_TOKENS", "0")),
            llm_price_prompt_per_1k=float(os.getenv("AGENT_LLM_PRICE_PROMPT_PER_1K", "0")),
            llm_price_completion_per_1k=float(os.getenv("AGENT_LLM_PRICE_COMPLETION_PER_1K", "0")),
            max_revisions=int(os.getenv("AGENT_MAX_REVISIONS", "2")),
            task_timeout_seconds=float(os.getenv("AGENT_TASK_TIMEOUT", "120")),
            investigation_strategy=os.getenv("AGENT_INVESTIGATION_STRATEGY", "fixed_workflow").strip()
            or "fixed_workflow",
            investigation_planner=os.getenv("AGENT_INVESTIGATION_PLANNER", "rule").strip() or "rule",
            max_investigation_rounds=int(os.getenv("AGENT_MAX_INVESTIGATION_ROUNDS", "4")),
            max_total_tool_calls=int(os.getenv("AGENT_MAX_TOTAL_TOOL_CALLS", "8")),
            tool_timeout_seconds=float(os.getenv("AGENT_TOOL_TIMEOUT", "5")),
            persist_max_attempts=int(os.getenv("AGENT_PERSIST_MAX_ATTEMPTS", "3")),
            persist_retry_backoff_seconds=float(os.getenv("AGENT_PERSIST_RETRY_BACKOFF", "0.05")),
            agent_demo_dir=os.getenv("AGENT_DEMO_DIR", "").strip(),
            task_store_path=os.getenv("AGENT_TASK_STORE", defaults.task_store_path),
            checkpoint_path=os.getenv("AGENT_CHECKPOINT_PATH", defaults.checkpoint_path),
            execution_mode=os.getenv("AGENT_EXECUTION_MODE", defaults.execution_mode).strip() or defaults.execution_mode,
            allow_header_identity=os.getenv("AGENT_ALLOW_HEADER_IDENTITY", "0").strip()
            in {"1", "true", "on", "yes"},
            upstream_token=os.getenv("AGENT_UPSTREAM_TOKEN", "").strip(),
        )
