# 变更准备 Agent 改造计划与基线核实

本文档是"证据驱动、可恢复、可评测的生产变更准备 Agent"改造的阶段 0 交付物。
它只记录**已核实的事实**：当前真实能力是什么、哪些缺陷已复现、哪些是本轮增量。

**表述纪律**（沿用 `docs/agent-baseline.md` 与 `AGENTS.md`）：

- 已核实的代码事实 ✅ 可以陈述，必须附文件与符号位置；
- 计划新增 ⚠️ 只有做完并跑过测试才能写成已完成；
- 未运行的验证必须显式标注，**跳过不等于通过**；
- 不把"合成数据上的演示"说成"生产验证"。

---

## 1. 基线复跑记录

### 1.1 环境

| 项 | 值 |
| --- | --- |
| 提交 | `e75cf90edaeb7c902a3e1ea44827e9e779f6fcb5` |
| 工作区 | 干净（仅 `.commandcode/`、`tmp-deploy/` 未跟踪，改造前已存在） |
| Python | **3.14.3**（独立 venv：`agent-app/.venv`） |
| Go | 见 `go.mod`（CI 覆盖 1.25 / 1.26） |
| 平台 | Windows |

解析到的关键依赖版本：

```
anyio==4.15.1     fastapi==0.141.1   httpx==0.28.1
langgraph==1.2.11 pydantic==2.13.5   pytest==9.1.1
starlette==1.6.0  uvicorn==0.53.0
```

> **与既有记录的两处不符，必须记下来：**
>
> 1. 本次环境是 **Python 3.14.3**，不是此前记录的 3.12.14。`requires-python = ">=3.11"`，
>    3.14 在允许范围内，但**两个版本上的实测结果不能互相替代**。
> 2. `pyproject.toml` 声明 `langgraph>=0.2`，本次实际解析到 **langgraph 1.2.11**（主版本 1.x）。
>    这直接影响 P2：LangGraph 持久化/中断 API 与 checkpointer 包版本必须匹配 1.x，
>    不能沿用按 0.2 写的示例。**P2 开始前须先收紧依赖范围并重新记录解析版本。**

### 1.2 命令与结果

| 命令 | 结果 |
| --- | --- |
| `python -m pytest -q`（`agent-app/`） | **39 passed**，5.14s |
| `python evals/run_eval.py --provider deterministic` | **11/11 通过**，报告 `evals/reports/20260917-110735-deterministic.json` |
| `go test ./internal/agent ./internal/httpapi ./internal/service -count=1` | 三个包**全部 ok**（0.822s / 3.274s / 3.042s） |

### 1.3 未运行项（不得当作通过）

| 项 | 状态 | 原因 |
| --- | --- | --- |
| `go test -race ./...` | **未运行** | 仅 Linux CI（本机无 C 工具链） |
| `go test ./...`（全仓库） | **未运行** | 本轮基线只跑与改造相关的三个包 |
| PostgreSQL / Redis 集成测试 | **未运行** | 需 `DBGUARD_TEST_POSTGRES_DSN` 等专用测试实例 |
| Playwright 端到端 | **未运行** | 需 Docker Compose 起隔离环境 |
| 真实模型（live）评测 | **未运行** | 未配置专用测试凭据与预算 |
| 跨服务真实联调（Go ↔ Python） | **未运行** | 见 §3.3，该链路的缺陷正是本轮要修的 |

---

## 2. 已有能力（不要重做，改动前先确认）

| 能力 | 位置 |
| --- | --- |
| Go 侧 Agent Loop：原生 tool_calls、轮次上限、证据校验、token 汇总、工具调用记录 | `internal/agent/runtime.go`（`analyzeAgentLoop`、`requiredEvidenceTools`、`validateEvidenceReferencesStrict`） |
| Go 侧 7 个只读工具 + 白名单 + JSON Schema 子集校验 | `internal/agent/tools.go` |
| Go 侧确定性规则引擎与发布闸门 | `internal/checker`、`internal/changegate` |
| Go 侧通行证签发与原子消费、审计哈希链 | `internal/changegate/passport.go`、`internal/audit` |
| Go 侧服务端注入身份的反向代理（丢弃客户端身份头，不转发 Cookie/Authorization） | `internal/httpapi/agentproxy.go` |
| Python 7 个只读工具 + 白名单 + `additionalProperties:false` 严格参数校验 | `app/tools/business.py`、`app/tools/registry.py` |
| Python 严格草案解析：未知字段拒绝、伪造 evidence_id 拒绝、槽位只从服务端取 | `app/workflow/graph.py`（`parse_model_draft`） |
| Python 有限修订闭环 + 无进展提前停 + `CHECK_BLOCKED` 与 `DRAFT_READY` 区分 | `app/workflow/graph.py` |
| Python 工具失败绝不等同于通过（`check.status=FAILED`） | `app/workflow/graph.py`（`_run_check`） |
| Python 输入注入检测（命中即 `INPUT_REJECTED`） | `app/guard.py`、`app/workflow/graph.py`（`_screen_input`） |
| 上游共享密钥校验（常量时间比较） | `app/api/routes.py`（`verify_upstream`） |
| 离线评估运行器（数据集哈希、逐例结果、显式 LIMITATIONS） | `agent-app/evals/run_eval.py` |

**同时要避免的错误说法**（已核实为不成立）：

- `DraftWorkflow` 固定调用规范检索、历史案例检索与 SQL 扫描 —— **工具注册不等于模型自主选用工具**；
- 已有 NEEDS_INFO / clarify，但补充后**重建初始状态**；`builder.compile()` **未配置 checkpointer**，
  不等于节点级恢复；
- 离线评估是 11 例 deterministic/scripted 回归，**不是真实模型质量证明**；
- `HybridRetriever` 的可选向量打分**只重排关键词候选**，不是独立向量召回。

---

## 3. 缺陷待复现（复现 → 修复 → 回归）

### 3.1 任务授权

| # | 缺陷 | 证据 |
| --- | --- | --- |
| B1 | `list/get/clarify/cancel` 调用了 `resolve_context(request)` 但**丢弃返回值**，未传给 service（对照 `create_task` 正确传入） | `app/api/routes.py` |
| B2 | 四个读/写操作只比对任务**存在性**，不比对组织与创建者 → 跨组织可读写 | `app/service.py`（`_require`、`list_tasks`） |
| B10 | `Chunk.organization_id` 字段存在，但 `search` **只按 doc_id 前缀过滤，从不比对组织**；`load_corpus` 也未传组织 → 检索层没有租户隔离 | `app/retrieval/keyword.py`、`app/retrieval/corpus.py` |

### 3.2 取消、重跑与持久化

| # | 缺陷 | 证据 |
| --- | --- | --- |
| B3 | `_execute` 的 `finally` **无条件 save**；`cancel()` 已写 CANCELLED，被取消的协程仍会写回 → 旧协程覆盖新状态 | `app/service.py` |
| B4 | `save()` **先改内存再落盘**；`_persist()` 抛错时内存快照已被污染 | `app/store/tasks.py` |
| B5 | `_load` 捕获 `JSONDecodeError / OSError` 后直接 `return` → **损坏状态静默当成空库** | `app/store/tasks.py` |
| C7 | `if not str(self._path): return` 是死代码（`str(Path)` 永不为空），该分支从未被测 | `app/store/tasks.py` |
| C8 | `create_task` 先 `save` 再 `_dispatch`；派发抛错则任务永远停在 `RECEIVED`，无超时兜底 | `app/service.py` |

### 3.3 Python → Go 只读工具认证

| # | 缺陷 | 证据 |
| --- | --- | --- |
| B11 | `Toolbox._fetch_change` 只发 `X-Actor-Id` / `X-Org-Id` 请求 `GET /api/changes/{id}`，而该路径在会话中间件下 → 真实部署下**三个远程只读工具返回 401**；代码却按 403 处理并注释为"后端按组织的权限检查在这里生效"。Go 代理又刻意不转发会话凭据 | `app/tools/business.py`、`internal/httpapi/agentproxy.go` |

### 3.4 本轮额外发现（规格未提及）

| # | 缺陷 | 证据 | 影响 |
| --- | --- | --- | --- |
| C1 | **两层重试相乘**：provider 内层按 `llm_max_attempts` 重试，`_generate_draft` 又用**同一配置**再套一层 → 单轮最多 4 次模型调用 | `app/llm/provider.py`、`app/workflow/graph.py` | 成本与延迟放大 |
| C2 | **模型可自封"已确认"**：`confirmed=bool(item.get("confirmed", False))` 直接采信模型输出；`DeterministicProvider` 自己就产出 `"confirmed": True` | `app/workflow/graph.py`、`app/llm/provider.py` | 违反"模型不能自行把假设标成已获人工确认" |
| C3 | **草案版本恒为 1**：硬编码 `version=1`；`Draft.revision_notes` 字段存在但**从未被写入** | `app/workflow/graph.py`、`app/schemas/drafts.py` | version / revision_notes 与实际修订不对应 |
| C4 | **`ClarifyRequest.note` 被静默丢弃**：schema 有该字段，`_merge_slots` 不读，`clarify()` 也不回报 | `app/schemas/drafts.py`、`app/service.py` | 用户自由文本无声消失 |
| C5 | **无法通过 clarify 补 `schema_snapshot`**：`ClarifyRequest` 无该字段，`TaskSlots.missing()` 也不含它 | `app/schemas/drafts.py` | 缺快照时无法走补充路径 |
| C6 | **非 PostgreSQL 会拿到 PG 专属语法**：无条件产出 `CREATE INDEX CONCURRENTLY` / `SET lock_timeout`，MySQL 请求既不拒绝也不降级 | `app/llm/provider.py`、`app/tools/scan.py` | 明令禁止的"展示成成功" |
| C9 | `WorkflowState.cancelled` 声明但无人写入 | `app/workflow/state.py` | 状态契约与实际不一致 |
| C10 | conftest 夹具不一致：`settings` 开 `allow_header_identity=True`，`empty_settings` 未开 | `agent-app/tests/conftest.py` | 可能掩盖身份配置差异 |

---

## 4. 本轮增量

| 阶段 | 增量 | 对应缺陷 |
| --- | --- | --- |
| **P0** | 服务/仓储边界授权；执行所有权与持久化诚实性；Python→Go 只读取数认证 | B1、B2、B10；B3、B4、B5、C7、C8；B11 |
| **P1** | 受约束调查循环取代固定脚本；provider 决策契约；预算与上下文 | B7、C1、C2、C3、C4、C5、C6、C9 |
| **P2** | LangGraph checkpointer + 进程重启恢复；材料确认记录 | B6 |
| **P3** | 真正可选的评测模式；开发/保留集拆分；可观测性与 `/agent/` 工作台 | B8 |

**不在本轮范围**：多 Agent 团队、自进化 Prompt、MCP Server、长期用户画像、向量数据库、
通用插件平台、多副本任务调度。可写后续 ADR，但不得抢占本轮闭环。

---

## 5. 不可破坏的约束

- Go 保持组织/应用授权、静态规则、审批、制品摘要、通行证签发与原子消费的权威来源。
- 模型与模型可见工具不得审批、签发/消费通行证、执行 SQL、部署、回滚或升级。
- 表结构来自显式导入的快照，不接生产库，不获取长期生产凭据。
- 身份只能来自服务端认证上下文；请求体或自报应用名不能授予访问权限。
- `NOT_RUN`、`DEMO_ONLY`、执行失败与真实验证严格区分；扫描未命中不代表生产安全。
- 模型/检索/外部工具输出均为不可信数据；正则或 untrusted 标签只是辅助，不是完整安全保证。
- trace 记录动作、工具结果摘要、证据与错误；不记录隐式思维链，不记录密钥、Cookie 或完整敏感数据。
- 无模型模式仍可运行，但必须显式标注 deterministic/scripted；真实模型失败不得偷偷换离线结果并计为成功。

---

## 6. 进度记录

### 2026-09-17：阶段 0 完成

- 复跑基线：39 passed / 11-11 / Go 三个包 ok（见 §1.2）。
- 逐行核实规格断言，11 条全部成立（§3.1–§3.3）。
- 额外发现 10 处规格未提及的缺陷（§3.4）。
- 记录两处与既有记录不符的环境事实（Python 3.14.3、langgraph 1.2.11）。

下一步：P0 实施（复现 → 修复 → 回归）。

### 2026-09-17：P0 完成

| 项 | 状态 | 证据 |
| --- | --- | --- |
| P0-1 任务授权（B1、B2、B10） | ✅ 完成 | 原始代码上 14 项新用例全部失败（含"不同组织检索到他人私有语料"）；修复后通过 |
| P0-2 执行所有权与持久化（B3、B4、B5、C7、C8） | ✅ 完成 | 改前/改后对照脚本：内存领先磁盘、未落盘记录可读、损坏静默清空、RUNNING 永久卡住，四项均已修正 |
| P0-3 只读工具服务间认证（B11） | ✅ 完成 | 真实跨进程验收 10/10（旧路径实测 401，新接口实测可用，伪造凭据/跨组织实测被拒） |

验证结果（详见 `docs/agent-upgrade-verification.md`）：

```
gofmt -l ./internal ./cmd    clean
go vet ./...                 clean
go test ./... -count=1       全部包 ok
pytest -q                    91 passed（基线 39 + 新增 52）
evals 11/11                  通过
node --check / npm test      clean / 2 passed
```

顺带修正的既有语义错误：`chunk_markdown` 把文档「适用范围」误当作 `organization_id`。

**本机无法运行、已由 CI 覆盖**（PR #14，run `35216812259`，全部 pass）：
`go test -race ./...`（quality-go 1.25/1.26）、PostgreSQL/Redis 集成测试、Playwright 端到端。

**至今未运行**（不得当作通过）：含 agent-app 的完整 `docker compose up --build`、
真实模型质量、真实浏览器交互与截图。

**未按"已复现"表述**：B3 是竞态，其确定性复现依赖本轮才引入的所有权原语；
原始代码上相关用例根本无法被收集。不要把它说成"已复现出失败"。

下一步：P1（受约束调查循环 + provider 决策契约 + 预算与上下文，
含已发现的 C1 重试相乘、C2 模型自封"已确认"、C3 版本恒为 1、C4/C5 澄清字段、C6 方言）。

### 2026-09-17：P1 前置修复完成，主体未开始

C1 重试相乘、C2 模型自封"已确认"、C3 版本/修订说明、C4/C5 补充信息字段、C6 方言边界
已修复并有改前/改后证据（改前分别为 4 failed 与 10 failed，全部为行为复现）。
`pytest -q` 125 passed、离线评估 11/11。详见 `docs/agent-upgrade-verification.md` §11。

**P1 主体仍未开始**：受约束调查循环、预算（最大轮次/累计工具调用/单工具超时/任务总时限/输出预算）、
空转检测、模型自主工具选择的决策契约、必需证据的确定性完成条件、工具结果结构化与摘要哈希、
usage 缺失策略、`fixed_workflow`/`bounded_agent` 策略开关。不要把这些读成已完成。
