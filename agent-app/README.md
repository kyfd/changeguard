# ChangeGuard 数据库变更准备 Agent

用户用一句自然语言提需求，Agent **补齐信息、检索规范、生成结构化草案、调用确定性检查并有限次修订**，
最后由人确认材料；**审批与发布仍由原有 Go 治理后端控制**。

## 部署形态：本服务是内部后端

**只有治理服务对外。** 本服务不发布端口、不提供界面：

```
浏览器
    │
    ▼
Go 治理后端（唯一对外入口）      身份认证、组织/应用权限、审批、发布门禁
    ├── GET  /                控制台（原有界面，未改造）
    ├── GET  /agent/          变更准备工作台（三栏，随 Go 二进制发布）
    └── ANY  /api/agent/*     反向代理：注入服务端解析的身份
              │
              ▼
          本服务（Python，内部后端）   需求澄清、工作流、模型调用、检索、结构化草案
              ├── 模型 API
              ├── 规范检索（合成语料）
              └── 经 Go 授权的只读业务工具
```

| 入口 | 地址 | 由谁提供 |
| --- | --- | --- |
| 控制台 | `http://<host>:8080/` | Go（原有） |
| 变更准备工作台 | `http://<host>:8080/agent/` | Go（`internal/httpapi/web/agent/`） |
| 本服务 | **无对外端口** | 只接受治理服务在 `/api/agent/*` 上的代理调用 |

**身份只由治理服务解析。** 界面不发送任何身份头；`X-Actor-Id` / `X-Org-Id` 是治理服务
从**已认证会话**解析后注入的，所以浏览器无法通过改请求头冒充他人。

## 这个服务不做什么

- **不参与放行判定**：模型只给建议（`ai_advice`），能否继续由确定性检查和人工审批决定。
- **不提供写能力**：Agent 可见的 7 个工具全部只读；创建/提交变更仍走原有已认证业务接口。
- **不接生产库**：表结构使用**导入的快照**，不做实时探索；不使用长期业务凭据。
- **不保存思维链**：事件只记录步骤与结论。

## 用 Docker Compose 部署

```powershell
# 治理服务已有的必填项 + 新增的上游共享密钥
$env:CHANGEGUARD_PASSPORT_HMAC_SECRET = "passport-secret-at-least-32-bytes-long"
$env:CHANGEGUARD_AGENT_UPSTREAM_TOKEN = "agent-upstream-secret-at-least-32-bytes"

docker compose up -d --build
# 打开 http://localhost:8080/      控制台
# 打开 http://localhost:8080/agent/ 变更准备工作台
```

两个刻意的设计：

1. **`agent-app` 不在 `dbguard` 的 `depends_on` 里。** Agent 不可用时，治理、审批与发布
   必须照常工作；此时 `/api/agent/*` 会显式返回 502/503，而不是让整套系统起不来。
2. **`agent-app` 不发布端口。** 它只在 Compose 网络内可达，`dbguard` 是唯一调用方。

`CHANGEGUARD_AGENT_UPSTREAM_TOKEN` 与容器里的 `AGENT_UPSTREAM_TOKEN` 必须一致。
留空表示不校验——仅在确认本服务只在内网可达时可接受。

镜像里自带合成语料（`examples/agent-demo`）。语料是检索的依据来源，
不放进镜像则每次检索都会返回"依据不足"。

## 本地开发（不容器化）

```powershell
# 终端 1：Agent 服务（内部后端）
cd agent-app
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -e ".[dev]"
$env:AGENT_ALLOW_HEADER_IDENTITY = "1"        # 身份由治理服务注入，仅本地这样开
$env:AGENT_UPSTREAM_TOKEN = "local-dev-shared-secret"
$env:AGENT_EXECUTION_MODE = "inline"          # 便于观察单步结果
.\.venv\Scripts\python.exe -m uvicorn app.main:app --port 8091

# 终端 2：治理服务（唯一对外入口）
cd ..
$env:CHANGEGUARD_AUTH_MODE = "local"
$env:CHANGEGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
$env:CHANGEGUARD_LISTEN_ADDRESS = "127.0.0.1:8080"
$env:CHANGEGUARD_AGENT_BASE_URL = "http://127.0.0.1:8091"
$env:CHANGEGUARD_AGENT_UPSTREAM_TOKEN = "local-dev-shared-secret"
go run ./cmd/dbguard
```

> 注意：不要同时设置 `CHANGEGUARD_X` 和 `DBGUARD_X`（同一个配置项）。
> 两个品牌前缀值不一致时，治理服务会**失败关闭**并拒绝启动。

**不需要模型也能跑**：未配置 `AGENT_LLM_BASE_URL` / `AGENT_LLM_API_KEY` 时使用确定性生成器，
走的是同一条严格解析路径——这是可运行状态，不是降级后的残缺状态。

### 直接调接口（绕开治理服务，仅限本地）

```powershell
$headers = @{
  "X-Actor-Id" = "alice"; "X-Org-Id" = "org_demo"
  "X-Agent-Upstream-Token" = "local-dev-shared-secret"
}
Invoke-RestMethod http://127.0.0.1:8091/api/agent/healthz -Headers $headers

# 只有一句需求 → 会被追问
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8091/api/agent/tasks -Headers $headers `
  -ContentType 'application/json' -Body '{"requirement":"给订单表按用户和创建时间准备索引变更"}'
```

## 变更准备工作台

位置：治理服务同源提供的 `http://<host>:8080/agent/`，资源随 **Go 二进制**发布
（`internal/httpapi/web/agent/`，纯静态、无构建步骤）。

> 为什么不在本服务里：一个界面只能有一个入口。放两份必然会漂移，
> 而且跨源调用会被治理服务的 `connect-src 'self'` 与 `frame-ancestors 'none'` 拦掉。

三栏对应计划里的分工：

| 栏 | 内容 |
| --- | --- |
| 对话 | 需求输入、可选已知信息、追问表单、进度时间线、停止与重新执行 |
| 草案 | 目标应用与环境、变更 SQL、回滚方案、假设与待确认项、AI 风险建议、修订记录 |
| 证据与检查 | 确定性检查结论与条目、引用证据原文片段、隔离库演练说明、出处与边界 |

界面里有五条**刻意写死**的表述约束，由 `internal/httpapi/agentproxy_test.go` 锁住：

1. `DRAFT_READY` 显示为"草案已生成 · 待人工确认"，并明说**不是审批结论、不会自动提交变更、不代表可在生产执行**；
2. 检查状态 `FAILED` 显示为"检查失败 —— 不得视为通过"（失败不等于没有问题）；
3. 扫描没有产生条目时说明"只能说未命中已知规则，不代表不存在风险"；
4. SQL 一旦被本地修改，右侧检查结论立刻标注为**已失效**（本地编辑不回传服务端，仅供审阅）；
5. AI 建议恒定为"不参与放行判定"。

同一份测试还断言界面**不含** `X-Actor-Id` / `X-Org-Id`——身份不由页面声明，这条用测试钉住。

## 工作流

```
START → screen_input ──命中注入──▶ INPUT_REJECTED（停止）
           │通过
           ▼
      check_info ──缺少必要信息──▶ NEEDS_INFO（追问，不编造）
           │完整
           ▼
   retrieve_evidence → generate_draft → run_check
                            ▲              │
                            └─ 可修订且未达上限 ─┘
                                           │完成 / 无进展 / 达上限
                                           ▼
                                        finalize
```

三条边界：

| 边界 | 实现 |
| --- | --- |
| 有限循环 | `max_revisions` 硬上限；检查结论与上一轮相同视为"无进展"提前停止 |
| 检查与建议分离 | `deterministic_check` 由扫描产生，模型只能提供 `ai_advice` |
| 失败不是通过 | 扫描工具失败时 `check.status = FAILED`，任务状态是 `CHECK_BLOCKED` |

## 草案包含什么

目标应用与环境、原始需求、数据库类型、变更 SQL、回滚方案、假设与待确认项、
引用证据、AI 风险建议、确定性检查结果、草案版本。

字段定义在 `app/schemas/drafts.py`，其中：

- 应用/环境/数据库/计划时间**只从服务端已确认的槽位取**，模型无法改写；
- 未知字段一律拒绝（不静默忽略）；
- 引用的证据 ID 必须真实存在，编造引用直接失败。

## 安全性质

| 性质 | 实现与验证 |
| --- | --- |
| 上游凭据 | 配置 `AGENT_UPSTREAM_TOKEN` 后，**每个** `/api/agent` 路由都要求 `X-Agent-Upstream-Token`（常量时间比较），`test_upstream_token_is_enforced_on_every_agent_route` |
| 身份由服务端解析 | 治理服务从会话取 actor 与组织；调用方自带的身份头被丢弃，`TestAgentProxyDiscardsCallerSuppliedIdentity` |
| 凭据不外泄给下游 | 治理会话 Cookie、`Authorization`、`X-CSRF-Token` 不转发，`TestAgentProxySendsSharedSecretAndStripsGovernanceCredentials` |
| 失败关闭 | 未配置下游时 `/api/agent/*` 返回 503，不静默返回空结果 |
| 身份不可由调用方声明 | `TrustedContext` 由 API 层构造；工具参数里出现 `organization_id` 会被拒绝（`test_identity_cannot_be_supplied_as_argument`） |
| 默认拒绝 | 未开启 `AGENT_ALLOW_HEADER_IDENTITY` 时所有接口返回 401 |
| 工具只读 | 注册表拒绝非只读工具（`test_non_read_only_tool_is_refused`） |
| 工具失败 ≠ 通过 | `test_check_tool_failure_never_becomes_passing` |
| 越权由后端裁决 | 治理后端返回 403 时工具显式失败，不重试、不降级为"没问题" |
| 注入安全停止 | 命中检测直接进入 `INPUT_REJECTED`，不生成草案 |
| 依据不足要说出来 | 检索无命中时 `ok=False`；草案标注"依据检索不完整" |

## 接口

下表是**本服务**的接口。生产部署下它们不直接暴露，统一经治理服务的
`/api/agent/*` 反向代理访问（代理会附加身份与上游凭据）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/agent/healthz` | 健康检查 + provider / 语料规模 / 运行中任务数 |
| GET | `/api/agent/tools` | 工具清单（含 schema 与只读标记） |
| POST | `/api/agent/tasks` | 创建任务（后台执行，返回 `202`） |
| GET | `/api/agent/tasks` | 任务列表 |
| GET | `/api/agent/tasks/{id}` | 任务详情（草案、证据、检查、事件） |
| POST | `/api/agent/tasks/{id}/clarify` | 补充信息并继续 |
| POST | `/api/agent/tasks/{id}/cancel` | 取消任务（真正停止后续工作） |

## 测试与评估

```powershell
.\.venv\Scripts\python.exe -m pytest -q          # 单元与接口测试
.\.venv\Scripts\python.exe evals\run_eval.py     # 离线评估，产出可复现报告
```

界面与代理的安全性由 **Go 侧**测试覆盖：`go test ./internal/httpapi/... -run Agent -v`。

评估集 `evals/datasets/starter.jsonl` 覆盖：正常任务、信息缺失、规范冲突、
工具失败、不可信输入、无依据。报告写入 `evals/reports/`，包含数据集哈希、
provider、逐例结果与**显式列出的局限**。

> 评估结论的边界：离线评估使用确定性或脚本化输出，**不代表真实模型的起草质量**；
> 也未统计 token 与费用。这些必须如实标注，不能拿"测试通过"当质量证据。

## 目录

```
app/
  api/routes.py         HTTP 接口、上游凭据校验、身份解析
  config.py             配置（默认无模型可运行）
  guard.py              提示注入检测
  service.py            编排：后台执行、超时、取消、持久化
  schemas/drafts.py     结构化契约（草案 / 证据 / 检查）
  llm/provider.py       模型接入 + 离线确定性生成器
  tools/registry.py     工具白名单、参数校验、只读约束
  tools/scan.py         确定性 SQL 扫描（检查结果唯一来源）
  tools/business.py     业务工具（快照、检索、治理后端只读调用）
  retrieval/            文档切分、关键词基线、可插拔混合检索
  workflow/state.py     状态与追问
  workflow/graph.py     LangGraph 工作流与有限循环
Dockerfile              内部后端镜像（构建上下文是仓库根目录，以便打包语料）
evals/                  评估集与运行器
tests/                  单元与接口测试
```

界面资源在 Go 侧：`internal/httpapi/web/agent/`。

## 已知限制

- **上游凭据默认留空**（不校验）。生产部署必须设置 `AGENT_UPSTREAM_TOKEN`，
  并确保本服务只在内网可达。
- 身份来自治理服务注入的请求头，**信任边界是"本服务只被治理服务调用"**；
  若本服务被直接暴露，请求头身份就不再可信。
- 任务状态存文件，仅适配单实例；多实例需要外部状态存储。
- 关键词检索对中文只做字符二元组，**无关中文查询仍可能召回弱相关片段**；
  这正是评估要对比"关键词 vs 混合检索"的原因，向量检索接口已预留但未启用。
- 评估集当前 11 例，未拆分开发集/保留测试集。
- 未统计 token 与费用；未覆盖多轮对话与并发场景。
- 界面只做**展示与调用**：没有"确认材料"的落库动作，确认结果未写入任务记录，
  也没有在界面上回写治理后端。
- **任务未按调用方组织隔离**：记录里写了 `organization_id`，但 `GET /tasks`、`GET /tasks/{id}`、
  `clarify`、`cancel` 都只做了认证、没有比对组织，因此任何已认证调用方都能读到或操作他人任务
  （`app/api/routes.py`、`app/service.py`）。这是多租户上线前必须先修的项。
