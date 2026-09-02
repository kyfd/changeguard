# ChangeGuard 部署与运维

ChangeGuard 不执行生产 SQL，也不操作 Kubernetes。生产部署通常位于受控 HTTPS 入口之后，使用 PostgreSQL 保存业务状态、Redis 保存会话，并使用与生产隔离的 PostgreSQL 实例执行 SQL 影子验证。

```text
Browser / CI
     |
 HTTPS ingress or Nginx
     |
ChangeGuard instance(s)
     |-- PostgreSQL: organizations, changes, passports, audit
     |-- Redis: sessions and login rate limits
     `-- isolated PostgreSQL: SQL shadow validation
```

本地功能验证可使用 file + memory。多实例部署前必须验证共享会话、数据库级原子消费、迁移兼容和备份恢复。

## 启动方式

### 最小本地运行

```powershell
$env:CHANGEGUARD_AUTH_MODE = "local"
$env:CHANGEGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:CHANGEGUARD_ENABLE_DEMO_DATA = "true"
$env:CHANGEGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
go run ./cmd/dbguard
```

打开 <http://localhost:8080>。此方式默认使用文件存储和内存会话；未配置影子库时 SQL 只有静态检查。按 Ctrl+C 让服务处理退出信号。

### Compose

```powershell
Copy-Item .env.example .env
# 修改 HMAC secret 和数据库密码
docker compose up --build
```

Compose 启动 PostgreSQL、Redis 和影子 PostgreSQL。停止并保留数据：

```powershell
docker compose down
```

不要在未确认卷名和备份状态时删除数据卷。

## 环境变量

应用优先接受 `CHANGEGUARD_*`。Compose 和 Kubernetes 示例已经使用该前缀；`deploy/production` 下的 systemd 环境模板暂时保留 `DBGUARD_*` 兼容键。旧前缀计划在 v4.0 删除，在迁移完成前不要在同一环境为两个前缀设置冲突值。下表按当前 systemd 模板列出键名。

| 变量 | 用途 | 生产要求 |
| --- | --- | --- |
| `DBGUARD_LISTEN_ADDRESS` | HTTP 监听地址 | 显式绑定 loopback，例如 `127.0.0.1:8080` |
| `DBGUARD_ENV_FILE` | 核心自行复核的规范环境文件 | systemd 外部设置为 `/etc/changeguard/core.env`，运行用户不可写 |
| `DBGUARD_ENV_PROFILE` | `development`、`staging`、`production` | 生产固定为 `production` |
| `DBGUARD_STORE_MODE` | `file` 或 `postgres` | 使用 `postgres` |
| `DBGUARD_DATA_FILE` | file 模式状态文件 | 使用持久卷绝对路径 |
| `DBGUARD_MIGRATION_WITNESS_FILE` | file 模式迁移证据侧车 | 与主状态同卷保存，不单独删除 |
| `DBGUARD_PRIMARY_DSN` | 业务 PostgreSQL DSN | 从 secret 注入 |
| `DBGUARD_DB_MAX_CONNS` | 单实例最大数据库连接数 | 结合实例数和数据库容量设置 |
| `DBGUARD_SESSION_MODE` | `memory` 或 `redis` | 多实例使用 `redis` |
| `DBGUARD_REDIS_URL` | Redis 地址 | ACL 用户、至少 16 字节密码；非 loopback 使用 `rediss://` |
| `DBGUARD_REDIS_PREFIX` | Redis key namespace | 每个环境独立并以冒号结尾 |
| `DBGUARD_AUTH_MODE` | `local`、`oidc`、`hybrid` | 优先 `oidc` 或 `hybrid` |
| `DBGUARD_ENABLE_DEMO_ACCOUNTS` | 创建演示账号 | `false` |
| `DBGUARD_ENABLE_DEMO_DATA` | 加载演示数据 | `false` |
| `DBGUARD_PUBLIC_URL` | 外部地址 | HTTPS，且与 OIDC 回调一致 |
| `DBGUARD_AUTH_SECURE_COOKIE` | Secure Cookie | `true` |
| `DBGUARD_TRUST_PROXY_HEADERS` | 信任代理头 | 只在受控代理链后启用 |
| `DBGUARD_METRICS_TOKEN` | `/metrics` Bearer token | 独立 secret |
| `DBGUARD_OPERATIONS_WEBHOOK_TOKEN` | 运维结果接收 Token | 独立长随机 secret |
| `DBGUARD_OPERATIONS_ORGANIZATION_ID` | 运维事件所属组织 | 显式设置 |
| `DBGUARD_PASSPORT_HMAC_SECRET` | 通行证 HMAC 密钥 | 至少 32 字节随机值 |
| `DBGUARD_PASSPORT_TTL` | 通行证有效期 | 1～30 分钟，默认 10 分钟 |
| `DBGUARD_EXPERIMENT_MODE` | SQL 验证模式 | 需要生产签发时使用 `postgres` |
| `DBGUARD_SHADOW_DSN` | 影子 PostgreSQL DSN | 独立实例和最小权限，从 secret 注入 |
| `DBGUARD_WORKERS` | SQL Outbox worker 数 | 按影子库容量设置；生产不能为 0 |
| `DBGUARD_ENABLE_UPGRADE_APPLY` | 允许 Web API 触发升级 apply | 默认且通常保持 `false` |

DSN、OIDC client secret、Redis 凭据、metrics/operations token 和通行证不能进入镜像、Git、日志或工单正文。

## Production profile 与启动检查

生产核心会重新读取 `DBGUARD_ENV_FILE`，不只依赖 systemd 展开的环境。重复键、非法行、缺失文件和 inherited override 冲突都会在组件初始化前失败。

`DBGUARD_ENV_PROFILE=production` 还会拒绝 wildcard 监听、含义冲突的 `PORT`、演示账号和数据、`demo_only` 实验、memory session、无 ACL Redis、远程明文 Redis、共享 namespace、非 HTTPS 公网地址、非 Secure Cookie、占位符 secret、缺少关键凭据和零 worker。

检查配置不会连接业务依赖，也不会输出 secret：

```bash
DBGUARD_ENV_FILE=/etc/changeguard/core.env \
DBGUARD_ENV_PROFILE=production \
/opt/changeguard/current/dbguard --check-config
```

规范模板位于 `deploy/production/changeguard-core.env.example`。其中的 `REPLACE_ME` 必须替换。

## systemd 与文件权限

先运行：

```bash
deploy/production/changeguard-core-host-prepare.sh apply
deploy/production/changeguard-core-host-prepare.sh check
```

脚本创建无登录 `changeguard` 身份和支持目录，不迁移生产数据所有权、不切换 release，也不重启服务。

`deploy/production/changeguard.service` 使用专用用户、`UMask=0077`、空 capability 集和 `ProtectSystem=strict`。启动前依次检查：

- 服务不以 root 运行；
- 环境文件和 release 对运行用户不可写；
- release 中 `SHA256SUMS` 完整通过；
- 数据目录可写；
- file 模式 witness 与 required marker 成对存在；
- 候选二进制的 `--check-config` 通过。

不要通过以 root 运行核心来绕过权限错误。

## SQL Outbox 运维

一次影子验证由稳定 `attempt_id` 标识，每次 claim 递增 `lease_generation`。租约过期后新 worker 从新的隔离事务完整重跑 APPLY；当前实现不能从单条 SQL 继续。FINALIZE 必须匹配 worker、generation、输入摘要和结果摘要。

排查事件时核对：

- `attempt_id` 和 `lease_generation`；
- 当前 stage 及阶段时间；
- `input_sha256` 和 `result_digest`；
- `LockedBy` 和 `LockedUntil`。

不要人工降低 generation、清空摘要或把 APPLY 直接改成 FINALIZE。只有 `POSTGRES` 模式下迁移和回滚完成影子执行的报告可以推进审批。`CREATE INDEX CONCURRENTLY` 和 `DROP INDEX CONCURRENTLY` 会去掉 `CONCURRENTLY`，以事务内等价形式验证；生产语句保持原样，因此仍需在目标环境评估并发索引行为。

## 健康检查和指标

```bash
curl --fail http://127.0.0.1:8080/health/live
curl --fail http://127.0.0.1:8080/health/ready
curl --fail \
  -H "Authorization: Bearer $DBGUARD_METRICS_TOKEN" \
  http://127.0.0.1:8080/metrics
```

- liveness 只表示进程存活；
- readiness 反映提供请求所需的关键依赖；
- metrics 只允许监控网络访问；
- 日志系统应对 Authorization、Cookie、password、secret 和 token 字段脱敏；
- 指标不能以随机或固定数据替代外部运行事实。

Prometheus 告警模板位于 `deploy/production/dbguard-prometheus-alerts.yaml`，使用前需要按样本量和告警分级校准阈值。

## HTTPS 和反向代理

浏览器和 Gate API 必须通过 HTTPS 暴露。代理应：

- 保留 Host 和请求 ID；
- 限制请求体、连接数和超时；
- 只在受控链路中设置并信任 `X-Forwarded-*`；
- 对 `/metrics` 使用独立网络策略；
- 限制 `/api/gate/verify`、`/api/gate/consume` 和运维 webhook 的来源网络，但不以 IP 代替凭据校验；
- 不记录 Authorization、Cookie 或完整请求体。

## PostgreSQL

- 使用专用数据库和最小权限账号；
- 迁移幂等创建并回填 `changeguard_changes`、`changeguard_outbox`、`changeguard_passports`、`changeguard_audit_events` 和 `changeguard_idempotency_records`；
- 保留 `dbguard_state` legacy JSONB。组织、成员、应用、规则及部分集成数据仍依赖它，不能删除；
- Outbox claim、幂等 claim、通行证消费和审计追加使用专用原子事务；
- 连接池按“实例数 × 每实例最大连接数”核算；
- 使用 `DBGUARD_TEST_POSTGRES_DSN` 运行多实例集成测试。未设置时测试会跳过，不能视为目标环境已验证；
- 定期备份并实际执行恢复演练。

## Redis

- 只保存会话、限流等有 TTL 或可重建的数据；
- 使用专用 ACL 用户、长随机密码、私网边界和明确持久化策略；
- 非 loopback 连接使用 TLS；
- 每个环境使用独立 `DBGUARD_REDIS_PREFIX`，不要共享默认 namespace；
- Redis 故障时 readiness、会话、登录和注册控制返回 503，不能降级为 memory 或匿名访问；
- 核心重启后现有会话应仍然有效。

## 通行证运维

上线前至少验证：

- 明文 Token 只在签发响应出现一次；
- 数据库只保存摘要和安全元数据；
- 过期、吊销、摘要变化、规则变化和重放都能阻断；
- 并发消费只有一个成功；
- Gate 超时、5xx 和网络错误时流水线停止；
- 消费、过期落盘和吊销进入审计；失败访问日志不包含 Token。

CI secret 应设为 masked/protected，`consume` 紧邻生产部署命令。详见 [CI/CD 接入](ci-integration.md)。

## 备份与恢复

至少备份组织、成员、应用、规则、变更、检查结果、通行证元数据、审计事件以及已接收的流水线和运维结果。

file 模式必须把主状态文件、migration witness 和 `.required` marker 作为一个恢复单元。`deploy/production/changeguard-backup.sh` 会生成带摘要的快照。恢复不能直接覆盖生产目录，先验证再 stage：

```bash
CHANGEGUARD_BACKUP_DIR=/opt/changeguard/backups \
  ./changeguard-restore.sh verify /opt/changeguard/backups/snapshot-YYYYMMDD-HHMMSS

CHANGEGUARD_BACKUP_DIR=/opt/changeguard/backups \
CHANGEGUARD_RESTORE_ROOT=/opt/changeguard/restore-staging \
  ./changeguard-restore.sh stage \
  /opt/changeguard/backups/snapshot-YYYYMMDD-HHMMSS \
  /opt/changeguard/restore-staging/restore-YYYYMMDD-HHMMSS
```

`verify` 检查 manifest、路径边界、文件权限、JSON、审计链和 witness 配对；`stage` 复制到隔离目录并再次验证。生产激活仍需专用账号权限、规范环境、preflight、`--check-config`、隔离启动和显式流量切换。

恢复验收应确认组织隔离、通行证终态、审计一致性和旧 Token 失败关闭。OIDC、Redis、metrics 等 secret 由部署平台重新注入。

## 候选安装、升级与回滚

不要直接把归档解压到 release root。使用安装器并提供发布流程记录的 transport SHA-256：

```bash
./changeguard-core-install.sh \
  /opt/changeguard/uploads/changeguard-core-candidate.tar.gz \
  EXPECTED_TRANSPORT_SHA256 \
  /opt/changeguard/releases \
  2026.08.08-candidate.N-shortcommit
```

安装器拒绝摘要不符、路径穿越、链接、设备文件和异常展开规模，统一权限后验证 `SHA256SUMS`。它只创建不可变 release，不修改 `/opt/changeguard/current`，也不重启服务。

`POST /api/upgrade/apply` 可触发 root watcher，默认由 `DBGUARD_ENABLE_UPGRADE_APPLY=false` 关闭。只在升级包来源、备份恢复、回滚、watcher 权限和共享目录边界均已验证的维护窗口临时启用，窗口结束后关闭并重启核心。

升级前执行：

1. 备份数据库或 file 状态，并完成 verify、stage 和隔离启动；
2. 运行测试、静态检查、镜像构建和浏览器流程；
3. 验证 Gate 的允许与阻断场景；
4. 对生产数据副本执行“新候选 → 当前版本 → 新候选”往返，确认状态收敛；
5. 验证真实 Redis 的会话持久性、限流、故障 503 和启动失败关闭；
6. 运行 host prepare、安装器、preflight 和 `--check-config`；
7. 先以 0% upstream 验证候选，再逐步切换流量。

应用版本回滚不等于数据库迁移回滚。PostgreSQL 迁移需要保持向后兼容或使用单独验证过的数据库恢复方案。

## 上线检查

- 专用 `changeguard` 用户运行核心，release 和 env 只读，只有数据目录可写；
- production profile、preflight 和 `--check-config` 通过；
- 演示账号和演示数据关闭；
- OIDC、HTTPS、Secure Cookie 和代理信任边界验证通过；
- PostgreSQL、Redis 和影子库使用独立最小权限凭据；
- Redis namespace、TLS、会话重启和故障行为验证通过；
- metrics、Gate 和 webhook 具备网络限制与独立凭据；
- 通行证并发消费、过期、吊销和重放测试通过；
- 备份恢复和版本往返演练通过；
- CI 对非 2xx、超时和网络错误保持失败关闭；
- 日志、报告和审计不包含 secret；
- 当前版本的测试与部署验证结果已保存。
