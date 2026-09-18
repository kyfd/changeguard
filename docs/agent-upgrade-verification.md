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
| `python -m pytest -q`（`agent-app/`） | **105 passed**（基线 39 + 新增 66：首轮 52、收尾 14） |
| `python evals/run_eval.py --provider deterministic` | **11/11 通过** → `evals/reports/20260917-113001-deterministic.json` |
| 递归 `node --check`（`internal/httpapi/web/**/*.{js,mjs}`） | **clean** |
| `npm test` | **2 passed，0 failed** |

新增回归用例：

| 位置 | 数量 | 覆盖 |
| --- | --- | --- |
| `agent-app/tests/test_authorization.py` | 14 | 任务归属、无权与不存在不可区分、旧记录失败关闭、检索租户隔离 |
| `agent-app/tests/test_task_lifecycle.py` | 27 | 落盘诚实性、损坏失败关闭、执行所有权与取消竞态、派发残骸、重启策略 |
| `agent-app/tests/test_governance_readonly_auth.py` | 11 | 内部只读接口的服务间认证与请求形状 |
| `agent-app/tests/test_execution_lifecycle.py` | 14 | 取消竞态、落盘故障注入、执行监督、inline/background 取消一致性（见 §9） |
| `internal/httpapi/agenttools_test.go` | 10 | 内部只读接口的服务认证、成员委托、组织/应用授权、通路隔离、窄投影 |

### 未运行项

下表区分「本机未运行、但 CI 已在 PR 上覆盖」与「至今未运行」。
两者都不是"测试套件通过"就能当作已验证的项。

**本机未运行，CI 已覆盖**（PR #14，run `35216812259`，全部 ✅ pass）：

| 项 | CI 作业 | 结果 |
| --- | --- | --- |
| `go test -race ./...` | `quality-go (1.25.x)` / `quality-go (1.26.x)` | pass |
| PostgreSQL / Redis 集成测试 | `integration` | pass |
| Playwright 端到端 | `e2e` | pass |

> 这几项在本机无法运行（Windows 无 C 工具链 / 无隔离数据库 / 未起 Compose），
> 因此它们是**由 CI 提供证据**，不是由本机实测提供。引用时必须说明来源。

**至今未运行**（不得当作通过）：

| 项 | 原因 |
| --- | --- |
| `docker compose up --build` 完整编排（含 agent-app） | CI 的 `e2e` 用的是**不含** agent-app 的 `compose.e2e.yml` |
| 真实模型（live）评测 | 未配置专用测试凭据与预算 |
| 真实浏览器交互与截图 | P0 未改动前端；浏览器验收属于 P3 |

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
| 配置迁移 | **无必需项**。未新增必需环境变量。P0 收尾新增两个**可选**配置项：`AGENT_PERSIST_MAX_ATTEMPTS`（默认 3）与 `AGENT_PERSIST_RETRY_BACKOFF`（默认 0.05 秒），不设置即用缺省 |
| 接口变更 | **新增** `GET /api/agent-tools/...`；未修改或删除任何既有接口。`cancel`/`clarify` 新增 **503** 作为"操作未生效、可重试"的显式语义 |
| 行为变更 | ① 任务读写默认仅创建者可见（原先任何已认证调用方都能读写）；② 损坏的状态文件会让服务启动失败（原先静默从空库启动）；③ 三个远程只读工具现在**必须**配置共享密钥才能用；④ 取消不再先停止执行再落盘，落盘失败时执行继续运行并返回 503；⑤ 终态落盘失败时会降级上报而不是静默丢监督 |
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
- **取消是协作式的**（§9.5）：不响应取消的外部依赖，其**正在进行**的那次调用无法被中断。
  能保证的是不再调度后续步骤、且其迟到结果不会被发布。
- **`cancel` 返回时工作流可能仍在退出中**：取消落盘成功后即返回，句柄释放与健康计数归零
  要等执行真正退出。这是有意的——否则一个不响应取消的依赖会把取消请求一起挂住。
- **降级状态只在本进程内可见**：`unpersisted_tasks` 与 `degraded` 是内存状态，
  单实例文件存储下进程重启即丢失；重启时在途任务会被标为中断失败（既有机制）。
  多实例要看到一致的降级视图，需要外部状态存储（与既有限制同源）。
- **降级记录按任务覆盖、不累积清理**：每个任务只保留最后一条问题记录，新执行开始时清除。
  存储长时间不可用时会留下与受影响任务数同量级的条目——这是有意的（健康状态必须持续可见）。

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
6. 让 Agent 执行具备**受监督的生命周期**：取消先落盘后终止执行、终态落盘带**有上限**重试、
   持续存储故障显式降级而不是伪造已落盘的失败、`inline` 与 `background` 都可被真正取消；
   并用真实浏览器验证了失败时不会被误诊为"功能未启用"、也不会禁用面板。

**不能声称**的（本轮未做或未验证）：多轮对话、节点级检查点与进程重启续跑、
多 Agent 协作、自进化 Prompt、评估集拆分与真实模型质量、任何准确率/延迟提升数字。

---

## 9. P0 收尾：执行生命周期加固

本节记录 P0 的第二轮。审查基于提交 `c15d9fa`；三项问题在该提交上**均仍存在**，已复核。

### 9.1 修了什么

| # | 问题 | 根因 | 处理 |
| --- | --- | --- | --- |
| 1 | 取消落盘失败后执行成为孤儿 | `cancel` 先移除执行句柄并终止执行，最后才落盘；落盘失败时执行已停、仓储仍 `RECEIVED`，健康检查显示 `ok`、`running_tasks=0`，`clarify` 又因状态不允许而拒绝 | 改为**先在副本上构造并落盘**，成功后才终止执行与清理句柄；失败抛 `TaskCancelRejected`（503，可重试），此时句柄不动、执行继续跑 |
| 2 | 终态落盘失败后丢失执行监督 | `_execute` 里 `_release` 在 `_save_if_owner` **之前**，落盘失败时句柄已移除、异常逃出既有异常处理，任务留在 `RECEIVED`，`wait_for` 无声返回陈旧状态 | 重新设计收尾：执行结束与持久化解耦，由**受管理执行**统一收口；终态落盘带**有上限**重试；用尽则记入降级状态，健康状态报 `degraded`，`wait_for` 抛 `TaskStateUnavailable` |
| 3 | inline 取消不生效 | `inline` 直接 `await _execute`，从不登记句柄，`cancel` 找不到可取消的对象；只能拒绝回写，工作流仍在调用模型与工具 | `inline` 与 `background` **共用同一套受管理执行**：都登记句柄，区别只在等待方式 |

### 9.2 自查发现的两个隐患（原清单未列）

在写测试与复查实现时发现，均已修掉：

- **落盘重试退避期间被取消会误报降级**：那时终态所有权已转移（用户取消或调用方放弃各自落盘），
  再记一条"落盘失败"会制造虚假的降级信号。现在让 `CancelledError` 正常向上传播。
- **放弃路径可能覆盖已落盘的终态**：调用方被取消与执行完成之间可能只差一个事件循环轮次，
  把已经落盘的 `DRAFT_READY` 改写成 `FAILED` 比"不写"更糟。现在只对**在途状态**执行放弃写入。

### 9.3 由本次改动引入、又被本次改动修掉的 UI 回归

`cancel` 失败现在返回 **503**，而治理代理"下游 Agent 未配置"**也**返回 503。
工作台的 `handleActionError` 原先按状态码判断，把 503 一律当成"功能未启用"：

- 提示用户"配置 `DBGUARD_AGENT_BASE_URL` 后重启本服务"——**错误建议**；
- 并调用 `markAgentDisabled`，**把整个面板永久置为不可用**（禁用创建按钮）。

一次存储抖动就会触发上述两件事。修法是让错误对象保留服务端的 `code`，
只在 `code === "SERVICE_UNAVAILABLE"` 时走"未启用"分支，其余 503 按普通错误展示。

### 9.4 修复后的语义

| 场景 | 行为 |
| --- | --- |
| 取消落盘失败 | 显式失败（`TaskCancelRejected`，HTTP 503 + `Retry-After`）；执行**继续运行**、句柄保留；重试可成功 |
| 终态落盘一次性失败 | 有上限重试（默认 3 次，退避 0.05s × 尝试次数）后成功；任务进入明确可查询的终态；健康状态保持 `ok` |
| 终态落盘持续失败 | 重试用尽；**不伪造已落盘的 FAILED**（仓储仍是最后一次成功写入的状态）；健康状态 `degraded` + `unpersisted_tasks`；`wait_for` 抛 `TaskStateUnavailable` |
| 重试期间被新执行接管 | 记 `skipped`：既不写旧结果，也**不谎报**存储降级 |
| 后台异常 | 由完成回调统一取走并记入降级状态，不再是只能靠 `Task exception was never retrieved` 在日志里偶遇 |
| 资源释放 | 由完成回调统一负责，且**校验执行身份**后才移除句柄，不会误删后来的新执行 |
| 调用方被取消（inline 请求断开） | 终止内部执行并留下确定的终态（`FAILED`，说明被中断），不留孤儿 |

### 9.5 inline / background 取消保证与限制

**保证**：取消会真正命中工作流（测试断言 provider 观察到取消），取消后**不再调度后续工具或模型调用**
（用工具调用计数断言），迟到结果不会被发布。

**限制**：取消是**协作式**的，在 await 点生效。若某个外部依赖不响应取消
（例如同步阻塞调用），我们无法中断它**正在进行的**那一次调用；能保证的是不再调度后续步骤、
且其结果不会被发布。"取消后不再回写"与"执行确实停了"是两件事，前者不能用来宣称后者——
所以测试同时断言了 provider 收到取消，而不只是检查回写被拒。

### 9.6 新增测试与改前/改后对照

新增 `agent-app/tests/test_execution_lifecycle.py`（14 项），全部事件驱动、无随机 sleep、
不依赖真实模型；落盘故障用包一层真实仓储的 `FlakyRepository` 注入。

**修复前**（把 `agent-app/app` 暂存回基线后运行同一批用例）：**7 failed / 7 passed**。
其中只有 **5 项是干净的业务复现**，必须如实区分：

| 结果 | 项 | 改前失败原因 |
| --- | --- | --- |
| 业务复现 | 取消失败不得丢弃执行句柄 | `assert []`：句柄已被丢弃 |
| 业务复现 | 一次性落盘故障后进入终态 | `'RECEIVED' == 'DRAFT_READY'`：卡在 RECEIVED |
| 业务复现 | 持续落盘故障必须显式且不伪造 | `wait_for` 未抛错，无声返回陈旧状态 |
| 业务复现 | 取消后不再调度工具（inline） | `provider.cancelled is False`：取消没命中工作流 |
| 业务复现 | inline 请求被取消不留孤儿 | 终态仍是 `RECEIVED` |
| **契约级** | inline 落盘故障要显式失败 | `assert 'ok' == 'degraded'`：断言的是新增健康契约，**不是**行为复现 |
| **缺方法** | 放弃路径不得覆盖终态 | `AttributeError: no attribute '_abandon_execution'`：这是**新方法的护栏测试**，**不得称为复现** |

**修复后**：该文件 14 项全过；全量 `pytest -q` **105 passed**（原 91 + 新增 14）。

### 9.7 浏览器验证（真实 Chromium，含截图）

三个进程独立启动：挂住的模型 stub（18999）、带触发文件故障注入的 Agent 服务（8091）、
配置了 Agent 的治理服务（18099）。用演示账号真实登录后进入 `/agent/`。

| 检查 | 结果 |
| --- | --- |
| 工作台加载后创建按钮可用 | PASS |
| 加载时未误报"下游未配置" | PASS |
| 任务停留在途状态（`RECEIVED`），停止按钮可见 | PASS |
| 注入持续落盘故障后点"停止"→ 显示错误横幅 | PASS（文案：`取消未生效：状态未能落盘（OSError），任务仍在运行，请重试`） |
| 错误信息说明可重试 | PASS |
| **未把应用层 503 误诊为「下游未配置」** | PASS |
| **未因一次存储失败禁用整个面板** | PASS |
| 任务仍在运行，没有被假装取消 | PASS（仍为 `RECEIVED`） |
| 恢复存储后重试 → 任务真正取消 | PASS（`CANCELLED`） |
| 无未捕获的 JS 异常 | PASS |
| 对照组：下游**确实**未配置时仍显示"未启用"并禁用按钮 | PASS（4/4） |

截图（保存在会话临时目录，随会话清理）：`01-workbench-loaded`、`02-task-running`、
`03-cancel-failed`、`04-cancel-succeeded`、`10-agent-disabled`。其中 `03-cancel-failed`
可视确认错误横幅与"开始准备材料"按钮仍可用。

> 注意：上一节的 `e2e` CI 作业**不能**作为本轮生命周期场景的证据——它跑的是既有的
> 配置/SQL 黄金流程，不覆盖取消竞态与落盘故障。本节的浏览器检查是针对这些场景新做的。

### 9.8 本轮未执行

| 项 | 原因 |
| --- | --- |
| 窄屏（移动端）浏览器检查 | 本轮只做了 1440×900 桌面视口 |
| `go test -race`（本机） | 仅 Linux CI 可跑 |
| 含 agent-app 的完整 `docker compose up --build` | 未改编排文件 |
| 真实模型质量 | 本轮不涉及；stub 只为"让任务停在调用上"，不模拟质量 |

---

## 11. P1（进行中）

P1 的目标是"受约束调查 Agent"。本轮先做**前置修复**：调查循环要在正确的重试分层与草案契约之上
运行，否则新的循环只会把这些缺陷放大。

**本节只记录已经完成并验证的部分。P1 主体（受约束调查循环）尚未开始**，见 §11.3。

### 11.1 已修的缺陷与改前/改后对照

| 缺陷 | 改前行为 | 现在 |
| --- | --- | --- |
| **重试次数相乘**：provider 与工作流共用 `llm_max_attempts` | 一次生成最多打 `N²` 次模型调用（N=3 时实测 **9 次**）；401 这类重试不会改变结果的失败也被两层各重试一遍（实测 **4 次**） | 拆成两个语义不同的开关：`llm_max_attempts` 只管传输层与暂时性故障（408/409/425/429/5xx），新增 `draft_parse_attempts` 只管"模型输出无法解析"。不可重试的失败**立刻停下**。上传次数上界为 `llm_max_attempts × draft_parse_attempts` |
| **模型可自封"已获人工确认"** | `parse_model_draft` 直接采信模型输出的 `confirmed`；离线确定性生成器自己也写过 `confirmed: true` | `confirmed` 移出允许字段，出现即拒绝（不静默忽略）；两个标志都由服务端决定，恒为 `confirmed=False` / `needs_confirmation=True` |
| **草案版本恒为 1** | `version` 硬编码；`revision_notes` 从未被写入——改了三轮的草案看起来仍是第一版 | 版本由服务端按实际轮次写入；`revision_notes` 记录本轮修订所响应的**确定性检查反馈** |
| **方言不匹配** | 目标为 MySQL 时同样产出 `CREATE INDEX CONCURRENTLY`、`SET lock_timeout`，并被标成可用草案 | 入口即拦下不在支持范围内的方言：任务以明确原因停在边界，不生成草案、不进入 PG 规则扫描；确定性生成器另有一道防线 |
| **`note` 被静默丢弃 / 无法补快照** | `ClarifyRequest.note` 被接收后丢弃；`schema_snapshot` 不是可补充字段（Pydantic 静默忽略） | `note` 写入任务记录并作为「补充说明」拼入模型看到的需求文本；`schema_snapshot` 可补齐，且不提供时不覆盖已有快照 |

改前对照（把 `agent-app/app` 暂存回基线后运行同一批新用例）：

| 文件 | 改前 | 说明 |
| --- | --- | --- |
| `test_model_retry_layering.py` | **4 failed** | 失败信息即成本本身：`重复发送了 4 次`、`实际 4 次，上限 2 次`、`实际 9 次，上限 3 次` |
| `test_draft_contract.py` | **10 failed** | 全部为行为复现：自封确认被接受、版本恒为 1、MySQL 拿到 PG 语法、`note` 未落记录、快照未生效 |

**没有一项失败是"缺少方法或字段"造成的**——为此测试里的 Settings 覆盖会过滤掉基线不支持的字段，
避免用 `TypeError` 冒充业务复现。

一处既有断言因其编码了旧耦合而调整：`test_invalid_json_fails_with_clear_error` 原本断言工作流层
重试次数等于 `llm_max_attempts`。保留其意图（有限重试而非无限循环），改为显式设
`draft_parse_attempts=2` 并断言恰好用完——比原来更强，不再依赖默认值。

### 11.2 验证

| 命令 | 结果 |
| --- | --- |
| `pytest -q` | **125 passed**（P0 收尾后 105 + 新增 20） |
| `evals/run_eval.py --provider deterministic` | **11/11** |
| `node --check` / `npm test` | 未受本轮影响（未改前端） |

### 11.3 受约束调查循环

新增 `app/workflow/investigate.py`。要点是**边界由代码强制，不由提示词**——
提示词里写"最多查 3 次"，模型可以不遵守；这里的上限循环自己拦。

| 边界 | 实现 |
| --- | --- |
| 轮次上限 | `max_investigation_rounds`（默认 4） |
| **累计**工具调用上限 | `max_total_tool_calls`（默认 8）——只限轮次挡不住"一轮里调很多次" |
| 单工具超时 | `tool_timeout_seconds`（默认 5s），超时按失败处理，不让循环挂住 |
| 空转检测 | 相同工具 + 相同参数再次出现即判定无进展并停止，**第二次不会真的执行** |
| 完成条件 | 由 `RequiredEvidence` 确定性判定；**决策者说"够了"不算数** |
| 停止原因 | 七种之一（证据足够 / 证据不足 / 轮次用尽 / 调用用尽 / 无进展 / 决策者不可用 / 工具失败），必须可见 |

决策者的能力必须显式声明，不允许把规则包装成模型：

- `RulePlanner`：**确定性规则**决策者，名字就是 `rule`，报告里也写 `planner=rule`；
- `ProviderPlanner`：要求 provider 具备原生动作契约（`decide()`）。OpenAI 兼容 provider
  自 §11.5 起实现了该契约；不具备 `decide()` 的 provider 会**显式报告不可用**，
  事件里留下「调查循环未启动」+ 原因，而不是用规则顶替并宣称是模型的选择；
- `ScriptedPlanner`：供测试构造确定动作序列。

接入方式是策略开关，**默认不改变既有行为**：`investigation_strategy` 默认 `fixed_workflow`
（原固定检索顺序保留为默认与回退），设为 `bounded_agent` 才走循环。

新增 15 项测试（`tests/test_bounded_investigation.py`），覆盖：轮次与累计调用两个上限、
空转检测（并断言重复调用未被真的执行）、单工具超时不让循环挂住、决策者不能说"证据够了"、
必需证据齐备时才判为足够、预算用尽但证据已齐时如实改判、工具失败不被当成成功、
以及 provider 不具备动作能力时的显式不可用。

> **这不是"已复现的缺陷"，是新能力**：循环此前不存在，因此没有"改前失败"的证据，
> 这 15 项用例是**新行为的规格**，不是缺陷复现。不要混为一谈。

### 11.4 P1 剩余部分

| 项 | 状态 |
| --- | --- |
| C1 重试分层 / C2 自封确认 / C3 版本 / C4·C5 补充字段 / C6 方言 | ✅ 完成（§11.1） |
| 受约束调查循环（预算、空转、必需证据、策略开关、决策者能力声明） | ✅ 完成（§11.3） |
| **模型原生工具选择**（provider 实现 `decide()`，用原生函数调用驱动动作） | ✅ 完成（§11.5） |
| 工具结果的结构化截断与证据标识/版本/摘要哈希 | ✅ 完成（`ToolObservation.payload_digest`，用例 `test_all_tool_results_are_fed_back_not_only_search_hits`） |
| usage 缺失时的预算策略（不得把未知 token/费用填 0） | ✅ 完成（`UsageBudget.known`，用例 `test_usage_is_unknown_rather_than_zero`） |
| `fixed_workflow` 与 `bounded_agent` 的**同输入对照评测** | ⬜ 未实现（属 P3 评测范围） |

**受约束调查循环与"固定脚本"的分界线已经跨过**：模型现在真的能选择只读工具（§11.5）。
但"模型能选"不等于"模型质量已证明"——真实模型评测仍为 `NOT_RUN`（§11.6）。

### 11.5 模型原生动作决策（P1 主体）

`InvestigationPlanner.plan` 改为 **async**：三个实现（`ProviderPlanner` / `RulePlanner` /
`ScriptedPlanner`）、循环里的调用、以及直接调用 `plan` 的测试辅助类都随之改。
`OpenAICompatibleProvider` 新增 `decide()`：用 OpenAI 兼容的**原生函数调用**让模型自己选择
只读工具，返回 `CallTool` / `Finish` / `AskUser`。

边界依旧由代码强制，不由提示词强制：

| 边界 | 实现 |
| --- | --- |
| 工具白名单 | `MODEL_ACTION_TOOLS` 在 `app/llm/provider.py` **单独声明**，不从注册表推导；下发时还要求 `read_only=True`。注册表新增写工具不会自动进入模型可见集合 |
| 一致性断言 | `test_model_action_whitelist_matches_registry_read_only_tools` 断言白名单**恰好等于**注册表只读工具集合：新增只读工具会使它失败，迫使作者显式登记；新增写工具不影响它 |
| 参数校验 | 复用注册表同一套 `validate_args`：未知字段、类型不符、缺必填一律拒绝，且该动作**不会被执行** |
| 身份与授权 | 只能来自服务端：`decide()` 在 schema 校验**之前**显式拒绝 `organization_id` / `X-Actor-Id` 等身份字段；工具执行用的是循环持有的 `TrustedContext`，模型参数无法覆盖 |
| 决策失败 | 未知工具、非法参数、伪造身份、模型调用失败 → `PlannerDecisionError` → 循环以 `PLANNER_FAILED` 停下；**不把失败当成"跳过"**，也不会继续生成草案 |
| 完成条件 | 模型调用 `finish_investigation` 只表示"查完了"；证据是否足够仍由 `RequiredEvidence` 判定 |

`ProviderPlanner` 接收**服务端**给出的只读工具规格（`build_planner(settings, provider, registry.specs())`），
只用于告诉模型有哪些工具、以及校验参数——模型无从新增工具。

新增 `tests/test_provider_action_decisions.py`（22 项），全部用 `httpx.MockTransport` 模拟
OpenAI 兼容端点，覆盖：多轮动作、非法参数（类型/缺必填/未知字段）、未知工具、伪造身份、
模型请求超时、模型所选工具超时、取消、工具调用预算耗尽、模型自封完成不改变证据判定、usage 上报。

> **这不是真实模型质量证明。** MockTransport 验证的是**契约与边界**，不是模型决策质量；
> 用例本身也不产生任何"准确率/延迟"数字。

### 11.6 本轮验证与未运行项

| 命令 | 结果 |
| --- | --- |
| `pytest -q` | **183 passed**（§11.3 后 161 + 本轮新增 22） |
| `evals/run_eval.py --provider deterministic` | **11/11** → `evals/reports/20260918-014314-deterministic.json` |
| `go test ./internal/agent ./internal/httpapi ./internal/service -count=1` | 三个包 **ok**（0.540s / 0.792s / 0.679s） |

**NOT_RUN（不得当作通过）**：

| 项 | 原因 |
| --- | --- |
| 真实模型（live）动作决策评测 | 未配置专用测试凭据与预算；不把 MockTransport 结果当模型质量 |
| 真实模型（live）草案质量评测 | 同上 |
| `go test -race ./...`、PostgreSQL/Redis 集成、Playwright e2e | 本机不满足运行条件，仅 Linux CI 覆盖 |


---

## 12. P2（PR-A）：节点级中断与从检查点恢复

### 12.1 依赖与解析版本（已收紧）

`pyproject.toml` 从 `langgraph>=0.2` 收紧为 `langgraph>=1.2,<2`，并新增
`langgraph-checkpoint-sqlite>=3.1,<4`。本机实测解析到：

```
langgraph 1.2.11 · langgraph-checkpoint 4.2.0 · langgraph-checkpoint-sqlite 3.1.1 · aiosqlite 0.22.1
```

检查点是**磁盘上的 SQLite**（`AsyncSqliteSaver`），不是 `InMemorySaver`——后者不能用来宣称支持进程重启。
连接是**短生命周期**的：每次图调用开一条连接、用完即关，避免跨事件循环复用同一条连接。

### 12.2 节点级中断（不是走到 END）

`WorkflowDeps` 新增 `checkpointer`；`compile(checkpointer=…)`，`thread_id=task_id`。
缺信息时 `_check_info` 调用 `interrupt({...})`，图**停在节点上**并落检查点。
`_route_entry` 的"走到 END + NEEDS_INFO"分支保留：**没有检查点的调用方**（部分既有测试、评测的
workflow harness）行为不变，因为 `interrupt()` 在没有检查点时会报错。

恢复值由服务端给出**完整**输入（slots + 快照 + 需求 + 输入版本），因此重复恢复是幂等的，
不会把同一条补充说明拼接两次。

### 12.3 恢复前的重新校验（不做"尽力继续"）

| 校验项 | 不通过时 |
| --- | --- |
| 归属：组织 + 创建者 | `TaskNotFound`（404，与"不存在"不可区分） |
| 状态：终态（`CANCELLED`/`INPUT_REJECTED`/…）不允许恢复 | `TaskNotResumable`（409） |
| 检查点存在且有待继续步骤 | 409 |
| **续跑未完成节点**（mode=checkpoint）时输入/材料版本必须与检查点一致；核对不出（无记录版本）**失败关闭** | 409 |

恢复模式如实区分并写入任务记录与视图：`interrupt`（从中断点续跑）、`checkpoint`（续跑未完成节点）。
`restart_from_scratch` 只在真正重跑时使用，**不把重跑说成续跑**。

### 12.4 进程重启时的处置

启动扫描改为**检查点感知**：在途任务（`RECEIVED`/`RUNNING`）若有检查点 →
`restart_policy=checkpoint_available`（需创建者显式恢复）；无检查点 → `interrupted_without_resume`。
判定用标准库 `sqlite3` 同步读同一份检查点文件（启动发生在事件循环之外），任何异常按"没有检查点"处理（保守）。
**不自动续跑**：留在在途状态会让健康检查与界面看起来还有任务在推进，因此仍显式落一个终态。

### 12.5 独立进程恢复验收（真实进程，非 mock）

`agent-app/scripts/recovery_acceptance.py`：起独立 uvicorn → 跑到中断 → **终止进程** → 重启 → 授权恢复 → 完成。

```
第一次启动 pid=33812  第二次启动 pid=4824
[PASS] 创建后在节点级中断上等待补充（status=NEEDS_INFO）
[PASS] 标记为 awaiting_input
[PASS] 追问包含缺失项
[PASS] 入口节点只执行过一次（screen_input=1）
[PASS] 进程已终止（PID 不再存活）
[PASS] 磁盘上存在检查点文件
[PASS] 确实是新进程（PID 不同）33812 -> 4824
[PASS] 重启后任务仍可查询且等待补充
[PASS] 授权恢复后完成到 DRAFT_READY
[PASS] 恢复模式标注为 interrupt
[PASS] 入口节点仍未重复执行（未从头重跑）screen_input=1
[PASS] 留下了 resumed 事件
[PASS] 产出了草案
结果：13/13 通过
```

判据是持久化事实：重启前后 **PID 不同**、检查点文件在磁盘上、且 `screen_input` 事件在整条
生命周期里**只出现一次**（若从头重跑会出现两次）。

### 12.6 本轮命令与结果

| 命令 | 结果 |
| --- | --- |
| `python -m pytest -q` | **193 passed**（§11 后 183 + 本轮新增 10） |
| `python evals/run_eval.py --provider deterministic` | **11/11** |
| `go test ./internal/agent ./internal/httpapi ./internal/service -count=1` | 三个包 **ok** |
| `go vet ./...` / `gofmt -l ./internal ./cmd` | clean / clean |
| `npm test` | 2 passed |
| 递归 `node --check`（`internal/httpapi/web/**/*.js`） | clean |
| `scripts/recovery_acceptance.py` | **13/13** |

新增用例：`tests/test_checkpoint_resume.py`（9 项：节点级中断、中断后继续不重跑、受支持失败续跑、
归属重校验、无待恢复步骤拒绝、陈旧输入版本拒绝、重启可恢复/不可恢复标注、取消不可恢复）
与 `tests/test_api.py::test_resume_endpoint_continues_from_the_interrupt`（路由层）。

### 12.7 任务书 §P2-8（JSON→SQLite 迁移）**不适用（N/A）**

已按决策采用：**任务记录仍存 JSON**（`app/store/tasks.py` 原样保留，既有持久化诚实性测试与语义不变），
SQLite **只存 LangGraph 检查点**。因此不存在"从 JSON 迁 SQLite"的迁移、也不需要对任务记录做版本化迁移。
检查点库由 `AsyncSqliteSaver.setup()`（`CREATE TABLE IF NOT EXISTS`）创建，本身可重入。

### 12.8 未运行（不得当作通过）

`go test -race ./...`、PostgreSQL/Redis 集成、Playwright e2e 仍由 CI 提供证据（本机不满足运行条件）；
真实模型（live）评测无凭据 → `NOT_RUN`。

---

## 13. P2（PR-B）：材料人工确认与 flaky 修复

### 13.1 材料确认（P2-6）

新增 `Confirmation` 记录与 `POST /api/agent/tasks/{id}/confirm`（仅创建者）。

| 要求 | 实现 |
| --- | --- |
| 记录确认人 / 时间 / 材料版本或内容哈希 | `confirmed_by`、`confirmed_organization`、`confirmed_at`、`material_version`(=input_version)、`material_hash`(草案 SQL + 回滚 SQL 的 SHA-256) |
| **幂等** | 同一人 + 同一版本 + 同一内容重复确认：直接返回，**不新增记录、不重复写事件** |
| **新修订使旧确认失效** | 草案重新生成（内容哈希变化）或输入/材料版本变化 → 旧确认标 `invalidated_at` + 原因，**保留痕迹**（不物理删除） |
| **确认 ≠ 审批 ≠ 执行许可** | 确认**不改变任务状态**、不产生任何放行判定；事件文案明示"不构成治理审批，也不代表可在生产执行"；审批与通行证仍只由 Go 治理服务负责 |

`material_hash` 提取到 `app/workflow/state.py`，`graph._signature` 改为调用它（去掉重复实现）。

### 13.2 flaky 修复：启动恢复用例不再依赖 1ms 墙钟

**诊断（先诊断，未用"重跑通过"解释）**：`internal/service/service_test.go` 的
`TestStartupRecoveryTakesOverExpiredApplyGeneration` 用 **1ms 租约** `ClaimOutbox` 后**立即**
`CheckpointExperimentOutbox`；`validOutboxLease`（`internal/store/outbox.go:293`）要求
`now.Before(LockedUntil)`。`-race` 下进程被插桩拖慢/被抢占，两次调用间隔常超过 1ms，
checkpoint 便返回 `ErrConcurrentWrite`——这是**测试的墙钟假设**，不是被测行为。
次要问题：断言单次读取 `OutboxCompleted`，而 `CompleteOutbox` 在 `FinalizeExperimentOutbox`
之后执行（`service.go:1279` vs `:1373`），存在"状态已就绪、outbox 尚未完成"的窗口；
`countingRunner.runs` 是普通 int。

**修法（修根因）**：

- `internal/store` 新增可注入时间源：`Store.now`（nil 时用 `time.Now`）+ `Store.clock()`；
  `ClaimOutbox` / `CompleteOutbox` / `RenewOutbox` / `FailOutbox` / `CheckpointExperimentOutbox`
  / `FinalizeExperimentOutbox` 的租约判定全部走它；新增 `NewMemoryWithClock` 供测试注入。
- 两个租约用例（service 与 store 各一）改为：**长租约**下 checkpoint 必成功 → 用时间源把租约
  **确定性地**推到过去 → 再启动恢复。不再有 1ms 租约与 `time.Sleep`。
- 断言改为**有界轮询**至 `OutboxCompleted`；`countingRunner.runs` 改为 `atomic.Int64`。

本机无法运行 `-race`（Windows 无 C 工具链），因此本机证据是 `-count=50` 的反复运行；
`-race` 证据由 CI `quality-go`（1.25.x / 1.26.x）提供。

### 13.3 顺带修复的既有缺陷（独立 PR #19）

诊断集成失败时发现：`postgresBackend.usePassport` 的**同消费者重放**分支返回空 payload，
`Store.UsePassport` 因此跳过 `installPostgresSnapshot`——并发消费中走重放分支的一方会一直把
该通行证显示为 `ACTIVE`（`ConsumedAt` 为 nil），尽管消费已经成功并提交。已改为返回已提交快照，
并让多实例用例断言**竞争双方**都看到 `CONSUMED`，使该缺陷不再被调度顺序掩盖。
该修复为独立 PR（#19，合并 `802f59c`），证据来自 CI `integration` 作业。

### 13.4 本轮命令与结果

| 命令 | 结果 |
| --- | --- |
| `python -m pytest -q` | **201 passed**（PR-A 后 193 + 本轮新增 8） |
| `python evals/run_eval.py --provider deterministic` | **11/11** |
| `go test ./... -count=1` | 全部包 **ok** |
| `go test ./internal/service -run TestStartupRecoveryTakesOverExpiredApplyGeneration -count=50` | **ok**（原本偶发） |
| `go test ./internal/store -run TestExpiredExperimentLeaseIsFencedAfterNewClaim -count=50` | **ok** |
| `go vet ./...` / `gofmt -l ./internal ./cmd` | clean / clean |
| `npm test` | 2 passed |
| 递归 `node --check`（`internal/httpapi/web/**/*.js`） | clean |

### 13.5 未运行（不得当作通过）

`go test -race ./...`（CI `quality-go`）、PostgreSQL/Redis 集成（CI `integration`）、
Playwright e2e（CI `e2e`）——本机不满足运行条件；真实模型（live）评测 `NOT_RUN`。

---

## 14. P3（PR-C）：评测运行器真正生效

### 14.1 CLI 与执行路径

| 参数 | 语义（**真正改变执行路径**，不是改报告名） |
| --- | --- |
| `--provider {deterministic,scripted,live}` | 选择运行时 provider。`deterministic` 一律用确定性生成器；`scripted` 用用例自带的脚本输出（没有则确定性）；`live` 用真实模型，未配置凭据时该用例记 `NOT_RUN` |
| `--strategy {fixed_workflow,bounded_agent}` | 写入 `Settings.investigation_strategy`；`bounded_agent` 时按 provider 选择决策者（live→`provider`，其余→`rule`） |
| `--split {dev,holdout,all}` | 选择开发集 / 保留集 / 两者 |
| `--dataset PATH` | 显式指定数据集（优先于 `--split`） |

用例可声明 `providers` / `strategies` 适用范围；不适用时记 **`SKIPPED`**（写明原因），
既不算通过，也不假装失败。退出码：全部执行通过 `0`；有失败 `1`；**只有 NOT_RUN 没有失败 `2`**
（避免 CI 把"未运行"当绿灯）。

### 14.2 数据集拆分与版本

`evals/datasets/dev.jsonl`（14 例，用于开发与回归）与 `evals/datasets/holdout.jsonl`
（4 例，**不用于反复调参**）。报告记录数据集版本号（每文件 `sha256[:12]`）与**全量 SHA-256**，
便于核对数据集未被改动。

用例覆盖：正常、缺失材料、无效材料（被确定性检查阻断）、无效证据（编造引用被拒）、
恶意输入（注入/越权），工具失败，**预算耗尽**、**取消**、**恢复**。

### 14.3 评分口径

- **硬性安全断言**（确定性，决定通过/失败）：状态、SQL/回滚关键字、引用数量、追问字段、
  检查项与检查状态、错误信息、恢复模式、入口节点执行次数，以及全局断言
  "草案不得包含被模型自封为已确认的假设"。
- **结局类别**：`completed`（以产出草案为目标的用例真的产出草案）/ `correct_refusal`
  （合理的拒绝或追问）/ `incorrect`。任务完成率 = `completed` / 目标为 `DRAFT_READY` 的用例数，
  **合理拒绝算对**，不强迫所有用例都产出草案。
- 报告**不预填任何提升比例**；失败用例原样保留。

### 14.4 报告字段

数据集版本与 SHA-256、提交 SHA、provider、strategy、配置、样本数（total/executed/passed/
failed/not_run/skipped）、逐例结果与失败原因、错误类型、**实际模型请求次数**、端到端耗时与
模型耗时、两者 P50/P95/min/max、usage 与**未知项**（未接入计费：能拿到 usage 就报，
费用一律 `unknown`；缺失不填 0）。JSON 与 Markdown 同时写入 `evals/reports/`。

### 14.5 本轮实测

| 命令 | 结果 |
| --- | --- |
| `run_eval.py --provider scripted --strategy bounded_agent --split dev` | **14/14**，失败 0，SKIPPED 0 |
| `run_eval.py --provider scripted --strategy bounded_agent --split holdout` | **4/4** |
| `run_eval.py --provider scripted --strategy fixed_workflow --split dev` | 13/13，**SKIPPED 1**（预算用例只在 bounded_agent 下有意义） |
| `run_eval.py --provider deterministic --strategy bounded_agent --split dev` | 9/9，**SKIPPED 5**（需要脚本输出的用例） |
| `run_eval.py --provider live --strategy bounded_agent --split dev` | **NOT_RUN 13**，退出码 **2**（无凭据；唯一执行的是不依赖模型的取消场景） |
| `pytest -q` | **208 passed**（PR-B 后 201 + 本轮新增 7） |
| `go test ./... -count=1` / `go vet ./...` / `gofmt -l` | 全部 ok / clean / clean |
| `npm test` / 递归 `node --check` | 2 passed / clean |

`SKIPPED` 与 `NOT_RUN` 的数量随 provider/strategy 变化，正是"参数真正生效"的直接证据。

### 14.6 未运行（不得当作通过）

真实模型（live）评测 **`NOT_RUN`**：本机未配置专用凭据与预算，因此没有任何 live 质量数字。
`go test -race`、PostgreSQL/Redis 集成、Playwright e2e 由 CI 提供证据。

---

## 15. P3（PR-D）：工作台展示、操作与真实浏览器验收

### 15.1 后端补的只读视图

`TaskView` 新增 `strategy` / `investigation` / `usage`；`investigation` 由 `bounded_agent` 的调查
结果填充，包含**决策者、停止原因、轮次与工具调用数、缺失的必需证据、usage 与工具结果摘要**
（`tool_observations`：工具名、成功/失败、类型、有界摘要、数据版本）。固定流程也会如实标注
`strategy=fixed_workflow`，不让界面误以为走了模型调查。

### 15.2 展示与操作

| 要求 | 实现 |
| --- | --- |
| 展示执行策略 / 调查动作 / 工具结果摘要 / 证据来源 / 停止原因 / 预算 | 新增「执行轨迹与预算」卡片：策略徽章、决策者、停止原因（可读文案）、轮次/工具调用、缺失证据、工具观察列表、token 预算（未知即写 unknown） |
| 补充材料 | 追问表单（现在显示**问题原文与示例**，并补齐 `planned_at_timezone` / `schema_snapshot` 字段） |
| 人工确认 | 「材料确认」卡片：确认人、时间、材料版本与内容哈希；重复确认后按钮置为"当前材料已确认" |
| 取消 | 运行中显示「停止」 |
| 受支持的恢复 | 有检查点时按钮是「从检查点恢复」（调用 `POST /resume`）；没有检查点时才显示「重新执行一次」——不把重跑说成续跑 |

### 15.3 四态必须一眼可分

- **模型建议**：灰底徽章「不参与放行判定」；
- **确定性检查**：绿/红徽章，并注明"仅为本地静态扫描的结论"；
- **材料确认**：新增紫色徽章「人工确认 ≠ 治理审批 ≠ 执行许可」，独立成卡；
- **治理审批**：独立一栏「治理审批 · 不在此处」，明示审批与通行证由治理服务完成。

顺带修正：`DRAFT_READY`「草案已生成 · 待人工确认」此前与 `RECEIVED`/`RUNNING` 同为蓝色，
现在使用专属紫色 tone，使"唯一需要人做决定的状态"不再与其他状态混在一起。

### 15.4 错误状态与不误诊

- 存储降级（`health.status==="degraded"`、`unpersisted_tasks`、`degraded_reason`）现在会**显示**，
  并明确写"这不是功能未启用"；
- 模型不可用时显示「模型不可用 · 确定性生成器」，说明这是可运行状态；
- 工具失败：确定性检查错误与调查轨迹里的工具观察都会展示；
- **修正 `refreshHealth` 的 503 判别**：与 `handleActionError` 一致，先看 `error.code`，
  只有 `SERVICE_UNAVAILABLE` 才走"未启用"；应用层 503 提示"未生效、可重试"，
  避免一次落盘抖动被误诊为"功能没开"并禁用整个面板。

### 15.5 不泄漏

页面只调用既有的 6 个接口，不请求 `/provider`、`/tools`；不读取也不显示任何密钥、Cookie 或
原始敏感数据；工具结果与证据片段一律经 `esc()` 转义后按纯文本展示；事件区明确写"不记录模型思维链"。

### 15.6 真实浏览器验收（真实前后端 + 真实登录）

`tests/manual/agent-workbench-acceptance.mjs`（**不放在 `tests/e2e/`**：CI 的 e2e 用
`compose.e2e.yml`，其中不含 agent-app）。真实启动：`bin/dbguard.exe`（18099，demo 账号、
隔离数据文件、`DBGUARD_AGENT_BASE_URL` 指向 agent-app）+ `uvicorn`（18091，`bounded_agent`、
stub 模型端点），Chromium 真实登录 `developer@example.com / Demo1234`：

```
[PASS] 真实登录成功（developer@example.com）
[PASS] 工作台可加载
[PASS] 展示实际执行策略
[PASS] 缺信息时停在待补充并给出追问表单
[PASS] 显示"从检查点恢复"而不是"重新执行一次"
[PASS] 展示执行轨迹与预算
[PASS] 预算区分已知/未知 — 未知（provider 未提供）
[PASS] 补充后从等待点继续并产出草案
[PASS] 入口节点只执行过一次（未从头重跑） — screen_input=1
[PASS] 四态区分：材料确认与治理审批各自成卡
[PASS] 可以人工确认材料且记录落库
[PASS] 重复确认被置为已确认（幂等）
[PASS] 保存桌面截图
[PASS] 运行中的任务可以取消
[PASS] 错误处理：空需求被拦下并给出提示
[PASS] 窄屏无横向溢出 — overflow=0px
[PASS] 保存窄屏截图
[PASS] 工作台无未捕获 JS 异常
[PASS] 工作台接口无 4xx/5xx
[INFO] 页面出现过的非 2xx 响应（含登录页预期 401）：401 /api/auth/session
结果：19/19 通过
```

> 登录页在未认证时会正常探测 `/api/auth/session` 并得到 401，这是控制台既有行为，
> 不属于工作台缺陷；断言因此限定在"工作台与 Agent 接口无 4xx/5xx + 无未捕获 JS 异常"。

截图（真实 Chromium，已随仓库保留）：
`docs/assets/agent-workbench-desktop.png`（1440×900）、`docs/assets/agent-workbench-narrow.png`（420×900）。
截图里可见：时间线（created → screen_input → check_info → resumed → retrieve_evidence → generate_draft
→ run_check → finalize → confirmed）、「执行轨迹与预算」中的策略/决策者/停止原因/预算 unknown、
工具观察、以及带紫色徽章的「材料确认」记录。

验收后已停止两个后台进程，未占用端口。

### 15.7 本轮命令与结果

| 命令 | 结果 |
| --- | --- |
| `pytest -q` | **210 passed**（PR-C 后 208 + 本轮新增 2） |
| `run_eval.py --provider scripted --strategy bounded_agent --split dev` | **14/14** |
| `go test ./... -count=1` | 全部包 **ok** |
| `go vet ./...` / `gofmt -l ./internal ./cmd` | clean / clean |
| `npm test` | 2 passed |
| 递归 `node --check`（含验收脚本） | clean |
| `tests/manual/agent-workbench-acceptance.mjs` | **19/19** |

### 15.8 未运行（不得当作通过）

真实模型（live）评测仍为 `NOT_RUN`（无凭据/预算）；`go test -race`、PostgreSQL/Redis 集成、
CI 的 Playwright e2e 由 CI 提供证据（CI e2e 栈不含 agent-app，因此不能作为本工作台的验收证据——
本工作台的证据是本节的真实浏览器验收）。

---

## 16. 剩余任务

| 阶段 | 状态 | 内容 |
| --- | --- | --- |
| P0 | ✅ 完成 | 授权、执行所有权与持久化诚实性、只读工具服务间认证；执行生命周期收尾 |
| P1 | ✅ 完成 | 重试分层、草案契约、方言边界、补充字段；受约束调查循环；provider 原生动作决策 |
| P2 | ✅ 完成 | PR-A：检查点持久化、节点级中断与恢复、恢复前重校验、独立进程验收（§12）；PR-B：材料确认记录（确认人/时间/版本+内容哈希、幂等、新修订失效）与 flaky 修复（§13）。任务书 §P2-8 迁移记为 N/A |
| P3 | ✅ 完成 | PR-C：评测 `--strategy`/`--provider` 真正生效、开发/保留集拆分、确定性评分与报告（§14）；PR-D：工作台展示与操作、四态区分、存储降级可见、真实浏览器验收 19/19 与截图（§15） |
