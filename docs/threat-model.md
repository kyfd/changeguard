# ChangeGuard 威胁模型

范围包括变更提交、确定性检查、PostgreSQL 影子验证、审批、一次性通行证、CI `verify`/`consume` 和审计链。不包括操作系统或云账号被完全接管、物理访问，以及 ChangeGuard 之外未回传的部署行为。

| 威胁 | 当前防护 | 剩余风险 | 相关验证 |
| --- | --- | --- | --- |
| 审批后篡改制品（TOCTOU） | 制品按原始字节计算 SHA-256；通行证绑定摘要、环境和规则版本。`changeguard-gate` 从实际文件重算摘要，不一致时拒绝。 | CI 如果对 A 计算摘要却部署 B，ChangeGuard 无法发现。Gate 步骤必须与部署使用同一工作区。 | `internal/changegate` 完整性测试；Gate digest/verify 测试 |
| 通行证重放或并发消费 | 明文 Token 只返回一次，服务端只存 SHA-256。文件模式在写锁下、PostgreSQL 模式通过条件 UPDATE 将 `ACTIVE` 改为 `CONSUMED`，并将变更原子标为 `COMPLETED`。同一 consumer 在丢失响应后重试会得到首次快照，不写第二次消费审计；不同 consumer 仍返回 `PASSPORT_REPLAY`。 | `COMPLETED` 只表示 Gate 已消费。消费后部署失败需要重新提交和审批，不能把 Token 交给另一条流水线。 | `TestUsePassportConcurrentConsumeCompletesChangeOnce`；`TestUsePassportSameConsumerReplayKeepsFirstSnapshot` |
| Token 泄露 | HMAC-SHA256 签名，弱密钥拒绝签发；CI Bearer 与浏览器会话分离；生产 HMAC secret 要求至少 32 字节且不能是占位符。 | 签发响应、CI 日志、进程参数或调试代理仍可能泄露明文。调用方必须使用 masked secret 和环境变量。 | `TestSignerRejectsWeakSecretAndTampering`；`TestTokenSHA256DoesNotExposeToken` |
| 摘要算法升级困难 | 制品和审计链使用 SHA-256，通行证使用 HMAC-SHA256。 | 当前没有摘要算法协商。算法迁移需要新的协议或通行证版本。 | 签发与校验 round-trip 测试 |
| 提交人绕过独立审批 | 服务端禁止自审，应用授权和组织角色分别校验，高风险审批要求独立复核。 | 两个授权人员仍可能串通；这超出技术控制能单独解决的范围。 | 服务层审批和权限测试 |
| 持久化失败造成半更新 | 文件存储保存失败时恢复内存快照；PostgreSQL 核心更新和审计走事务路径。首次消费成功后丢失 HTTP 响应时，原 consumer 重试 `consume` 可确认同一快照。 | 服务可能在状态落盘后、响应返回前崩溃。不同 consumer 仍不能把丢失的 200 当成可再次部署的许可。 | `TestUsePassportSaveFailureRollsBackPassportAndChange`；PostgreSQL 契约测试 |
| 重复请求或消息 | 幂等记录使用唯一主键；Outbox 通过 `FOR UPDATE SKIP LOCKED` 领取。 | 未提供幂等键的创建请求仍可能产生重复业务对象。 | `internal/store` 和 `internal/service` 幂等测试 |
| 审计事件被改写或删除 | 新审计事件按组织链接前序哈希；`changeguard-evidence verify` 可离线核对链。 | 这是应用级篡改检测，不是 WORM 或第三方见证。具备数据库管理权限的人仍可删除整段数据。 | Store 审计链测试；evidence bundle 测试 |
| 影子库误连生产 | 配置和运行时比较 shadow 与 primary DSN 的 host:port，相同则拒绝；影子 SQL 拒绝事务逃逸和 psql meta command。 | 不同数据库名可能仍在同一 PostgreSQL 集群；仅从 DSN 无法判断是否是生产副本。必须使用独立实例、凭据和网络边界。 | `TestProductionProfileRejectsShadowOnPrimaryHost`；`TestPostgresModeRejectsShadowOnPrimaryHost`；SQL 校验测试 |
| `verify` 成功后 `consume` 失败 | `verify` 只读；`consume` 才执行一次性状态转换。任何失败均保持关闭。 | 流水线忽略非零退出码会使门禁失效。 | Gate 命令和通行证状态机测试 |
| `consume` 成功后部署失败 | 文档和 API 语义明确区分 Gate 消费与部署结果；外部流水线和运维事件单独记录。 | 没有与部署目标建立两阶段提交。 | operations webhook 测试 |
| 演示结果被误作生产证据 | `NOT_RUN` 和 `DEMO_ONLY` 不能推进 SQL 审批或签发生产通行证；production profile 拒绝演示账号和演示数据。 | 非生产 profile 仍需由部署方隔离，避免被误接入生产流水线。 | `TestSimulationIsExplicitlyNotRun`；production profile 测试 |
| 配置敏感值进入持久化或导出 | 制品保存前执行脱敏；证据包排除制品正文、明文 Token 和 Token 摘要。 | 脱敏是启发式的，未知字段名或编码形式可能漏检。ChangeGuard 不能作为密钥存储。 | `changegate.Redact` 测试；evidence bundle 测试 |
| 模型分析影响放行 | 模型模块使用只读工具，建议风险与治理风险分离，不具备审批、签发、消费或部署能力。 | 模型输出仍可能误导人工判断，界面必须标明其建议性质并保留来源。 | Agent 工具许可和离线协议测试 |

## 明确边界

- 不连接生产库执行 SQL；
- 不接管 Git、CI/CD 或 Kubernetes；
- 不把模型解释作为审批依据；
- 不把影子数据库结果描述为生产性能或业务正确性；
- 不把 `COMPLETED` 描述为部署成功；
- 不把应用级哈希链描述为第三方不可抵赖证明。
