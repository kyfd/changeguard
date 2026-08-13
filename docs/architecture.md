# ChangeGuard 系统架构

## 1. 文档状态

本文描述 ChangeGuard v1 的产品架构和可信证据规则。仓库继续保留 dbguard 历史命名；experiment 与 outbox 用于 SQL 的真实 PostgreSQL 影子事务，agent 仅是兼容的可选分析模块。产品不承诺影子压测、AI 自动审批或复杂事件平台。

目标架构只围绕一个核心结果设计：**生产流水线只能部署与已检查、已批准制品一致的内容。**

## 2. 设计原则

1. **确定性优先**：相同制品、相同规则版本应得到相同结果。
2. **证据不越界**：静态检查不包装成运行验证；未执行的指标不生成。
3. **失败关闭**：生产门禁超时、异常或校验不一致时默认阻断。
4. **最小集成**：复用现有 Git、CI/CD 和部署工具，不接管构建或发布。
5. **权限分离**：提交人不能自审，管理权限与应用级提交/审核权限分离。
6. **敏感信息最小化**：制品摘要用于绑定；通行证只存哈希，原始令牌只返回一次。

## 3. 逻辑架构

~~~mermaid
flowchart LR
    DEV["开发者浏览器"] --> API["HTTP API / 会话鉴权"]
    REV["审核人浏览器"] --> API
    CI["Jenkins / GitLab CI"] --> GATE["Gate API / Bearer 通行证"]

    API --> SVC["变更与权限服务"]
    GATE --> SVC
    SVC --> CHECK["确定性规则检查器"]
    SVC --> PASS["通行证签发与原子消费"]
    SVC --> AUDIT["审计时间线"]

    CHECK --> DATA["文件或 PostgreSQL 存储"]
    PASS --> DATA
    AUDIT --> DATA
    API --> SESSION["内存或 Redis 会话"]
    API --> OBS["健康检查 / 指标 / 日志"]
~~~

## 4. 组件职责

### 4.1 内置前端

提供创建变更、查看检查结果、整改、审批、通行证状态和审计时间线的主流程。前端只展示服务端授权后的数据，不通过隐藏按钮代替权限校验。

### 4.2 HTTP API 与认证

- 提供本地登录、会话、OIDC、企业成员和应用接口；
- 对浏览器写操作执行会话与 CSRF 校验；
- 对 CI 消费接口使用独立 Bearer 通行证，不复用用户会话；
- 统一请求体大小、错误语义、安全响应头和访问日志。

### 4.3 变更与权限服务

负责状态迁移、角色/应用授权、禁止自审、检查批次、发现项、审批和审计。所有关键校验必须在服务端执行，并在一次状态变更中保存业务数据和审计证据。

### 4.3.1 统一审计与 Evidence Bundle

`AuditEvent` 使用统一 schema 记录 request、actor/auth、resource/version、request digest、result/reason 以及 related event、attempt、passport 关联。Store 的所有审计追加点经过同一规范化入口，并在持有写锁时按企业链接前序事件；canonical payload 明确排除 `hash` 自身。持久化失败时 Store 从已持久化快照恢复业务状态和审计尾部。旧事件可以没有 hash，但只能位于企业链的历史前缀；新哈希链开始后的空 hash 会被离线校验拒绝。

`internal/evidence` 将一个 change 的制品摘要、规则/check/findings、演练、审批、通行证公开元数据、CI event/outcome 和 audit chain proof 打包为 JSON。目标审计摘要只重复 `id/hash/prev_hash/canonical_digest` 这些可与 proof 逐字段核对的链事实；Action、Result、actor、版本等 canonical 语义既不能在脱敏后由单向 digest 证明，也不应泄漏原文，因此不在审计摘要中重复，相关业务语义由 change/check/passport 等 Manifest section 提供。Manifest 绑定企业、change ID、change digest、生成时间和各 section 的 compact-JSON SHA-256。`cmd/changeguard-evidence export` 使用严格、只读的文件快照加载，不保存或创建源状态；`verify` 不需要服务端、数据库或签名 secret，即可验证 Manifest、change binding 和 audit chain。

证据包按结构排除 artifact/SQL 正文、明文 Token 和 Token hash，自由文本使用统一脱敏器。这里的 SHA-256 链仅提供应用级篡改检测，并非第三方见证或密码学不可抵赖；外部 WORM、透明日志或可信时间戳锚定仍是后续能力。

PostgreSQL 模式已拆出 `changeguard_changes`（每变更一行 JSONB）、`changeguard_outbox`、`changeguard_passports`、`changeguard_audit_events` 和 `changeguard_idempotency_records`。Outbox claim 使用 `FOR UPDATE SKIP LOCKED`，幂等 claim 依赖唯一主键，通行证消费使用条件 UPDATE，审计按组织事务 advisory lock 串行链尾。`dbguard_state` 仍保留完整 legacy JSONB，作为兼容 fallback 与迁移见证；组织、成员、应用、规则、集成事件、结果信号和 Agent 数据等低频实体仍只在 legacy state。普通 Store 保存产生的核心表内容是兼容投影，不应视为全部业务已经规范化或可独立删除 legacy state。

### 4.4 确定性规则检查器

v1 只处理三类生产制品：

- DATABASE：SQL 迁移与回滚说明；
- CONFIG：YAML、JSON 或 ENV 风格配置；
- KUBERNETES：工作负载和服务清单。

规则输出至少包含规则编号、严重级别、是否阻断、制品、命中位置、证据片段和整改建议。规则不得生成没有真实测量来源的耗时、锁等待、扫描行数或 P99。

### 4.5 变更通行证

通行证是 v1 的关键付费能力，目标职责如下：

- 仅在变更满足检查与审批条件后签发；
- 绑定组织、应用、环境、变更版本、规则版本和制品摘要；
- 原始令牌只返回一次，持久化层只保存强哈希；
- 支持过期和人工吊销；
- CI 消费时原子校验并标记已用，防止并发重放；
- 签发、成功消费、过期状态落盘和吊销产生业务审计事件；HTTP 访问日志只记录最小请求元数据且不得包含原始 Token。

目标 API 契约见 [CI/CD 接入指南](ci-integration.md)。

### 4.6 存储与会话

- 本地演示可使用文件存储和内存会话，降低启动成本；
- 团队部署可使用 PostgreSQL 保存业务数据、Redis 保存会话；
- 数据库约束或事务应保证状态迁移和通行证消费的一致性；
- 备份范围至少包含企业、成员、应用、规则、变更、发现项、通行证元数据和审计事件。

### 4.7 可观测性

提供存活、就绪和受保护指标端点。指标用于观察服务本身，不用于伪造变更风险证据。日志不得包含密码、OIDC token、Cookie、原始通行证或疑似敏感制品正文。

## 5. 目标状态机

~~~mermaid
stateDiagram-v2
    [*] --> DRAFT
    DRAFT --> CHECKING: 提交或重新检查
    CHECKING --> CHECK_FAILED: 存在阻断项
    CHECKING --> WAITING_APPROVAL: 配置/K8s 无阻断项
    CHECKING --> READY_FOR_EXPERIMENT: SQL 无阻断项
    CHECK_FAILED --> CHECKING: 更新制品后重检
    READY_FOR_EXPERIMENT --> EXPERIMENT_QUEUED: 请求影子验证
    EXPERIMENT_QUEUED --> EXPERIMENT_RUNNING: Worker 领取
    EXPERIMENT_RUNNING --> WAITING_APPROVAL: PostgreSQL SQL/回滚均通过
    EXPERIMENT_RUNNING --> CHECK_FAILED: 验证失败
    WAITING_APPROVAL --> REJECTED: 审核拒绝
    WAITING_APPROVAL --> APPROVED: 审核通过
    APPROVED --> DRAFT: 修改制品或关键元数据，撤销旧审批与通行证
    APPROVED --> COMPLETED: CI 原子消费通行证，治理闸门闭环
    REJECTED --> DRAFT: 重新编辑
~~~

核心约束：

- SQL 演练 Outbox 的一次业务尝试由稳定 `attempt_id` 标识；每次领取（包括租约过期接管）递增 `lease_generation`，它同时是 fencing token。续租、失败、完成和最终报告写入都必须匹配 worker、generation 且租约仍有效，旧 worker 即使稍后返回也不能覆盖新 generation 的结果。
- Outbox 持久化真实 runner 边界 `PREPARE → APPLY → FINALIZE`、阶段开始/更新时间、`input_sha256` 和 `result_digest`。当前 Runner 的 APPLY 是包含执行 SQL 与回滚 SQL 的单体隔离 PostgreSQL 事务，不声称支持语句级或步骤级恢复；进程中断或租约过期后，新 generation 从新的隔离事务重新执行整个 APPLY。
- `FINALIZE` 在同一存储原子写中校验当前 generation、绑定输入摘要，并写入变更报告和结果摘要。同一 `attempt_id` 与相同结果的重复 finalize 幂等；输入或结果摘要不一致时失败关闭。服务启动只补建缺失的活动 Outbox，并接管已过期 lease；已经持久化 FINALIZE 结果的事件不会重新执行 APPLY。
- 制品内容、目标环境、应用或规则版本发生变化后，旧审批和旧通行证失效；
- CHECK_FAILED 不能批准；
- 提交人不能批准自己的变更；
- COMPLETED 不能再次签发或消费通行证；
- SQL 必须经过 READY_FOR_EXPERIMENT、EXPERIMENT_QUEUED、EXPERIMENT_RUNNING；只有真实 PostgreSQL 影子事务中 SQL 与回滚脚本均成功，才能进入 WAITING_APPROVAL。配置和 Kubernetes 不伪造运行时演练，确定性检查无阻断项即可进入 WAITING_APPROVAL。

## 6. 可信证据模型

| 证据 | 产生方式 | 可以证明 | 不能证明 |
| --- | --- | --- | --- |
| 制品 SHA-256 | 对原始文件字节计算，并把制品元数据、环境与回滚内容纳入聚合摘要 | 审批与部署内容是否一致 | 内容本身一定安全 |
| 规则发现项 | 确定性静态检查 | 是否命中已知文本风险模式 | 生产性能与业务正确性 |
| 人工审批 | 授权审核人操作 | 指定人员接受当前风险 | 事故一定不会发生 |
| 通行证消费 | CI 在部署前原子校验 | 已批准制品被用于本次发布 | 部署后的可用性 |
| 审计事件 | 服务端状态变更时记录 | 谁在何时执行了什么操作 | 外部系统未记录的事实 |

任何页面、报告或 API 都应沿用这套措辞。

## 7. 安全边界

- ChangeGuard 不执行生产 SQL，不连接生产 Kubernetes 控制面；
- 不保存真实密钥；配置检查命中后应只保留最小必要证据；
- CI Gate 必须使用 HTTPS，生产环境限制网络来源；
- 浏览器会话与 CI 通行证是两类凭据，生命周期和权限不同；
- OIDC 登录必须校验 issuer、audience、state、nonce 和 PKCE；
- 组织 ID 和应用授权在每次读写时校验，不能只依赖前端过滤。

## 8. 部署形态

### 本地演示

单个 Go 进程、文件存储、内存会话。适合简历演示和功能验证，不作为高可用生产方案。

### 小型企业部署

Nginx/Ingress + 一个或多个 ChangeGuard 实例 + PostgreSQL + Redis。CI 通过内网或受控 HTTPS 入口访问 Gate API。

只有在通行证消费具备数据库级原子性、实例无状态、健康检查和备份恢复完成验证后，才适合横向扩展。

## 9. 历史兼容与迁移

Go module、cmd/dbguard、DBGUARD_ 环境变量和部分数据结构继续保留 dbguard 名称，避免破坏导入路径和既有部署。

历史模拟 experiment 或 agent 输出不得作为批准依据；当前只有 mode=POSTGRES 且 SQL 与回滚脚本均真实执行成功的报告可以推进 SQL 审批。兼容代码应满足以下最低规则：

- 不返回随机或固定伪造的“通过”指标；
- 没有真实执行证据时明确标记为未执行或不适用；
- 不因可选 AI 服务不可用而影响确定性检查；
- 迁移历史模拟记录时不得自动进入 WAITING_APPROVAL 或 APPROVED；当前 SQL 的 READY_FOR_EXPERIMENT、EXPERIMENT_QUEUED、EXPERIMENT_RUNNING 状态继续作为真实影子验证流程使用。
