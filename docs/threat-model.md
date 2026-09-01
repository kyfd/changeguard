# ChangeGuard 威胁模型

本文把现有设计整理成招聘者和审阅者能直接核对的安全论证。每一行都对应代码或测试，而不是愿望清单。

范围：变更提交、规则检查、PostgreSQL 影子验证、审批、一次性通行证、CI verify/consume、审计链。不覆盖操作系统加固、云账号接管或物理访问。

| 威胁 | 当前防护 | 剩余风险 | 验证测试 |
| --- | --- | --- | --- |
| 审批后篡改制品（TOCTOU） | 制品按原始字节计算 SHA-256；通行证绑定该摘要、环境与规则版本。CI 用 `changeguard-gate` 对真实文件重算摘要，不一致则拒绝 `verify` / `consume`。 | CI 脚本如果哈希的是 A、部署的是 B，门禁看不到。ChangeGuard 不接管 Git 或集群。 | `internal/changegate` 完整性测试；`cmd/changeguard-gate` digest/verify 测试 |
| 通行证重放 / 并发消费 | Token 只存 SHA-256，明文只在签发响应出现一次。`UsePassport` 在写锁（文件存储）或条件 UPDATE（PostgreSQL）下把状态从 ACTIVE 改为 CONSUMED，并把变更原子标为 COMPLETED。 | `COMPLETED` 只表示通行证已用掉，不表示生产部署成功。consume 成功但发布失败时需要新的变更单。 | `TestUsePassportConcurrentConsumeCompletesChangeOnce`（100 goroutine，只允许 1 次成功） |
| Token 泄露 | HMAC-SHA256 签名（`cg1.` 前缀）；弱密钥拒绝签发；Bearer 与浏览器会话分离；生产要求 `CHANGEGUARD_PASSPORT_HMAC_SECRET` ≥ 32 字节且非占位符。 | 第一次响应、CI 日志或调试代理仍可能把明文打出来。调用方必须把 Token 放进 masked secret。 | `TestSignerRejectsWeakSecretAndTampering`；`TestTokenSHA256DoesNotExposeToken` |
| 摘要碰撞 / 算法无法升级 | 制品与审计链使用 SHA-256；通行证签名是 HMAC-SHA256。 | 没有算法标识协商。SHA-256 碰撞仍属理论风险；升级摘要算法需要新的通行证版本字段。 | 签发/校验 round-trip 测试 |
| 提交人与审批人串谋 | 服务端禁止自审；高风险需要独立复核；角色与应用授权分离。 | 两个真实的人可以串通。这是组织问题，产品只能保证不是同一个人点两次。 | 服务层审批与权限测试 |
| 数据库事务失败导致半更新 | 文件存储在 `saveLocked` 失败时回滚内存快照。PostgreSQL 核心表与审计在同一条权威路径上更新；v3 迁移是 expand-only，`dbguard_state` 仍作回滚见证。 | 进程在 fsync 之后、响应之前崩溃时，客户端可能收不到成功响应，但状态已经前进。调用方必须按通行证状态重试，而不是再发一次签发。 | `TestUsePassportSaveFailureRollsBackPassportAndChange`；PostgreSQL 多实例契约测试 |
| 重复消息 / 非幂等 | 幂等记录有唯一主键；Outbox 用 `FOR UPDATE SKIP LOCKED` 认领。 | 没有 Idempotency-Key 的客户端重试仍可能产生重复变更单。 | `internal/store` 与 `internal/service` 幂等测试 |
| 审计日志被删除或改写 | 审计事件按组织链接前序哈希；离线 `changeguard-evidence verify` 可核对链。新事件空 hash 会被拒绝。 | 这是应用级防篡改，不是 WORM 或第三方时间戳。能写数据库的人仍可能删表。 | `internal/store` 审计链测试；`internal/evidence` bundle 测试 |
| 影子库误连生产库 | 生产配置与运行时都会比较 `CHANGEGUARD_SHADOW_DSN` 和 `CHANGEGUARD_PRIMARY_DSN` 的 host:port，相同则拒绝。影子 SQL 拒绝事务逃逸和 psql meta command。 | 同一主机上的“另一个数据库”仍可能在同一集群。无法从 DSN 判断对方是不是生产副本。必须用独立凭证和独立实例。 | `TestProductionProfileRejectsShadowOnPrimaryHost`；`TestPostgresModeRejectsShadowOnPrimaryHost`；`TestValidateExperimentSQLRejectsTransactionEscape` |
| CI verify 成功但 consume 失败 | verify 只读校验摘要/环境/规则/有效期；consume 才做一次状态转换。失败保持 fail-closed。 | 流水线如果忽略非零退出码，门禁无效。 | Gate 命令测试；通行证状态机测试 |
| consume 成功但实际部署失败 | README 和产品文档明确：`COMPLETED` ≠ 生产正常。后续要靠独立的 operations webhook / 证据包。 | 没有和 Kubernetes 滚动发布做两阶段提交。 | 文档约束；operations webhook 测试 |
| 演示数据被当成生产证据 | 未执行的影子验证标记为 `NOT_RUN` / `DEMO_ONLY`，不能签发生产通行证。生产 profile 禁止演示账号。 | 操作者仍可能在生产打开演示开关——生产校验会拒绝。 | `TestSimulationIsExplicitlyNotRun`；生产 profile 测试 |
| 配置里的明文密钥进入审计 | 制品入库前脱敏；证据包排除 SQL 正文、明文 Token 和 Token hash。 | 脱敏靠启发式，未知密钥名可能漏网。 | `changegate.Redact` 测试；evidence bundle 测试 |

## 明确不承诺的能力

- 不连接生产库执行 SQL
- 不接管 Git、Jenkins 或 Kubernetes
- 不把模型解释层当作审批依据
- 不把演示模式的耗时、锁等待或 P99 包装成生产证据
