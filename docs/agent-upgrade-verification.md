# 变更准备 Agent 改造验证记录（P0）

本文记录**已经实际执行**的内容及其结果。口径沿用 `docs/agent-baseline.md` 与 `AGENTS.md`：

- 只写跑过的命令与真实输出；
- `NOT_RUN` / 跳过项必须显式标注，**跳过不等于通过**；
- 不把计划中的能力写成已完成；
- 不编造在线流量、准确率或延迟数字。

**本轮范围：P0（安全与生命周期基础）。P1–P3 尚未开始**，本文末尾列出剩余任务。

---

## 1. 环境与提交

| 项 | 值 |
| --- | --- |
| 基线提交 | `e75cf90edaeb7c902a3e1ea44827e9e779f6fcb5` |
| 本轮改动 | 未提交（工作区改动，见 §3） |
| Python | 3.14.3（`agent-app/.venv`） |
| Go | 见 `go.mod`（本机 `go1.26.3`） |
| 平台 | Windows |

解析到的关键依赖（供后续版本收紧参考）：

```
anyio==4.15.1     fastapi==0.141.1   httpx==0.28.1
langgraph==1.2.11 pydantic==2.13.5   pytest==9.1.1
starlette==1.6.0  uvicorn==0.53.0
```

> **与既有记录不符，已记录**：本次环境是 **Python 3.14.3**（既有记录为 3.12.14），
> `langgraph` 实际解析到 **1.2.11**（`pyproject.toml` 只写了 `>=0.2`）。
> 两个版本上的实测结果不能互相替代。

---

## 2. 实测命令与结果

| 命令 | 结果 |
| --- | --- |
| `gofmt -l ./internal ./cmd` | **clean** |
| `go vet ./...` | **clean** |
| `go test ./... -count=1` | **全部包 ok**（23 个有测试的包，无 FAIL） |
| `python -m pytest -q`（`agent-app/`） | **91 passed**（基线 39 + 新增 52） |
| `python evals/run_eval.py --provider deterministic` | **11/11 通过** → `evals/reports/20260917-113001-deterministic.json` |
| 递归 `node --check`（`internal/httpapi/web/**/*.{js,mjs}`） | **clean** |
| `npm test` | **2 passed，0 failed** |

新增回归用例：

| 位置 | 数量 | 覆盖 |
| --- | --- | --- |
| `agent-app/tests/test_authorization.py` | 14 | 任务归属、无权与不存在不可区分、旧记录失败关闭、检索租户隔离 |
| `agent-app/tests/test_task_lifecycle.py` | 27 | 落盘诚实性、损坏失败关闭、执行所有权与取消竞态、派发残骸、重启策略 |
| `agent-app/tests/test_governance_readonly_auth.py` | 11 | 内部只读接口的服务间认证与请求形状 |
| `internal/httpapi/agenttools_test.go` | 10 | 内部只读接口的服务认证、成员委托、组织/应用授权、通路隔离、窄投影 |

### 未运行项（不得当作通过）

| 项 | 状态 | 原因 |
| --- | --- | --- |
| `go test -race ./...` | **未运行** | 仅 Linux CI（本机无 C 工具链） |
| PostgreSQL / Redis 集成测试 | **未运行** | 需专用测试实例与 DSN |
| Playwright 端到端 | **未运行** | 需 Docker Compose |
| `docker compose up --build` 完整编排 | **未运行** | 本轮未改动编排文件 |
| 真实模型（live）评测 | **未运行** | 未配置专用测试凭据与预算 |
| 真实浏览器交互与截图 | **未运行** | P0 未改动前端；浏览器验收属于 P3 |

---

## 3. 本轮改动

### 3.1 P0-1 服务/仓储边界授权

| 文件 | 改动 |
| --- | --- |
| `app/api/routes.py` | `list/get/clarify/cancel` 把此前**被丢弃**的 `TrustedContext` 传入 service（`create_task` 原本就是对的） |
| `app/service.py` | 新增 `_authorize`：组织与创建者都匹配才放行，失败一律 `TaskNotFound`（与"不存在"返回同一个 404，不用状态码差异泄露资源是否存在）；旧记录缺组织/创建者时**失败关闭**；`list_tasks` 只返回调用方自己创建的任务 |
| `app/retrieval/base.py` | 新增 `organization_visible`：无组织标记=公开合成语料，带标记必须显式授权。**默认空集**，即不传就只能看到公开语料 |
| `app/retrieval/keyword.py` | `KeywordRetriever.search` / `HybridRetriever.search` 增加租户作用域，在**打分循环内、排序前**生效 |
| `app/retrieval/corpus.py` | `load_corpus` / `build_retriever` 接受显式 `organization_id` |
| `app/tools/business.py` | `_search` 的组织范围取自 `TrustedContext`（工具参数里写不出来） |

**顺带修掉的一个真实语义 bug**：`chunk_markdown` 原先把文档正文里的「适用范围」（如
"PostgreSQL 生产库"）回填成 `Chunk.organization_id`，把**适用场景**当成了**租户身份**。
一旦某份真实文档写着"适用范围：某客户"，就会产生错误的组织归属。已改为组织标记**只来自
显式入参**，并删除了不再使用的 `_SCOPE` 正则。

### 3.2 P0-2 执行所有权与持久化诚实性

| 文件 | 改动 |
| --- | --- |
| `app/store/tasks.py` | `save` 改为**先落盘、成功后才提交内存**；`_load` 区分"文件不存在"（正常）与"损坏/不可读"（抛 `TaskRepositoryCorrupted`，失败关闭）；落盘失败清理临时文件；空路径由死代码改为显式 `ValueError` |
| `app/service.py` | 引入 `execution_id` + `run_generation`：`cancel` 与重跑都会换掉 `execution_id`，旧执行在 `_save_if_owner` 处被拒绝写入；`finally` 不再无条件落盘；新增 `_dispatch_or_fail`（派发失败标为 `FAILED`，不留 `RECEIVED` 残骸）；新增 `_mark_interrupted_tasks`（启动时把上次遗留的 `RECEIVED`/`RUNNING` 标为中断失败并写 `restart_policy=interrupted_without_resume`） |

### 3.3 P0-3 只读工具的服务间认证

| 文件 | 改动 |
| --- | --- |
| `internal/httpapi/agenttools.go`（新增） | `GET /api/agent-tools/changes/{id}?projection=context\|findings\|experiment`。两层认证：共享密钥（常量时间比较，未配置时 503 失败关闭）+ 成员委托（回查成员存在且启用、组织一致，再走 `ChangeFor` 的组织与应用级授权）。未知投影直接拒绝，不做"默认返回全部"兜底 |
| `internal/httpapi/server.go` | `routes()` 改为两棵树：内部只读接口**不复用会话中间件**，其余仍走 `auth.Middleware`；配置状态新增 `agent_tools_enabled` |
| `app/tools/business.py` | `_fetch_change` 改调内部只读接口，携带共享密钥与委托身份；缺密钥时**显式不可用且不发请求**；401/403/404/503/其他分别给出明确错误 |
| `app/config.py` | `upstream_token` 注释补上**双向**语义 |

**没有新增环境变量**：复用了已有的 `DBGUARD_AGENT_UPSTREAM_TOKEN` ↔ `AGENT_UPSTREAM_TOKEN`
配对。`runtimeconfig.applyBrandAliases` 是通用前缀映射，因此 `docker-compose.yml` 现有的
`CHANGEGUARD_AGENT_UPSTREAM_TOKEN` 会自动映射到 Go 侧读取的名字，**编排文件无需改动**。

---

## 4. 缺陷复现证据（改前 vs 改后）

复现不是在脑子里做的：P0-1 用 `git stash` 把改动暂存后对**原始代码**跑同一批用例；
P0-2 用一份只依赖两版共有 API 的独立脚本（示例见 §6 的说明）跑改前/改后对照。

### 4.1 任务授权（B1/B2/B10）

原始代码：新增的 14 项授权用例**全部失败**，且失败是真实行为而非测试问题：

- `test_other_callers_cannot_read_a_task`：跨组织读取返回 **200**（应为 404）；
- `test_other_callers_cannot_cancel_a_task`：他人取消返回 **200**；
- `test_tool_layer_scopes_search_to_the_caller_organization`：
  `assert 'private#1' not in ['private#1', 'public#1']` —— **不同组织确实检索到了他人的私有语料**；
- `test_demo_corpus_stays_public_after_scope_change`：证实了上述「适用范围→organization_id」的混淆。

修复后：91 passed。

### 4.2 生命周期与持久化（B4/B5/重启策略）

| 观测项 | 原始代码 | 修复后 |
| --- | --- | --- |
| 损坏状态文件 | `silently loaded empty -> list()=[]` | `refused to load -> TaskRepositoryCorrupted` |
| 落盘失败后的内存 | `memory=DRAFT_READY disk=RECEIVED`（**内存跑在磁盘前面**） | `memory=RECEIVED disk=RECEIVED` |
| 落盘失败后读未落盘记录 | `get(task_new)={'task_id': 'task_new', ...}`（**能读到磁盘上不存在的记录**） | `get(task_new)=None` |
| 重启时的在途任务 | `status=RUNNING restart_policy=None`（**永久卡住**） | `status=FAILED restart_policy=interrupted_without_resume` |

### 4.3 B3（执行所有权）

诚实说明：B3 是一个**竞态**，它的确定性复现依赖所有权原语本身，因此无法在原始代码上
写出一条稳定失败的用例——原始代码根本没有能表达"这次执行是否仍是当前执行"的东西。
它的证据是：新增的所有权单元测试（`test_stale_execution_cannot_overwrite_a_newer_one`
等）在原始代码上**根本无法被收集**（`_begin_execution` / `_save_if_owner` 不存在），
以及修复后这些用例全部通过。**这一条不要按"已复现"表述**。

---

## 5. 跨服务验收（真实进程，非 mock）

按规格要求，真实启动隔离的 Go 服务 + 合成演示数据：

```
go build -o dbguard.exe ./cmd/dbguard
# 以 PORT=18099 / DBGUARD_DATA_FILE=<隔离路径> / DBGUARD_ENABLE_DEMO_ACCOUNTS=true
#    / DBGUARD_AGENT_UPSTREAM_TOKEN=e2e-shared-secret / DBGUARD_WORKERS=0 启动
# 用带相同密钥的 Settings 驱动 Toolbox 调用三个只读工具（变更 chg_20260730_001）
```

结果（10/10 通过）：

| 检查 | 结果 |
| --- | --- |
| 改造前的老路径 `/api/changes/{id}` 仅凭身份头被拒（**B11 的实测复现**） | PASS，`status=401` |
| `get_change_context` 经内部只读接口可用 | PASS |
| context 投影带制品摘要与 `description_untrusted` 标记 | PASS |
| `get_rule_findings` 可用，带确定性风险等级 | PASS（`MEDIUM`） |
| `get_experiment_report` 可用，状态可区分 `NOT_RUN` | PASS（`NOT_RUN`） |
| 伪造共享密钥被拒 | PASS |
| 未配置共享密钥时显式不可用 | PASS |
| 冒充其他组织被拒 | PASS |
| 不存在的变更明确失败 | PASS |

验收后已停止该后台进程，未占用端口。

---

## 6. 迁移与回滚

| 项 | 说明 |
| --- | --- |
| 数据迁移 | **无**。任务状态仍是同一个 JSON 文件，只是新增了 `execution_id` / `run_generation` / `restart_policy` 字段（旧记录缺这些字段时按失败关闭或按缺省处理） |
| 配置迁移 | **无**。未新增环境变量，复用既有共享密钥 |
| 接口变更 | **新增** `GET /api/agent-tools/...`；未修改或删除任何既有接口 |
| 行为变更 | ① 任务读写默认仅创建者可见（原先任何已认证调用方都能读写）；② 损坏的状态文件会让服务启动失败（原先静默从空库启动）；③ 三个远程只读工具现在**必须**配置共享密钥才能用 |
| 回滚 | 全部改动集中在 8 个文件，`git checkout -- <paths>` 即可回到基线行为。回滚后需注意：跨组织读写、取消回写、静默清空等问题会一并回来 |

---

## 7. 残留风险

- **重启不续跑**：在途任务被显式标为失败，用户需要重新发起。这是 P0 的有意取舍，
  不是遗漏；真正的恢复（检查点 + 重新授权 + 输入版本校验）在 P2。
- **执行期间存储状态仍是 `RECEIVED`**：工作流结果在结束时一次性落盘，因此无法从状态字段
  判断"跑到第几步"。这是既有设计，本轮未改；靠 `events` 观察。
- **跨实例认领**：任务仓储仍是单实例文件存储，多实例需要外部状态存储（既有已知限制）。
- **`test_deny` 之外的授权策略**：默认仅创建者可操作。若将来需要组织共享或管理员代管，
  必须在 `AgentService._authorize` 显式实现——目前没有任何隐式共享。
- **只读接口的成员委托**仍依赖"本服务只被治理服务调用"这一前提；共享密钥是本轮的边界。

---

## 8. 仅由当前代码与实测支持的表述

以下每条都能用本文档 §2–§5 或仓库中可运行的测试复现，未引用任何外部材料，
也未包含未测得的数字：

1. 在 ChangeGuard 治理服务中新增了服务间专用只读接口，用**共享密钥 + 成员委托**两层认证
   替代原先不可用的"仅身份头"调用路径，并通过真实跨进程验收证明链路可用
   （旧路径实测 401，新路径实测可用，伪造凭据/跨组织实测被拒）。
2. 把 Agent 的任务读写授权下沉到**服务与仓储边界**，并按组织与创建者隔离；
   跨组织读取、列表泄露、他人取消在修复前均为可通过用例复现的缺陷。
3. 使 Agent 任务持久化具备**诚实性**：先落盘后提交内存、损坏状态失败关闭、
   `NOT_RUN` 与真实结果可区分；并用改前/改后对照证明了内存领先磁盘与静默清空两个缺陷。
4. 为 Agent 执行引入**执行所有权（execution_id / run_generation）**，使取消与重跑能栅栏住
   旧协程的回写，并把进程重启后的在途任务显式标注为"中断且未续跑"。
5. 为检索层建立了**租户可见性边界**，并顺带修正了把文档"适用范围"误当作组织身份的语义错误。

**不能声称**的（本轮未做或未验证）：多轮对话、节点级检查点与进程重启续跑、
多 Agent 协作、自进化 Prompt、评估集拆分与真实模型质量、任何准确率/延迟提升数字。

---

## 9. 剩余任务

| 阶段 | 状态 | 内容 |
| --- | --- | --- |
| P0 | ✅ 完成 | 授权、执行所有权与持久化诚实性、只读工具服务间认证 |
| P1 | ⬜ 未开始 | 受约束调查循环、provider 决策契约、预算与上下文（含已发现的 C1 重试相乘、C2 模型自封"已确认"、C3 版本恒为 1、C4/C5 澄清字段、C6 方言） |
| P2 | ⬜ 未开始 | LangGraph checkpointer、进程重启恢复、材料确认记录。**注意**：本机 `langgraph` 为 1.2.11，checkpointer 包版本必须匹配 1.x，开始前需先收紧依赖并重新记录解析版本 |
| P3 | ⬜ 未开始 | 真正可选的评测模式、开发/保留集拆分、可观测性与 `/agent/` 工作台 |

未开始的原因不是阻碍，而是按规格"P0 → P1 → P2 → P3，每阶段新增回归并复跑测试"推进：
P0 的授权与所有权是调查循环的前置条件，先做它可以让后续阶段的影响面可控。
