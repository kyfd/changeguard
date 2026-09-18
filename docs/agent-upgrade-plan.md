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

**P1 主体已实现**：`app/workflow/investigate.py` 的受约束调查循环（轮次与累计工具调用双上限、
单工具超时、空转检测、必需证据由代码判定、决策者能力必须显式声明），以
`investigation_strategy` 开关接入，默认仍是 `fixed_workflow` 因而不改变既有行为。

**仍未完成**：模型原生工具选择（provider 未实现 `decide()`，目前只做到"显式报告不可用"
而不是"真让模型选"）、工具结果摘要哈希、usage 缺失策略、`fixed_workflow` 与
`bounded_agent` 的同输入对照评测。

### 2026-09-18：P1 模型原生动作决策完成

`InvestigationPlanner.plan` 改为 **async**；`OpenAICompatibleProvider` 实现 `decide()`，
用原生函数调用真正由模型选择只读工具。白名单 `MODEL_ACTION_TOOLS` 与 `tools/registry.py`
分开声明，并有"白名单 == 注册表只读工具"的一致性断言兜底：注册表新增写工具不会自动让模型
获得调用能力。参数必须过 schema 校验，身份字段由服务端注入、模型参数不得覆盖。

验证（本机实测，详见 `docs/agent-upgrade-verification.md` §11.5–§11.6）：

```
pytest -q                                          183 passed（§11.3 后 161 + 新增 22）
evals/run_eval.py --provider deterministic         11/11
go test ./internal/agent ./internal/httpapi ./internal/service -count=1   三个包 ok
```

**NOT_RUN**：真实模型（live）动作决策与草案质量评测——未配置专用凭据与预算。
新增 22 项用 `httpx.MockTransport` 模拟端点，验证的是契约与边界，**不是模型质量证明**。

**P1 仍未完成**：`fixed_workflow` 与 `bounded_agent` 的同输入对照评测（属 P3 评测范围）。

### 2026-09-18：P2 主体（PR-A）节点级中断与恢复完成

依赖按实际解析版本收紧：`langgraph>=1.2,<2`、新增 `langgraph-checkpoint-sqlite>=3.1,<4`
（实测 langgraph 1.2.11 / checkpoint 4.2.0 / checkpoint-sqlite 3.1.1 / aiosqlite 0.22.1）。
检查点落在磁盘 SQLite（`AsyncSqliteSaver`），非 `InMemorySaver`。

`_check_info` 缺信息时用 `interrupt()` **把图停在节点上**；用户补充后从该节点继续，
中断前的节点不重跑。恢复前重新校验归属（组织 + 创建者）、状态、检查点待办与输入/材料版本；
校验不过返回 409，不做"尽力继续"。启动扫描改为检查点感知（`checkpoint_available` vs
`interrupted_without_resume`），**不自动续跑**。

**任务书 §P2-8（JSON→SQLite 迁移）记为 N/A**：任务仍存 JSON，SQLite 只存检查点。

验证（本机实测，详见 `docs/agent-upgrade-verification.md` §12）：

```
pytest -q                                          193 passed
evals/run_eval.py --provider deterministic         11/11
go test ./internal/agent ./internal/httpapi ./internal/service -count=1   三个包 ok
scripts/recovery_acceptance.py                     13/13（独立进程：中断 → 终止 → 重启 → 恢复 → 完成）
```

下一步：PR-B（材料确认 + 失效 + flaky 修复）、PR-C（评测）、PR-D（工作台 + 浏览器验收）。

### 2026-09-18：P2（PR-B）材料确认与 flaky 修复完成

- **材料确认**：新增 `Confirmation` 与 `POST /tasks/{id}/confirm`（仅创建者）。记录确认人、时间、
  材料版本与内容哈希；同一材料重复确认**幂等**；草案重新生成或输入/材料版本变化时旧确认**失效**
  并保留痕迹。**确认 ≠ 治理审批 ≠ 执行许可**：确认不改变任务状态，也不授予执行权利。
- **flaky 修复（先诊断）**：启动恢复用例的 1ms 租约 + 立即 checkpoint 在 `-race` 下必然偶发
  `ErrConcurrentWrite`。为 `internal/store` 引入可注入时间源（`Store.clock` / `NewMemoryWithClock`），
  两个租约用例改为确定性过期；断言改为有界轮询，`countingRunner.runs` 改为原子计数。
- **顺带修复既有缺陷**（独立 PR #19）：Postgres 通行证重放分支返回空 payload，导致并发消费中
  走重放的一方一直显示 `ACTIVE`；已返回已提交快照，并让多实例用例断言竞争双方都看到 `CONSUMED`。

验证（本机实测，详见 `docs/agent-upgrade-verification.md` §13）：

```
pytest -q                                          201 passed
evals/run_eval.py --provider deterministic         11/11
go test ./... -count=1                             全部包 ok
go test ./internal/service -run TestStartupRecovery… -count=50    ok
go vet ./... / gofmt -l                             clean
npm test                                            2 passed
```

下一步：PR-C（评测 `--strategy`/`--provider` 真正生效、开发/保留集、评分与报告）、PR-D（工作台 + 浏览器验收）。

### 2026-09-18：P3（PR-C）评测运行器完成

`--provider {deterministic,scripted,live}` 现在**真正选择运行时 provider**；`--strategy` 写入
`investigation_strategy`；`--split {dev,holdout,all}` 选择开发集 / 保留集。用例可声明适用范围，
不适用记 `SKIPPED`；live 无凭据记 `NOT_RUN` 并用独立退出码 `2`，不计入通过。

数据集拆为 `evals/datasets/dev.jsonl`（14 例）与 `holdout.jsonl`（4 例，不用于调参），
记录版本与全量 SHA-256。作业覆盖新增**预算耗尽、取消、恢复**。评分：硬性安全断言确定性判定，
另报 `completed` / `correct_refusal` / `incorrect` 与任务完成率（合理拒绝算对）。
报告含数据集哈希、提交、provider、strategy、逐例结果、错误类型、模型请求数、端到端与模型耗时
P50/P95、usage 与未知项；JSON + Markdown，不预填提升比例。

验证（本机实测，详见 `docs/agent-upgrade-verification.md` §14）：

```
scripted + bounded_agent + dev        14/14
scripted + bounded_agent + holdout     4/4
scripted + fixed_workflow + dev       13/13（SKIPPED 1：预算用例只在 bounded_agent 下有意义）
deterministic + bounded_agent + dev    9/9 （SKIPPED 5：需要脚本输出的用例）
live + bounded_agent + dev            NOT_RUN 13，退出码 2（无凭据）
pytest -q                             208 passed
go test ./... -count=1                全部 ok
```

下一步：PR-D（工作台展示与操作、四态区分、真实浏览器验收）。

### 2026-09-18：P3（PR-D）工作台与浏览器验收完成

`TaskView` 新增 `strategy` / `investigation` / `usage`；调查结果把决策者、停止原因、轮次与工具调用、
缺失必需证据、usage 与**工具结果摘要**一并带出。工作台新增「执行轨迹与预算」卡与「材料确认」卡，
追问表单显示问题原文与示例并补齐时区/快照字段；恢复按钮在有检查点时是「从检查点恢复」，
否则才是「重新执行一次」——不把重跑说成续跑。

**四态一眼可分**：模型建议（灰·不参与放行判定）/ 确定性检查（绿红·仅本地静态扫描）/
材料确认（专属紫色·人工确认 ≠ 治理审批 ≠ 执行许可）/ 治理审批（独立一栏·不在此处）。
`DRAFT_READY`「待人工确认」改为专属色，不再与排队中同为蓝色。存储降级与模型不可用现在会显示，
并修正 `refreshHealth` 的 503 判别（先看 `error.code`，不再把落盘抖动误诊为"功能未开"）。

验证（本机实测，详见 `docs/agent-upgrade-verification.md` §15）：

```
pytest -q                                          210 passed
run_eval.py --provider scripted --strategy bounded_agent --split dev   14/14
go test ./... -count=1 / go vet / gofmt            ok / clean / clean
npm test / 递归 node --check                        2 passed / clean
tests/manual/agent-workbench-acceptance.mjs        19/19（真实登录 + 真实前后端）
```

截图随仓库保留：`docs/assets/agent-workbench-desktop.png`、`docs/assets/agent-workbench-narrow.png`。
验收后已停止后台进程，未占用端口。

**P2 与 P3 全部完成**；真实模型（live）评测仍为 `NOT_RUN`（无凭据/预算）。

### 2026-09-18：后续修复（恢复并发竞态、检查点路径隔离、用例挂死）

- **恢复并发竞态（已复现 → 已修复）**：`resume` 在读检查点（await）与占用执行之间不原子，
  并发恢复会各自派发，在同一个 `thread_id` 上并发跑图。修复前复现：`max_overlap=4, accepted=5`；
  修复后：`max_overlap=1, accepted=1, refused=4`（先拒绝"已有在途执行"，再在**无 await** 的同一段代码里
  重新确认代际并占用执行）。
- **检查点路径隔离**：`checkpoint_path` 默认改为与任务存储同目录（`Settings.checkpoint_file`）。
  此前 6 个测试模块回落到仓库里的同一个 `agent-app/data/agent-checkpoints.sqlite`，互相污染状态；
  现在测试不再创建该文件。
- **顺带修掉一处用例挂死**：`test_handle_and_health_after_timeout` 的任务超时为 0.05 s，而全新检查点库下
  走到 provider 需 **488 ms**（文件已存在时 94 ms），于是任务在到达 provider 前就超时，用例却在
  **无界**等待 `entered` → 永久挂死。已改为 2.0 s 超时，并把所有同类等待改为**有界**（15 s）。
- **临时目录权限错误：未复现，暂不判定**（6 次干净全量运行均通过；唯一异常是外部 Ctrl+C 中断，
  未跑任何用例）。需要完整 traceback 与其具体路径才能归因。

验证：`pytest -q` **214 passed**（且 `agent-app/data/` 不再出现）；`evals` 14/14；跨进程恢复 13/13；
`go test ./...` / `go vet` / `gofmt` / `npm test` 全部 clean。
