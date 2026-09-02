# ChangeGuard

ChangeGuard 是生产变更门禁。它把 SQL、应用配置和 Kubernetes 清单纳入同一套检查、审批和 CI 校验流程，并用绑定制品 SHA-256 的一次性通行证防止审批后替换文件。

ChangeGuard 不执行生产 SQL，也不接管 Git、CI/CD 或 Kubernetes。它只负责在现有部署步骤之前核对制品、审批和通行证。

```text
Developer / Git repository
          |
          v
    Change submission
          |
          v
  Normalize + redact + hash
          |
     +----+----+
     |         |
 Static rules  PostgreSQL shadow validation
     |         |  (SQL only)
     +----+----+
          |
       Review
          |
       Approval
          |
  One-time passport
          |
    CI verify / consume
          |
       Deployment
```

三个关键约束：

1. 通行证绑定制品 SHA-256、目标环境和规则版本。CI 对工作区中的真实文件重新计算摘要，不一致就拒绝。
2. 通行证明文只返回一次，服务端只保存摘要。消费使用原子状态更新，并发请求只能有一个成功。
3. SQL 只有在隔离的 PostgreSQL 影子库中完成迁移和回滚验证后，才具备运行验证证据。并发索引语句会去掉 `CONCURRENTLY`，以影子等价形式在事务中执行；生产 SQL 保持原样。`NOT_RUN` 和 `DEMO_ONLY` 不能用于生产签发。

详细边界见[威胁模型](docs/threat-model.md)。

## 适用场景

ChangeGuard 适合已经使用 Git 和 CI/CD、需要把人工审批绑定到最终部署制品，并愿意自托管 PostgreSQL、Redis 与隔离影子库的团队。它可以把 SQL、配置和 Kubernetes 变更放进同一套准入流程。

如果你需要工具直接执行生产 SQL、操作 Kubernetes、替代完整 ITSM/发布编排平台，或只想要一个无状态命令行扫描器，ChangeGuard 并不合适。

## 界面

![变更单列表，高风险配置和 Kubernetes 被阻断](docs/assets/01-change-list.webp)

![变更详情：制品 SHA-256、规则发现、通行证尚未签发](docs/assets/02-change-detail.webp)

![准入工作台与审计事件](docs/assets/03-dashboard.webp)

![风险全景](docs/assets/04-risk-panorama.webp)

## 支持的制品

| 类型 | 输入 | 典型阻断项 | 签发条件 |
| --- | --- | --- | --- |
| SQL | 迁移 SQL、回滚 SQL、回滚方案 | 无条件 UPDATE/DELETE、危险 DDL、缺少回滚、非并发索引 | 静态规则通过，并在隔离 PostgreSQL 中完成迁移与回滚验证；并发索引使用去掉 `CONCURRENTLY` 的影子等价形式 |
| 配置 | YAML、JSON、ENV | 明文凭据、生产 debug、关闭鉴权或 TLS 校验 | 当前规则没有未处理的阻断项 |
| Kubernetes | Deployment、StatefulSet、Pod 等 | `latest`、privileged、提权、root、hostPath、缺少资源限制或探针 | 当前规则没有未处理的阻断项 |

`COMPLETED` 只表示通行证已经消费，不表示部署成功或线上服务健康。

## 最小本地演示

要求 Git 和 Go **1.25**。这个方式使用文件存储和内存会话，不需要 PostgreSQL 或 Redis；未配置影子库时，SQL 只能做静态检查。

```powershell
git clone https://github.com/kyfd/changeguard.git
Set-Location changeguard
$env:CHANGEGUARD_AUTH_MODE = "local"
$env:CHANGEGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:CHANGEGUARD_ENABLE_DEMO_DATA = "true"
$env:CHANGEGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
go run ./cmd/dbguard
```

打开 <http://localhost:8080>，或请求 `http://localhost:8080/health/ready` 检查服务状态。按 `Ctrl+C` 停止。上面的 HMAC secret 仅用于本机演示。

演示账号只在 `CHANGEGUARD_ENABLE_DEMO_ACCOUNTS=true` 时创建，密码均为 `Demo1234`：

| 邮箱 | 角色 |
| --- | --- |
| `developer@example.com` | 创建、提交、整改 |
| `reviewer@example.com` | 复核、审批、签发通行证 |
| `owner@example.com` | 管理应用、成员和规则，高风险审批 |

生产环境必须关闭演示账号和演示数据。关闭开关不会删除已经写入持久化存储的演示凭据；复用过演示数据的环境还需要显式删除这些账号，生产环境更适合使用全新的存储。

## Compose 演示

Compose 会启动 PostgreSQL、Redis 和 PostgreSQL 影子库，适合验证外部存储与 SQL 影子执行。`docker-compose.yml` 仅用于本地演示，不是生产部署清单。

```powershell
Copy-Item .env.example .env
# 修改 .env 中的 HMAC secret 和数据库密码
docker compose up --build
```

服务就绪后访问 <http://localhost:8080>。停止并清理本地容器时运行 `docker compose down`。

带预置演示数据的端到端环境：

```powershell
docker compose -f compose.e2e.yml up --build
```

影子库必须与生产隔离。影子 DSN 与主库 DSN 使用相同 host:port 时，服务会拒绝启动或执行验证。

## CI 接入

构建 Gate CLI 并检查清单摘要：

```powershell
go build -o changeguard-gate.exe ./cmd/changeguard-gate
.\changeguard-gate.exe digest -manifest .\examples\ci-demo\.changeguard.json
```

把 `CHANGEGUARD_URL` 和签发时得到的 `CHANGEGUARD_TOKEN` 配置为 CI masked/protected secret，然后在部署前执行：

```text
changeguard-gate verify  -manifest .changeguard.json -consumer <pipeline-id>
changeguard-gate consume -manifest .changeguard.json -consumer <pipeline-id>
```

`consume` 必须紧邻生产部署步骤，任何非零退出码都应停止流水线。不要把 Token 写入仓库、请求体或构建日志。参见 [CI/CD 接入指南](docs/ci-integration.md)和 [CI 示例](examples/ci-demo/README.md)。

## 测试

```powershell
go test ./...
go vet ./...
npm test
node --check internal/httpapi/web/api-adapter.js
node --check internal/httpapi/web/app.js
```

CI 还运行 race detector、PostgreSQL/Redis 集成测试、镜像构建和 Playwright 浏览器测试。

仓库包含可重复运行的并发消费测试和规则扫描 benchmark。测量方法、环境与未测项见 [benchmarks.md](docs/benchmarks.md)；这些结果不是生产 SLA。

## 目录

```text
cmd/dbguard/                 核心 HTTP 服务入口
cmd/changeguard-gate/        CI 摘要、验签和消费客户端
cmd/changeguard-evidence/    证据包导出和离线校验
internal/                    规则、服务、存储、认证、HTTP 和内置页面
deploy/                      Compose、Kubernetes 和生产运维资产
docs/                        产品、架构、安全、接入与运维文档
examples/                    示例制品和 CI 配置
```

Go module 是 `github.com/kyfd/changeguard`。为兼容已有脚本，服务入口和发布产物仍使用 `dbguard` 名称。环境变量优先使用 `CHANGEGUARD_*`；旧的 `DBGUARD_*` 仍可用，但会产生弃用提示，计划在 v4.0 删除。

## 文档

从[文档索引](docs/README.md)开始，或直接查看：

- [产品范围](docs/product.md)
- [业务流程](docs/business-flow.md)
- [系统架构](docs/architecture.md)
- [威胁模型](docs/threat-model.md)
- [HTTP API](docs/api.md)
- [CI/CD 接入](docs/ci-integration.md)
- [部署与运维](docs/enterprise-operations.md)
- [Changelog](CHANGELOG.md)

## License

[MIT](LICENSE)
