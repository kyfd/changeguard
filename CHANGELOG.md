# Changelog

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
