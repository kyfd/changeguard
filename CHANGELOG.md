# Changelog

## 3.0.9

精修变更准备工作台（`/agent/`）的视觉层级与响应式呈现；不改 API、交互逻辑、治理状态或安全语义。

### 界面修复

- 为三栏面板、状态徽章、时间线、SQL 与证据卡片补充更清晰的层级、语义色和细节样式；加深次级文字以满足浅色背景下的可读性。
- 修复 Linux Chromium 在 320px 视口下原生日期输入框固有宽度溢出，极窄屏下调整可选信息面板内边距。
- 保持材料确认紫色、失败红色、通过绿色的既有语义；保留键盘焦点轮廓和减少动态效果设置。

### 升级

使用 Release 的 `changeguard-3-0-9.tar.gz` 和 SHA256，按既有流程备份运行数据、环境配置及旧 release 指向。样式通过 `//go:embed web/*` 嵌入 `dbguard` 核心二进制，升级核心服务并强制刷新浏览器后生效；无数据迁移。

健康检查失败按升级脚本回滚上一版本；本版不修改 Agent 服务逻辑、审批、通行证或确定性放行规则。

### 已知限制

- 本版只改工作台 CSS；不会改变线上 Agent 功能、后端处理或治理判断。
- 本地以隔离模拟数据验收布局，不代表真实后端或生产部署验收；桌面与移动布局测试通过。

## 3.0.8

收紧变更准备 Agent（`/agent/`）的授权、模型调用记账与确定性 SQL 检查。治理判定不变：审批、通行证签发与消费规则未被修改，模型工具仍全部只读。

### Agent 授权与配置

- Compose 未给 Agent 配置治理回调地址，容器内默认 `127.0.0.1:8080` 指向自身，三个治理只读工具即使配了共享密钥也永远连不上核心服务；已显式指向 `dbguard`。
- 开启头身份模式但未配置 `AGENT_UPSTREAM_TOKEN` 时，此前会静默跳过上游认证，内网可达者可伪造 `X-Actor-Id` / `X-Org-Id` 读写任务。现在失败关闭，`/api/agent/*` 返回 503 并带机器可读错误码。
- `resume` 与 `clarify` 未过用量闸门却会重新调用模型，可绕过分钟/每日额度；现在先校验归属再扣配额，无权调用者不会消耗任务所有者的额度。

### 模型调用记账

- 改为在发出 HTTP 请求**之前**先持久化 `pending` 预占并保守计入预算，返回后原位结算。进程崩溃或取消时无法结算的记录保留为未知消耗，不再静默漏记。
- 恢复不再用新账本覆盖逐请求明细：保留有界调用历史与原始 `execution_id`，累计预算继续续算。
- 工具失败与决策失败现在终止调查，不再在证据已充足时继续生成草案。

### 确定性 SQL 检查

- 用词法分析替代脆弱正则，修复 `DELETE ... RETURNING`、带引号表名、字符串中 `'where'` 三类无条件 DML 漏检；未闭合引号、注释或括号失败关闭。
- `DO` / `CREATE FUNCTION` / `CREATE PROCEDURE` / `BEGIN ATOMIC` 的 dollar-quoted 过程体对词法扫描不透明，改为**要求人工复核**而不是当作已通过；字面量 `SELECT $$...$$` 不受影响。
- 修正 `FOR UPDATE`、`ON CONFLICT DO UPDATE`、`MERGE ... THEN UPDATE` 的误报，以及列名恰为 `function` / `procedure` 时被误判为过程体。

### 工作台（`/agent/`）

- 确认材料必须携带当前内容哈希；本地编辑 SQL 时禁用确认按钮，避免确认的是服务端原稿而非屏幕上看到的内容。
- 新增历史任务入口与 URL 恢复（只保存任务标识，不在浏览器保存 SQL、证据或身份）。

### 升级

1. 按既有流程备份主库、文件状态与密钥（`deploy/production/changeguard-backup.sh`）。
2. 用 Release 中的 `changeguard-3-0-8.tar.gz` 和 `.sha256` 配合 `deploy/upgrade/changeguard-upgrade.sh`（需 root）。
3. 核心二进制内含前端资源（`//go:embed web/*`），升级后强制刷新浏览器；本版无数据迁移。
4. **Agent 侧需要单独同步**：`agent-app/` 与 `docker-compose.yml` **不在**核心升级包内。Compose 部署需同时更新 `AGENT_GOVERNANCE_BASE_URL=http://dbguard:8080`，并确保 `AGENT_UPSTREAM_TOKEN` 非空，否则 `/api/agent/*` 会返回 503（核心治理服务不受影响）。
5. 行为变更：跨组织或非创建者的任务读写此前返回 200，现在统一返回 404。

健康检查失败由升级脚本回滚旧 release；回滚后需注意：跨组织读写、取消漏账与过程体漏检会一并恢复。

### 已知限制

- **真实模型（live）评测为 `NOT_RUN`**：本机无专用凭据与预算，本版没有任何模型质量结论。确定性评测只证明协议与安全边界，不代表起草质量。
- 过程体与 `MERGE` 的判定是**词法启发式**，不是完整 SQL 语义分析或真实数据库演练；子查询内的无条件 DML 仍保守上报。
- 单实例文件存储，不支持多副本；恢复对外部模型调用是 **at-least-once**，不承诺 exactly-once。
- 本版不改变审批、确定性放行、通行证签发或消费规则，也不给 Agent 增加任何写能力。

## 3.0.7

### 修复与接口

- Agent 时间线说明固定独占下一行，修复恢复事件长文本被挤成竖排的问题，并增加实际宽度回归。
- 新增按变更读取已记录发布结果的接口，复用组织与应用权限；缺失或无法判断的证据不算通过。
- 接入企业模型用量查询，区分已实现的模型配置与尚未实现的出站能力。
- 保留既有规则导出接口及其组织隔离，增加回归覆盖。

### 升级

使用 Release 的 `changeguard-3-0-7.tar.gz` 和 SHA256，升级前备份运行数据、环境配置及旧 release 指向。本版无数据迁移；核心二进制包含前端资源，升级后刷新页面。

健康检查失败按现有流程回滚旧 release；本版不修改审批、通行证或确定性放行规则。

### 已知限制

- 企业出站配置、事故回溯和 Agent 运行时接口尚未实现，不在本次发布中宣称可用。
- 模型用量仅表示核心运行时记录的调用额度，不是上游计费账单，也不代表 Python Agent 的完整 token 消耗。

## 3.0.6

### 界面修复

- Agent 表单根据容器宽度折列，补齐文本输入样式、日期宽度、键盘焦点与矮屏滚动；长进度和证据不再被横向裁切。
- 切换任务清理旧 SQL 编辑；轮询更新进度、证据及健康提示时保留未提交输入、焦点与滚动位置，过期响应不能覆盖新任务。
- 计划时间按填写的 IANA 时区转换，未填时区明确使用浏览器时区；创建与追问共用转换，草案按实际时区显示，拒绝非法日期和夏令时缺失/歧义时刻。
- 控制台修复手机通知面板越界、风险处理弹窗滚动、移动导航焦点与登录背景动画清理。
- 新增独立的 `npm run test:ui` 浏览器回归，并接入 CI 已安装 Chromium 的作业；隔离测试不代表生产后端验收。

### 升级

1. 按既有流程备份数据库、文件状态和密钥。
2. 使用 `changeguard-3-0-6.tar.gz` 及 Release 提供的 SHA256 调用 `deploy/upgrade/changeguard-upgrade.sh`。
3. 静态资源嵌在核心二进制内，需要升级核心服务并强制刷新浏览器；本版不需要数据迁移。

健康检查失败由升级脚本回滚上一版本；也可按既有手册切回旧 release。不要删除运行数据。

### 已知限制

- 界面回归使用隔离数据，真实模型、真实后端与生产部署需要分别验证。
- 时间转换依赖浏览器 Intl 时区数据库，支持 2000—2099 年；夏令时重复时刻需要改选无歧义的时间。本版不迁移已有排期。
- 本版不改变审批、确定性检查、通行证签发或消费规则。

## 3.0.5 - 2026-09-22

修复变更准备工作台（`/agent/`）对话栏的横向溢出。治理判定不变。

### 变更准备工作台（`/agent/`）

- 展开「可选：直接提供已知信息」后，日期控件的固有宽度不再把两列网格撑出对话栏。列、字段和输入框都可以收缩到父级宽度内。
- 过长的折叠标题会换行，不再把卡片撑宽。对话内容区只保留纵向滚动。

### 升级

1. 备份主库与文件状态（`deploy/production/changeguard-backup.sh`）。
2. 用 Release 中的 `changeguard-3-0-5.tar.gz` 和 `.sha256` 配合 `deploy/upgrade/changeguard-upgrade.sh`（需 root）。
3. 前端资源嵌在 `dbguard` 二进制内（`//go:embed web/*`），随本发行版一起生效，无需单独部署静态文件，也不需要同步 agent-app。

健康检查失败时，升级脚本仍会把 `current` 软链切回上一版本。回滚窗口内不要碰 `dbguard_state`。

### 已知限制

- `COMPLETED` 只表示通行证已消费，不代表生产部署成功。
- 本版只修正对话栏表单在窄列里的横向溢出，不改变追问、草案、审批或通行证行为。

## 3.0.4 - 2026-09-22

新增**企业自配模型接入**：管理员可在「集成设置 → 接入 AI」填写 OpenAI 兼容或 Anthropic 接口地址、API Key 与模型名，直接拉取上游可用模型列表，并先测试连通再保存。

### 新增能力

- 四个接口：`GET/PUT /api/enterprise/llm`（读状态/保存）、`POST /api/enterprise/llm/test`（连通性探测）、`POST /api/enterprise/llm/models`（拉取上游模型列表）、`GET /api/enterprise/llm/presets`（DeepSeek / OpenAI / Anthropic 预设）。
- 按企业隔离：每个组织各配各的，`OrganizationModelConfig` 落在 `store.state.model_configs`，读写都带 organizationID。
- 保存前强制探测：连不上的配置不会被保存，避免事后变成一个难以定位的故障。
- 已保存的配置接入 `agent.Runtime` 的 `SetResolver` 钩子（该钩子自 #14 起就存在但一直没有调用方）。解析不到企业配置时返回 false，Agent 走本地规则归纳，**不会**回退到平台 env 里的 Key。
- Anthropic Messages 形态使用 `x-api-key` + `anthropic-version`；它没有公开的模型列表端点，界面明确要求手动填写模型名，而不是发一个必然 404 的请求。

### 安全边界

- **API Key 永不回显**：落盘的是 AES-256-GCM 密文（`v1:` 前缀，随机 nonce），接口只返回一个用于辨认的尾缀提示。测试断言密文不含明文、两次加密结果不同、改动一个字节即解密失败。
- **主密钥缺失时整体失败关闭**：未配置 `CHANGEGUARD_SECRETS_MASTER_KEY` 时不加密、不保存、不返回明文，界面说明"要配什么、配在哪"。不会退化成明文存储。
- **主密钥更换后旧密文明确解不开**：返回 409 要求重新填写，而不是静默当成"没有 Key"。
- **SSRF 防护**：服务地址只允许 http/https，默认拒绝环回、私网、链路本地与云元数据端点（含 169.254.169.254、`.internal`、`.local`、CGNAT、TEST-NET）。不跟随跳转。内网部署可用 `CHANGEGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM=1` 显式放行私网，但链路本地**永远**拒绝。
- **上游错误不回显响应体**：4xx 的 body 常把 Authorization 头回显出来，透出等于泄露 Key；只按状态码分类成一句可读的话。
- 权限：仅企业管理员（`enterprise_admin` 或技术负责人）可写，其他人只读状态。每次保存/测试写审计，不记录 Key。

### 升级

1. 备份主库与文件状态（`deploy/production/changeguard-backup.sh`），**并单独备份 `CHANGEGUARD_SECRETS_MASTER_KEY`**——丢失它会导致所有已保存的企业 Key 无法解密。
2. 在 `/etc/changeguard/core.env` 增加 `CHANGEGUARD_SECRETS_MASTER_KEY=<至少 32 字节随机串>`。不配置则功能不可用（失败关闭），治理功能不受影响。
3. 用 Release 中的 `changeguard-3-0-4.tar.gz` 和 `.sha256` 配合 `deploy/upgrade/changeguard-upgrade.sh`（需 root）。
4. 存储变更：`store.state` 新增 `model_configs` 字段（`omitempty`）。旧数据文件加载时该字段为零值，已用生产数据副本验证可正常加载、既有 47 条变更记录完整。
5. 若模型网关在内网，额外设置 `CHANGEGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM=1`。

健康检查失败时，升级脚本仍会把 `current` 软链切回上一版本。回滚窗口内不要碰 `dbguard_state`。

### 已知限制

- 企业模型接入**不改变治理判定**：模型只提供参考建议，静态规则、审批状态、制品摘要与通行证签发仍由治理服务决定。
- Anthropic 形态**不参与 Agent 分析**：`internal/agent.Runtime` 目前只实现 OpenAI 兼容调用，因此界面可以保存 Anthropic 配置并测试连通，但分析不会用它（ resolver 对该形态返回 false）。这是显式边界，不是静默失败。
- 主密钥轮换**没有自动化**：更换后需每个企业重新填写 Key。后续可加"用旧密钥解密、用新密钥重加密"的迁移工具。
- 模型列表上限 200 条，超出部分截断。
- `COMPLETED` 只表示通行证已消费，不代表生产部署成功。
- 外部模型请求**不保证 exactly-once**：恢复是 at-least-once，消耗以调用账本为准。

## 3.0.3 - 2026-09-21

修复变更准备工作台（`/agent/`）的若干界面缺陷。治理侧行为不变：Agent 仍然只准备材料，不审批、不执行。

### 变更准备工作台（`/agent/`）

- **追问表单不再丢弃服务端已识别出的候选值**：`suggest_slots()` 从需求原文里确定性抽取的槽位建议（应用、环境、数据库、表名、计划时间）现在会预填进对应控件，并标注"已预填 · 这是建议值，不是已确认信息"。此前这些值随接口下发但前端从未读取，用户要把刚写过的东西重新敲一遍。用户改掉或清空都以用户为准，清空的值不会被提交。
- **本地编辑的失效提示现在真的会出现**：中栏"本地编辑未经验证"卡片原来按"渲染时是否已编辑"条件渲染，而编辑动作不重绘中栏、能重绘中栏的路径又都先清空编辑状态，导致它永远不出现。现在两栏的失效提示都常驻 DOM 只切换显隐。
- **本地编辑不再每次按键重绘右栏**：`markStale()` 原来调用 `renderEvidence()` 整体重建右栏（近 3000px 高），每敲一个字符都把用户的滚动位置打回顶部。现在只切换显隐与样式类。
- **非槽位追问不再渲染成"假输入框"**：`field="open_question"` 的条目（模型要求的补充说明、草案里的未解决问题）在 `ClarifyRequest` 里没有对应字段，提交后会被服务端静默丢弃。现在它们只作为"需要人工确认"的陈述展示，不再伪装成可提交的输入框。
- **新增「补充说明」输入框**：`ClarifyRequest.note` 是既有且已被服务端使用的字段（会写入任务记录并在下一次执行时拼进需求文本），但前端一直没有入口；现在追问卡片提供该输入框。
- **健康状态改为周期刷新**（5 秒）：此前只在页面加载时读一次，下游在页面打开后才恢复或才降级时，右上角状态、右栏降级卡片与创建按钮会一直停在加载时的那一帧上（创建按钮可能从此点不动）。轮询失败不再弹错误横幅。会话过期（401）与"服务不可达"现在分开提示。
- 进度时间线在非当天的事件上补日期，避免跨天或中断到第二天的进度看起来发生在同一时间；「本地编辑」复选框的命中区放大。

### 工程与发布

- `release.yml` 的前端语法检查从单层 glob 改为递归 `find`：`internal/httpapi/web/agent/*.js`（本次修复的文件）原先被静默漏掉，与 `ci.yml` 的 `quality-js` 作业保持一致。

### 升级

1. 备份主库与文件状态（`deploy/production/changeguard-backup.sh`）。
2. 用 Release 中的 `changeguard-3-0-3.tar.gz` 和 `.sha256` 配合 `deploy/upgrade/changeguard-upgrade.sh`（或 `deploy/production/changeguard-core-install.sh`，需 root）。
3. 前端资源嵌在 `dbguard` 二进制内（`//go:embed web/*`），随本发行版一起生效，无需单独部署静态文件。
4. **Agent 服务（agent-app）需要同步到本 commit 或更高版本**：预填数据来自 #34 引入的 `suggested` 字段，「补充说明」依赖 #31 引入的 `note` 字段。更早的 agent-app 上这两项会分别表现为"没有预填"和"说明被忽略"，但不会报错。
5. `AGENT_*` 只影响 Agent 侧；未配置 `DBGUARD_AGENT_BASE_URL` 时 `/api/agent/*` 仍返回 503，治理功能不受影响。

健康检查失败时，升级脚本仍会把 `current` 软链切回上一版本。回滚窗口内不要碰 `dbguard_state`。

### 已知限制

- `COMPLETED` 只表示通行证已消费，不代表生产部署成功。
- Agent 的任务记录（JSON）与图检查点（SQLite）都是**单实例**存储；多副本 / 分布式部署未支持。
- 外部模型请求**不保证 exactly-once**：恢复是 at-least-once，被中断的节点可能已经发出过请求，消耗以调用账本为准。
- **真实模型质量未验证**：live 评测是 `NOT_RUN`（无专用凭据与预算）。离线评测是确定性回归，不能作为"模型建议更好"或"模型检查更准"的证据；`fixed_workflow` 与 `bounded_agent` 的对照只经过 scripted provider。
- 控制台仍有若干面板没有对应服务端路由（结果信号、事故回溯、CI 信任、企业 LLM/出站、Agent 运行时、规则导出、影响图谱占位），详见 `docs/agent-ops-and-interview.md` §5，不计入已完成能力。
- 原子库和主库不能共用同一个 host:port；同一集群上的另一个库仍放不住。
- 审计链路是应用层防篡改，不是 WORM。没有代码签名。
- HTTP P50/P95 还没有可提交的测量。
## 3.0.2 - 2026-09-20

本版把「变更准备 Agent」纳入主线，并补齐发布与升级链路。治理侧仍是唯一权威：Agent 只**准备材料**，不审批、不执行，也不接触生产库。

### 变更准备 Agent（`/agent/`）

- 把需求整理成结构化草案，检索规范与历史案例并引用**可核对片段**，再做确定性静态扫描（并发建索引、锁超时、回滚缺失等）。模型建议**不参与放行判定**。
- 缺信息时停在**节点级中断**上追问，补充后从检查点继续（不是从头重跑）；跨进程恢复由真实进程验收（杀进程 → 重启 → 恢复）。
- 材料的人工确认记录确认人、时间、材料版本与内容哈希，重复确认幂等，材料变更后旧确认失效。**材料确认 ≠ 治理审批 ≠ 执行许可**。
- 调查策略可选 `fixed_workflow`（默认）或 `bounded_agent`：轮次、累计工具调用、单工具超时、任务 token／费用／请求次数上限都由代码强制，完成条件由确定性规则判定，模型说"查完了"不算数。
- 模型只能调用**只读**工具，白名单独立于工具注册表并有一致性测试；身份与授权只来自服务端注入，模型参数无法覆盖。
- 未配置模型凭据时退回确定性生成器——这是可运行状态，不是错误状态。

### 账本与预算

- 调用账本**按请求**记录并随每次调用落盘；取消、超时、网络失败同样计入，缺失 usage 显式记为"未知"，**不填 0 冒充已知**。
- 恢复、重试与进程重启**续算**同一任务预算，不重置；缺定价数据时费用上限失败关闭。
- 预算只决定"能不能发下一次请求"：已经付过钱的成功响应不会被预算检查丢弃。

### 工程与发布

- CI 增加 `quality-agent` 作业（agent-app 的 pytest 与嵌入式 JS 语法检查，缺依赖时报可执行提示）。CI 的 e2e 栈**不含** agent-app，不能作为工作台的验收证据。
- 新增 `release.yml`：推送 `vX.Y.Z` annotated tag 后校验（tag 指向 HEAD、可祖先到 `main`、gofmt／npm test／go test／vet／`-race`），产出离线升级包 `changeguard-X-Y-Z.tar.gz`、`.sha256` 与 CycloneDX SBOM，发布到 GitHub Releases。
- 文档新增 `docs/agent-ops-and-interview.md`（部署边界、设计说明、五分钟演示脚本、可引用事实清单）与 `docs/agent-upgrade-verification.md`（逐项验证记录，未运行项显式标注）。

### 修复

- 通行证 `consume` 对原 consumer 幂等。丢失 HTTP 200 后，同一 Token、制品摘要、环境和 `consumer` 重试返回首次公开快照，状态码仍是 200，并带 `Idempotency-Replayed: true`。不同 consumer 继续返回 `409 PASSPORT_REPLAY`。不写第二次消费审计，也不改 `consumed_at`。详见 [ADR 0001](docs/adr/0001-idempotent-passport-consume.md)。
- 模型重试分层：传输层与内容解析层不再共用一个开关（原先一次生成最多打 N² 次请求）；草案版本与修订说明由服务端写入；非 PostgreSQL 方言在入口拦下，不再产出方言不匹配的草案。
- 修掉一个发布版本可复现的并发竞态（取消与恢复在检查点读取处交错），以及启动恢复用例依赖 1ms 墙钟租约的偶发失败。
- 账本三处边界：请求被取消后漏记、usage 缺失被报成"已知的 0 费用"、最后一次被允许请求的成功响应被预算检查拒绝。

### 升级

1. 备份主库和文件状态（`deploy/production/changeguard-backup.sh`）。
2. 用 Release 里的 `changeguard-3-0-2.tar.gz` 和 `.sha256` 走 `deploy/upgrade/changeguard-upgrade.sh`（或 `deploy/production/changeguard-core-install.sh`，需 root）。
3. 治理侧的迁移与配置沿用 3.0.1：`deploy/migrations/002_core_authority_v3.sql`（可重复执行），环境变量优先 `CHANGEGUARD_*`。
4. Agent 是**可选**组件：`/agent/` 静态资源随包发布；未配置 `DBGUARD_AGENT_BASE_URL` 时 `/api/agent/*` 返回 503，其余功能不受影响。`AGENT_*` 只影响 Agent 侧。
5. Agent 自身不连生产库：表结构来自显式导入的快照，三个远程只读工具需要共享密钥 + 成员委托。

健康检查失败时，升级脚本会把 `current` 软链切回上一版。回滚窗口内不要删 `dbguard_state`。

### 已知限制

- `COMPLETED` 只表示通行证已消费，不表示生产部署成功。
- Agent 的任务记录（JSON）与图检查点（SQLite）都是**单实例**存储；多副本 / 分布式部署未支持。
- 外部模型请求**不保证 exactly-once**：恢复是 at-least-once，被中断的节点可能已经发出过请求，消耗以调用账本为准。
- **真实模型质量未验证**：live 评测为 `NOT_RUN`（无专用凭据与预算）。离线评测是确定性回归，不能作为起草质量或"模型调查更好"的证据；`fixed_workflow` 与 `bounded_agent` 的对照只跑过 scripted provider。
- 控制台仍有若干面板没有对应服务端路由（结果信号、事故回溯、CI 信任、企业 LLM/出站、Agent 运行时、规则导出、影响图谱占位），详见 `docs/agent-ops-and-interview.md` §5，不得计入已完成能力。
- 影子库和主库不能共用同一个 host:port；同一集群上的另一个库仍挡不住。
- 审计链是应用级防篡改，不是 WORM。没有代码签名。
- HTTP P50/P95 还没有可提交的测量。

## 3.0.1 - 2026-09-02

`main` 成为唯一开发主线。`v3.0.1` 从 `main` 的 annotated tag 构建。

Go 最低 1.25，CI 在 1.25 和 1.26 上跑测试。`npm test`、gofmt、staticcheck 进 quality。Trivy Action 钉在 `v0.36.0` 的 commit SHA。Release tag 必须能祖先到 `origin/main`。

### 升级

1. 备份主库和文件状态。
2. 应用 `deploy/migrations/002_core_authority_v3.sql`（可重复执行）。
3. 用 Release 里的 `changeguard-3-0-1.tar.gz` 和 `.sha256` 走 `deploy/upgrade/changeguard-upgrade.sh`。
4. 环境变量优先 `CHANGEGUARD_*`。旧的 `DBGUARD_*` 仍可读，启动时会告警，计划在 v4.0 删除。
5. Go module 现为 `github.com/kyfd/changeguard`。服务入口和发布二进制暂时仍叫 `dbguard`。

健康检查失败时，升级脚本会把 `current` 软链切回上一版。回滚窗口内不要删 `dbguard_state`。

### 已知限制

- `COMPLETED` 只表示通行证已消费，不表示生产部署成功。
- 影子库和主库不能共用同一个 host:port；同一集群上的另一个库仍挡不住。
- 审计链是应用级防篡改，不是 WORM。
- 没有代码签名。
- HTTP P50/P95 还没有可提交的测量。

## 3.0.0 - 2026-08-29

### PostgreSQL / Redis 集成测试

CI 拉起 Postgres 16 和 Redis 7.4，分别跑：

- `TestPostgresNormalizedMultiInstance`
- `TestRedisSessionRepositoryIntegration`

缺 DSN 或服务不健康时失败，不再 `SKIP`。

### 端到端测试

`compose.e2e.yml` + Playwright：开发账号登录、提交 CONFIG+SQL 变更、静态检查、PostgreSQL 影子验证通过、进入待审批。见 `tests/e2e/config-sql-golden.spec.js`。

### 数据迁移

`002_core_authority_v3.sql` 只扩展不收缩：

- `changeguard_changes` 增加 status / application_id / artifact_sha256 / 时间列
- `changeguard_audit_events` 增加 per-org sequence
- 新增 `changeguard_core_authority`
- `dbguard_state` 保留作回滚见证

### Release / 供应链

打 annotated `v*` tag 后：gofmt、测试、vet、race、verification.json、离线构建、SBOM、tar.gz + SHA256。govulncheck 和 Trivy 在 CI 的 supply-chain 任务里。

### 未验证

- consume 成功后的实际部署结果尚未进入 Gate 状态
- 影子库误连同集群非主库
- 审计日志被 DBA 删表
- 前端完整权限矩阵和可达性
