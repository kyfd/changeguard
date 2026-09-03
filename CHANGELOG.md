# Changelog

## Unreleased

Gate `consume` 对原 consumer 幂等。丢失 HTTP 200 后，同一 Token、制品摘要、环境和 `consumer` 重试返回首次公开快照，状态码仍是 200，并带 `Idempotency-Replayed: true`。不同 consumer 继续返回 `409 PASSPORT_REPLAY`。不写第二次消费审计，也不改 `consumed_at`。详见 [ADR 0001](docs/adr/0001-idempotent-passport-consume.md)。

`COMPLETED` 仍然只表示通行证已消费。

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
