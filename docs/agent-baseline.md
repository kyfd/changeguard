# Agent 改造基线（阶段 0）

本文是"数据库变更准备 Agent"改造的**阶段 0 交付物**。它只记录事实：
当前真实能力是什么、哪些是计划新增、哪些是模拟输入、哪些是真实执行结果。

**核心原则：这四类东西不能混着说。** 面试和文档里混说，是这类项目最常见的失真来源。

---

## 1. 基线（2026-09-15）

### Go 测试

```powershell
go test ./... -count=1
```

结果：**22 个包全部 ok**，无 FAIL。没有测试文件的包：
`cmd/changeguard-agent-eval`、`cmd/changeguard-agent-gateway`、`internal/model`。

### Web 测试

```powershell
npm test
```

结果：**2 项通过，0 失败，0 跳过**。

### 未运行项（必须显式标注，不能当成通过）

| 项 | 状态 | 原因 |
| --- | --- | --- |
| `go test -race ./...` | **未运行** | Windows 本机无 C 工具链（`gcc` missing），race 需在 Linux CI 运行 |
| PostgreSQL 集成测试 | **未运行** | 未设置 `DBGUARD_TEST_POSTGRES_DSN`，测试显式跳过 |
| Redis 会话集成测试 | **未运行** | 未设置 `DBGUARD_REDIS_TEST_URL`，测试显式跳过 |
| Playwright 端到端 | **未运行** | 需要 Docker 起 Compose 环境 |

> **跳过不等于通过。** 上面四项在文档和演示里只能表述为"未验证"。

---

## 2. 当前真实能力：现有 Go Agent

### 入口

| 位置 | 说明 |
| --- | --- |
| `POST /api/changes/{id}/agent-ask` | 针对**某个已存在的变更**提问 |
| `GET /api/changes/{id}/agent-conversations` | 读取历史对话 |
| `GET /api/changes/{id}/agent-conversations/{conversationID}` | 读取单次对话 |

### 工具清单（全部只读）

`internal/agent/tools.go` 中的 `DefaultToolRegistry()` 注册了 7 个工具：

| 工具 | 作用 | 参数 |
| --- | --- | --- |
| `get_rule_findings` | 读取确定性规则命中项（含阻断级别与建议） | 无 |
| `get_experiment_report` | 读取预发布验证、数据库演练与回滚证据 | 无 |
| `get_change_context` | 读取变更元数据与制品摘要 | 无 |
| `query_policies` | 查询企业风险策略库并对当前 SQL/制品做模式命中预览 | 无 |
| `scan_sql` | 对变更 SQL 做本地静态风险扫描（不调用模型） | 无 |
| `search_historical_changes` | 检索同企业/同应用历史变更 | `limit` (1–20) |
| `get_service_topology` | 获取应用依赖拓扑与运行环境 | 无 |

工具安全机制（可复用，不要重写）：

- `DataSource` 接口（`tools.go:16-20`）**结构上没有任何写方法**；
- 工具白名单：未注册的名字直接拒绝；
- `validateToolArguments` 实现 JSON Schema 子集校验（对象、`additionalProperties`、整数上下界）；
- 工具返回的文本标记为不可信（`description_untrusted` 等），不作为指令执行。

### 现有 Agent 的**关键限制**

> **所有工具都作用在"已经存在的变更"上（change-scoped）。**
> 没有"从一句自然语言需求起步、先补信息再生成草案"的能力。

这正是本次改造要补的核心缺口——不是"再加几个工具"，而是**新增一个需求侧的入口与状态机**。

---

## 3. 四类能力必须分开表述（阶段 0 验收项）

| 类别 | 具体内容 | 能不能宣称 |
| --- | --- | --- |
| **当前真实能力** | 变更级只读问答；7 个只读工具；确定性规则检查；影子库演练（需 `技术负责人` 触发）；通行证签发与原子消费；审计链 | ✅ 可以，但要说清前提 |
| **计划新增能力**（首版） | 需求澄清、规范检索（RAG）、结构化草案生成、静态检查驱动的有限次修订、材料确认界面 | ⚠️ 只有做完才能写进简历 |
| **模拟输入** | 合成订单表结构、合成慢查询 SQL、合成变更规范与历史案例（`examples/agent-demo/`） | ✅ 但必须标注"合成数据，非生产" |
| **真实执行结果** | `go test`／`npm test` 的实际输出；后续评估报告里的实测数字 | ✅ 可以，需附版本与日期 |

**不能做的事**：

- 把"合成数据上的演示"说成"生产验证"；
- 把"计划新增"写进简历；
- 把 `NOT_RUN` / `DEMO_ONLY` 说成"验证通过"（这是仓库既有红线，见 `AGENTS.md`）。

---

## 4. 与四阶段计划的对照

### 保留（已符合，不要重做）

| 计划要求 | 仓库现状 |
| --- | --- |
| Go 后端继续负责权限、审批、发布门禁 | 已有：`internal/service` 状态机、`canUseApplication` 授权、通行证签发与消费 |
| 模型工具只读 | 已有：`DataSource` 结构上无写方法，工具白名单 + schema 校验 |
| 不接收长期生产凭据 | 已有：影子库 DSN 与主库 DSN 分离校验，模型侧无数据库连接 |
| 确定性检查结果与模型建议分开 | 已有：`AgentAnalysis.AdvisoryRisk` 明确"不参与放行"（`model.go:193-195`） |
| 输出区分 `NOT_RUN` / `DEMO_ONLY` / 真实演练 | 已有：`compactExperiment` 返回 `status`，`trustedSQLExperiment` 四条件校验 |
| 不重写 Go 核心 | 符合：本次改造只新增 Python 服务与只读工具调用 |

### 修改（存在过度表述或实现缺陷）

| 位置 | 问题 | 处理 |
| --- | --- | --- |
| `docs/adr/0002` 引言 | "the model has exactly one write capability" 与首版"模型工具只读、由确定性编排写入"冲突 | 已标注为延后提案并说明边界 |
| `docs/adr/0002` 第 25–28 行 | "No external side effect" 不成立：`QueueExperiment` 会在隔离 PostgreSQL 影子库**真实执行 SQL** | 已在 ADR 顶部修正 |
| `docs/adr/0002` 第 28 行 | "A wrong draft is a discardable row. The remedy is deletion" —— **仓库没有删除变更的接口**（已检索 `DeleteChange` / `DELETE`，0 命中） | 已在 ADR 顶部修正 |
| `docs/adr/0002` 第 38 行 | "the agent needs the `submit` capability and nothing else" 与第 140 行的自述冲突（`QueueExperiment` 要求 `技术负责人`） | 已在 ADR 顶部修正 |
| `docs/adr/0002` 第 53 行 / 设计文档 Stage 1 | "`VerifyGate(consume=true)` 按 actor 类型拒绝" 在实现层落不了地：`VerifyGate` 里的 actor 是合成的 `model.User{ID: "ci:" + consumer, Role: "CI"}`（`passport.go:211`），**该路径没有会话 actor 可检查** | 已在两处标注，需先决定是否引入 Gate 调用方身份 |
| `docs/agent-submission-design.md` | 整篇按"首版实现"阅读 | 已加状态横幅，标注为延后提案 |

### 新增（首版要做）

| 模块 | 说明 |
| --- | --- |
| Python Agent 服务（`agent-app/`） | FastAPI + 结构化输入输出 + 任务状态 + 追问 + 限额 |
| 需求侧状态机 | 接收需求 → 检查必要信息 → 补齐 → 读取证据 → 生成草案 → 静态检查 → 有限修订 → 输出 |
| 规范检索（RAG） | 关键词基线 + 向量检索对比；片段级引用可追溯 |
| 表结构快照工具 | 首版用上传/导入的快照，**不接生产库实时探索** |
| 评估集与报告（`evals/agent/`） | 30 → 80 个案例；三组对照；指标与失败样例 |
| 材料确认界面 | ✅ 已交付，但**与计划的实现方式不同**：计划写"复用现有 Web"，实际做成由 Python 服务自己托管的独立静态页 `agent-app/app/static/`（`/ui`）。原因见下 |

---

## 5. 两个必须先决定的冲突

### 冲突一：`agentflow`（Go 新项目）与 Python Agent 服务的关系

当前工作区里已有一个我搭的 Go 项目 `agentflow/`（带人工闸门的工作流服务，独立仓库）。
本计划要求的是 **Python + FastAPI** 的 Agent 服务，接入现有 Go 治理后端。

**两者定位重叠，同时作为简历主线违反计划里"不为了简历同时接多个 Agent"的约束。**

| 方案 | 说明 |
| --- | --- |
| A（推荐） | `agentflow` 保留为**独立练习仓库**，不作简历主线；简历主线是"ChangeGuard + Python Agent 服务" |
| B | 把 `agentflow` 里的编排思路（计划→闸门→审计）**迁移**到 Python 服务，`agentflow` 归档 |
| C | 放弃 Python 服务，用 `agentflow` 承担（**不推荐**：偏离 AI 应用岗主流栈，且要重写治理对接） |

### 冲突二："说一句话后自动推进到待审批"何时做

计划的判断正确：它会引入专用服务账号、可信发起人归属、创建幂等、自审拦截、
失败恢复、审批绑定最终制品等一系列工程，**量级大于模型编排本身**。

**首版不做。** 首版边界保持：模型工具只读，写操作由确定性编排在**已认证业务接口**上执行。

---

## 6. 阶段 0 完成标准自查

| 验收项 | 状态 |
| --- | --- |
| 跑现有 Go、Web 测试并记录通过/失败/跳过 | ✅ 本文第 1 节 |
| 梳理现有 Agent 工具与对话入口 | ✅ 本文第 2 节 |
| 准备合成数据（表结构、查询、规范、历史案例） | ✅ `examples/agent-demo/` |
| 明确演示场景，不使用真实业务数据 | ✅ 合成数据已标注来源与用途 |
| 整理两份自动代提文档，修正过度表述 | ✅ ADR 0002 与设计文档已加状态与修正说明 |

---

## 7. 下一步（阶段 1，预算 3–4 天）

最小 Agent 闭环，**先不加 RAG**：

```text
接收需求 → 检查必要信息 ──缺失──▶ 追问
         → 读取证据
         → 生成结构化草案
         → 调用静态检查
         → 有限次修订（或达上限停止）
         → 输出草案 + 未解决问题
```

首版验收：能追问而不编造表结构；模型返回非法 JSON 有明确错误或有限重试；
达到轮数上限会停止而不是无限循环。

---

## 8. 进度记录

### 2026-09-15：阶段 1 完成，阶段 2/3 打通基础

新增 Python 服务 `agent-app/`（FastAPI + Pydantic + LangGraph），已实现：

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 1 | 最小闭环：状态保存、追问、工具参数校验、最大轮数、超时、取消 | ✅ 完成 |
| 2 | 真实业务工具层：工具白名单、可信上下文注入、治理后端只读调用 | ✅ 完成（后端未启动时显式失败） |
| 3 | 检索：Markdown 切分 + 关键词基线 + 可插拔向量接口 | ✅ 基线完成，向量未启用 |
| 5 | 离线评估：11 例数据集 + 运行器 + 可复现报告 | ✅ 起始版本 |

验证结果：

```
pytest            33 项通过
离线评估          11/11 通过
服务冒烟          healthz ok / 27 个语料片段 / 追问 5 项
```

### 2026-09-15：阶段 4 材料确认界面交付

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 4 | 对话 / 草案 / 证据三栏材料确认界面 | ✅ 交付 |

实现：`agent-app/app/static/`（`index.html` + `styles.css` + `app.js`），
由 Python 服务在 `/ui` 下静态托管，`/` 跳转过去；无构建步骤，不引入前端框架依赖。

**与计划的偏离（必须记录）**：计划原文是"复用现有 Web，新增对话／草案／证据三栏"。
实际没有改动 ChangeGuard 的内嵌前端（`internal/httpapi/web/` 为 `go:embed` 资产，改动需重建 Go 二进制，
且要新增到 Python 服务的代理或 CORS 配置），而是在 Python 服务内做了独立静态页。
代价是**没有与现有控制台的导航整合**；收益是**没有触碰 Go 核心**。
（该偏离已在下一节修正为单一入口。）

界面里写死的五条表述约束（`tests/test_ui.py` 锁住）：

1. `DRAFT_READY` → "草案已生成 · 待人工确认"，并明说不是审批结论、不会自动提交变更、不代表可在生产执行；
2. 检查 `FAILED` → "检查失败 —— 不得视为通过"；
3. 扫描零条目 → "只能说未命中已知规则，不代表不存在风险"；
4. SQL 被本地修改 → 右侧检查结论立即标注"已失效"（本地编辑不回传服务端）；
5. AI 建议恒定标注"不参与放行判定"。

另有一组**界面契约测试**：断言界面读取的字段（`provider.provider`、`knowledge_chunks`、
`draft.deterministic_check`、`draft.evidence` 等）在接口返回里真实存在——少键不会报错，只会静默显示 `undefined`。

验证结果：

```
pytest            46 项通过（其中界面相关 13 项）
服务冒烟          GET / → 307 /ui/ ；/ui/ 200 ；app.js 200(31.9KB) ；styles.css 200(12.7KB)
数据链路          DRAFT_READY v1 / 修订 1 次 / 检查 PASSED 0 阻断 / 证据 6 条 / 建议 MEDIUM / 未解决问题 3 项
```

**未验证项**（不得当作通过）：

- 未在真实浏览器里做过交互验证（没有前端端到端测试，也没有截图证据）；
- ~~界面未接入真实会话认证，仅支持演示用请求头身份~~ —— 该限制已在下一节
  "合并为单一入口"中消除：界面改用治理服务的会话，身份由服务端解析后注入；
- 真实模型起草质量、多实例一致性、真实治理后端的端到端联调，仍未验证。

**本轮顺带发现的既有缺陷**（未修，已记入 `agent-app/README.md`）：
任务未按调用方组织隔离——`GET /tasks`、`GET /tasks/{id}`、`clarify`、`cancel`
都只认证不比对组织，任何已认证调用方都能读到或操作他人任务。

### 2026-09-15：合并为单一入口

上一轮把工作台做成了 Python 服务自托管的 `/ui`，结果是**两个界面、两个端口**。
本轮并成**一个入口**：

| 变化 | 之前 | 现在 |
| --- | --- | --- |
| 工作台位置 | `agent-app/app/static/`，Python 在 `/ui` 提供 | `internal/httpapi/web/agent/`，随 Go 二进制发布，挂在 `/agent/` |
| Agent 接口 | 浏览器直连 `127.0.0.1:8091` | 经 Go 在 `/api/agent/*` 反向代理 |
| 身份来源 | 页面自己填 `X-Actor-Id` / `X-Org-Id` | Go 从**已认证会话**解析后注入；页面不含身份头 |
| 调用方认证 | 无 | `DBGUARD_AGENT_UPSTREAM_TOKEN` ↔ `AGENT_UPSTREAM_TOKEN` 共享密钥 |
| Python 服务 | 对外提供界面 | 纯内部后端：无 `/ui`、不发布端口 |

三条约束来自代码事实，不是风格选择：

1. **iframe 方案不可行**：安全头是 `X-Frame-Options: DENY` 与 `frame-ancestors 'none'`。
2. **跨源直连不可行**：CSP 是 `connect-src 'self'`。
3. **身份不能由浏览器声明**：那等于任何登录用户都能冒充他人，必须由 Go 在服务端解析后注入。

过程中发现并修掉一个**真实退化**：根静态处理器用 `fs.Stat(staticFS, "agent/")` 判断路径，
而带尾斜杠的路径对 `fs.ValidPath` 非法，判断必然失败，于是 `/agent/` 被当成未知页面
**静默回退成控制台首页**。现在 `/agent/` 有独立处理器，并由测试钉住
（`TestAgentWorkbenchIsServedFromTheSameOrigin`）。

验证结果：

```
go test ./... -count=1      22 个包全部 ok（含新增 8 项代理／工作台测试）
pytest                      39 项通过（移除 13 项界面测试，新增 6 项上游凭据测试）
compose config              通过；解析确认 agent-app 无 published 端口
端到端（本机 8080 + 8091）   /agent/ 200 且是工作台不是控制台；未登录 /api/agent/* 401；
                            登录后经代理 healthz ok；创建任务 NEEDS_INFO（5 项追问）
身份伪造验证                请求带 X-Org-Id=org_attacker / X-Actor-Id=usr_attacker，
                            下游实际收到 org_demo / usr_developer
```

**现已由 CI 覆盖**（`quality-agent` 作业，随 PR #11 加入）：

原先 agent 侧的 pytest 与镜像构建**完全没有自动化覆盖**，是本轮补上的：

| 覆盖项 | 之前 | 现在 |
| --- | --- | --- |
| agent-app 的 pytest | 从未在 CI 运行（39 项） | `quality-agent` 作业运行，实测 `39 passed in 0.96s` |
| `agent-app/Dockerfile` 构建 | 从未在任何地方构建过 | `docker build -f agent-app/Dockerfile .`，CI 中通过 |
| `internal/httpapi/web/agent/*.js` 语法 | 单层 glob 漏掉子目录 | `quality-js` 改为递归检查 |

这些检查挂在聚合作业 `quality` 下，因此**自动成为 main 规则集要求的检查**，
无需修改规则集配置。口径是：镜像"能构建"已被证明，而不是只做过静态审查。

**仍未验证项**（不得当作通过）：

- **`docker compose up --build` 完整编排未跑过**：镜像单独构建已验证，但五个服务
  （primary/shadow Postgres、Redis、dbguard、agent-app）一起起来的编排没跑过。
  CI 的 `e2e` 用的是**独立的** `compose.e2e.yml`，其中不含 `agent-app`。
- Agent 路径的端到端只在**文件存储 + 内存会话**下验证过（本地 8080 + 8091 手工跑通）。
- 真实模型起草质量、多实例一致性仍未验证。

架构与边界说明见 `docs/agent-architecture.md`，服务说明见 `agent-app/README.md`。

### 2026-09-17：P0 安全与生命周期基础

按 `docs/agent-upgrade-plan.md` 的阶段划分完成 P0（授权、执行所有权与持久化诚实性、
只读工具服务间认证）。完整验证记录见 `docs/agent-upgrade-verification.md`。

本节只记要点，避免与验证文档出现两处会漂移的副本：

| 变化 | 之前 | 现在 |
| --- | --- | --- |
| 任务读写授权 | 只认证，任何已认证调用方都能读写他人任务 | 服务/仓储边界按组织 + 创建者校验；无权与不存在返回同一个 404 |
| 检索可见性 | 带组织标记的语料对所有人可见 | 必须显式授权；默认只能看到公开合成语料 |
| 任务落盘 | 先改内存再落盘；损坏文件静默当空库 | 先落盘后提交内存；损坏时失败关闭 |
| 取消与重跑 | 无执行所有权，旧协程可回写 | `execution_id` / `run_generation` 栅栏 |
| 进程重启 | 在途任务永远停在 `RECEIVED`/`RUNNING` | 显式标为中断失败，`restart_policy=interrupted_without_resume` |
| 远程只读工具 | 请求需会话的 `/api/changes/{id}`，实测 401 | 走内部只读接口，共享密钥 + 成员委托两层认证 |

```
pytest -q        91 passed（基线 39 + 新增 52）
go test ./...    全部包 ok
go vet ./...     clean
gofmt            clean
evals            11/11
npm test         2 passed
```

**未验证项**（不得当作通过）：`go test -race`（仅 Linux CI）、PostgreSQL/Redis 集成、
Playwright 端到端、真实模型质量、真实浏览器交互。

**一句必须说清的话**：P0 只修掉了安全与生命周期缺陷；"重启后安全续跑"**没有**实现，
在途任务会被标为失败。不要把它表述为"已支持断点恢复"。
