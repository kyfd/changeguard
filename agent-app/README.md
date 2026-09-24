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
开启头身份模式时密钥必须非空；留空则所有 `/api/agent/*` 返回 503，内网部署也不例外。
这不会阻止核心治理服务启动。本地开发同样配置共享密钥，不提供匿名身份头例外。
Compose 显式将 `AGENT_GOVERNANCE_BASE_URL` 设置为 `http://dbguard:8080`；非容器部署默认使用本机 8080。

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

## 恢复与确认边界

- 工作台提供“我的历史任务”，刷新通过 URL 中的 `task` 标识重新请求后端；浏览器不持久化 SQL、证据或身份，跨用户请求仍由服务端拒绝。
- 材料确认必须携带当前 `material_hash`；缺失或过期均返回 409。本地 SQL 编辑后不能确认服务端原稿，需先恢复原稿。
- 每次模型 HTTP 请求发送前先持久化 `pending` 记录并保守计入预算，返回后原位结算，不重复计数。崩溃或取消后无法结算的记录保留为未知消耗；恢复是 **at-least-once**，并不保证供应商恰好执行一次。
- 账本保留最多 60 条请求明细（含原执行 ID），累计预算不受明细截断影响。旧数据中已漏记的在途请求无法事后重建。
- SQL 扫描新增词法边界检查，区分注释、字符串、引号标识符及子查询深度；它仍不是完整 SQL 语义验证或真实数据库演练。

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
| 只读工具的服务间认证 | 三个远程只读工具走治理后端的**内部只读接口** `/api/agent-tools/changes/{id}`，要求共享密钥 + 成员委托两层认证；未配置密钥时**显式不可用且不发请求**（`test_governance_readonly_auth.py`、Go 侧 `TestAgentTools*`） |
| 任务归属 | 组织与创建者都在**服务/仓储边界**校验，无权与不存在返回同一个 404（`test_authorization.py`） |
| 检索租户隔离 | 带组织标记的语料必须显式授权才可见，默认只能看到公开合成语料（`test_authorization.py`） |
| 落盘诚实性 | **先落盘、成功后才提交内存**；状态文件损坏时失败关闭，不从空库启动（`test_task_lifecycle.py`） |
| 执行所有权 | 取消与重跑都会换掉 `execution_id`，旧执行无法回写；派发失败不留 `RECEIVED` 残骸（`test_task_lifecycle.py`） |
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

### 治理后端为 Agent 提供的内部只读接口

三个远程只读工具**不再**请求面向浏览器的 `/api/changes/{id}`（那条路径在会话中间件下，
本服务没有也不应该持有会话，改造前必然 401）。它们走：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/agent-tools/changes/{id}?projection=context\|findings\|experiment` | 内部只读，窄投影 |

两层认证缺一不可：`X-Agent-Upstream-Token`（服务认证，常量时间比较；未配置时 503 失败关闭）
与 `X-Actor-Id`/`X-Org-Id`（**只是声明**，必须回查成员存在且启用、组织一致，再走应用级授权）。
该入口**不复用会话中间件**，浏览器拿不到共享密钥，因此对它不可达。
实现见 `internal/httpapi/agenttools.go`。

## 测试与评估

```powershell
.\.venv\Scripts\python.exe -m pytest -q          # 单元与接口测试
.\.venv\Scripts\python.exe evals\run_eval.py     # 离线评估，产出可复现报告
```

界面与代理的安全性由 **Go 侧**测试覆盖：
`go test ./internal/httpapi/... -run 'Agent' -v`（含 `TestAgentTools*` 的十项拒绝路径）。

回归测试按关注点分文件：

| 文件 | 锁住的性质 |
| --- | --- |
| `tests/test_authorization.py` | 任务归属（跨组织/同组织他人/旧记录失败关闭）、无权与不存在不可区分、检索租户隔离 |
| `tests/test_task_lifecycle.py` | 落盘诚实性（先落盘后提交内存、损坏失败关闭）、执行所有权与取消竞态、派发失败不留残骸、重启策略 |
| `tests/test_governance_readonly_auth.py` | 内部只读接口的服务间认证：缺密钥不发请求、请求形状、拒绝状态码不变成成功 |

**跨服务验收**（真实启动隔离的 Go 服务 + 合成演示数据，不使用 mock）：
先跑 `go build -o dbguard.exe ./cmd/dbguard` 并以
`PORT` / `DBGUARD_DATA_FILE` / `DBGUARD_ENABLE_DEMO_ACCOUNTS=true` /
`DBGUARD_AGENT_UPSTREAM_TOKEN` 启动，再用带相同密钥的 `Settings` 驱动
`Toolbox` 调用三个只读工具，验证可用性与各条拒绝路径。

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

- **上游凭据留空即失败关闭**：头身份模式下未设置 `AGENT_UPSTREAM_TOKEN` 时，
  所有 `/api/agent/*` 返回 503（带 `SERVICE_UNAVAILABLE` 错误码），内网部署也不例外。
  这是**可接受的核心独立启动状态**：治理、审批与发布完全不受影响，只是 Agent 路由不可用；
  要让 Agent 可用就必须设置 `AGENT_UPSTREAM_TOKEN`，并确保本服务只在内网可达。
  同一密钥也用于本服务→治理服务的内部只读调用；
  未配置时那三个远程只读工具会**显式不可用**（不会退化成匿名读取）。
- 身份来自治理服务注入的请求头，**信任边界是"本服务只被治理服务调用"**；
  若本服务被直接暴露，请求头身份就不再可信。
- 任务状态存文件，仅适配单实例；多实例需要外部状态存储。
- **重启不做安全续跑**：启动时把上次进程遗留的 `RECEIVED`/`RUNNING` 任务显式标为
  中断失败（`restart_policy=interrupted_without_resume`）。真正的节点级恢复需要
  检查点与重新授权，属于后续工作。
- 任务默认**仅创建者可操作**。仓库目前没有组织共享或管理员代管的规范，
  因此没有隐式授予同组织其他用户读写权；将来若引入共享策略，必须在
  `AgentService._authorize` 显式实现。
- 关键词检索对中文只做字符二元组，**无关中文查询仍可能召回弱相关片段**；
  这正是评估要对比"关键词 vs 混合检索"的原因，向量检索接口已预留但未启用。
- 评估集当前 11 例，未拆分开发集/保留测试集。
- 未统计 token 与费用；未覆盖多轮对话与并发场景。
- 界面只做**展示与调用**：没有"确认材料"的落库动作，确认结果未写入任务记录，
  也没有在界面上回写治理后端。
- 任务执行期间**存储状态仍是 `RECEIVED`**（工作流结果在结束时一次性落盘），
  因此进度只能通过 `events` 观察，不能靠状态字段判断"正在跑第几步"。
