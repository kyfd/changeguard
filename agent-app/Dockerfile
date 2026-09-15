# 变更准备 Agent：内部后端服务。
#
# 两点刻意的设计：
#   1. 不发布端口。浏览器不直接访问它，只有 ChangeGuard 治理服务通过
#      /api/agent/* 反向代理进来；界面由治理服务同源提供（/agent/）。
#   2. 镜像自带合成语料（examples/agent-demo）。语料是检索的依据来源，
#      不放进镜像的话每次检索都会返回"依据不足"。
#
# 构建上下文是**仓库根目录**，因为语料在 agent-app/ 之外：
#   docker build -f agent-app/Dockerfile -t changeguard-agent .

FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    AGENT_TASK_STORE=/app/data/agent-tasks.json \
    AGENT_DEMO_DIR=/app/corpus

WORKDIR /app

# 依赖只从 pyproject.toml 取，避免 Dockerfile 与本地开发各维护一份清单。
COPY agent-app/pyproject.toml ./
COPY agent-app/app ./app
RUN pip install --no-cache-dir .

COPY examples/agent-demo ./corpus

RUN useradd --system --create-home --uid 10001 agent \
    && mkdir -p /app/data \
    && chown -R agent:agent /app
USER agent

EXPOSE 8091

# healthz 也要求上游凭据（若配置了密钥），所以健康检查必须带上它。
HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=5 \
    CMD python -c "import os,sys,urllib.request; \
req=urllib.request.Request('http://127.0.0.1:8091/api/agent/healthz', headers={'X-Agent-Upstream-Token': os.environ.get('AGENT_UPSTREAM_TOKEN','')}); \
sys.exit(0 if urllib.request.urlopen(req, timeout=3).status == 200 else 1)"

CMD ["python", "-m", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8091"]
