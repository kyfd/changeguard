# ChangeGuard

生产变更门禁。把 SQL、配置和 Kubernetes 清单放进同一张变更单，跑规则检查和审批，再给 CI 发一次性通行证。通行证绑的是文件 SHA-256，审批后再改文件就会被拦住。

它不接生产库执行 SQL，也不接管 Git、Jenkins 或 Kubernetes。现有流水线前面加一层即可。

```text
Developer / Git Repository
          |
          v
    Change Submission
          |
          v
  Normalize + Redact + Hash
          |
     +----+----+
     |         |
 Static Rules  PostgreSQL Shadow Validation
     |         |
     +----+----+
          |
       Review
          |
       Approval
          |
 One-time Passport
          |
    CI Verify / Consume
          |
       Deployment
```

继续往下看之前，先看这三个问题：

1. **如何防止审批后文件被修改？** 通行证绑定制品 SHA-256、环境和规则版本。CI 对着真实文件重算摘要，对不上就拒绝部署。
2. **如何防止通行证重放或重复使用？** Token 只出现一次，只存 SHA-256。消费是一次条件更新：100 个并发消费者里只能有一个成功。
3. **如何证明 SQL 和回滚实际被验证过？** 只有隔离的 PostgreSQL 影子库真正跑过，才能把证据写成已验证。演示模式明确标成 `NOT_RUN` / `DEMO_ONLY`，不能签发生产通行证。

完整论证见 [威胁模型](docs/threat-model.md)。五分钟走查见 [演示剧本](docs/demo-playbook.md)。

## 界面

![变更单列表，高风险配置和 Kubernetes 被阻断](docs/assets/01-change-list.webp)

![变更详情：制品 SHA-256、规则发现、通行证尚未签发](docs/assets/02-change-detail.webp)

![准入工作台与审计事件](docs/assets/03-dashboard.webp)

![风险全景](docs/assets/04-risk-panorama.webp)

## 做什么

| 类型 | 输入 | 会拦什么 | 什么情况下能签发通行证 |
| --- | --- | --- | --- |
| SQL | 执行 SQL、回滚 SQL、回滚方案 | 无条件 UPDATE/DELETE、危险 DDL、缺回滚、非并发索引 | 规则通过，并且在隔离的 PostgreSQL 影子库里真正跑过 SQL 和回滚 |
| 配置 | YAML / JSON / ENV | 明文凭据、生产 debug、关掉鉴权或 TLS 校验 | 当前规则没有未处理的阻断项 |
| Kubernetes | Deployment / StatefulSet / Pod 等 | `latest`、privileged、提权、root、hostPath、缺资源或探针 | 当前规则没有未处理的阻断项 |

没跑过的验证会标成 `NOT_RUN` / `DEMO_ONLY`，不能当生产证据。`COMPLETED` 只表示这张通行证已经用掉，不代表上线后一定正常。

## 本地运行

最低 Go **1.25**。CI 在 1.25 和 1.26 上跑测试。默认用文件存储和内存会话，不用装 PostgreSQL / Redis。

```powershell
$env:CHANGEGUARD_AUTH_MODE = "local"
$env:CHANGEGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:CHANGEGUARD_ENABLE_DEMO_DATA = "true"
$env:CHANGEGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
go run ./cmd/dbguard
```

打开 <http://localhost:8080>。HMAC secret 只给本地演示用，生产请换成随机值。旧的 `DBGUARD_*` 变量仍然有效，启动时会打印弃用警告，计划在 v4.0 删除。

没配影子库时，配置和 Kubernetes 仍可走完检查、审批和通行证；SQL 只能做静态检查。要开影子验证：

```powershell
$env:CHANGEGUARD_EXPERIMENT_MODE = "postgres"
$env:CHANGEGUARD_SHADOW_DSN = "postgres://runner:password@127.0.0.1:5432/changeguard_shadow?sslmode=disable"
```

影子库必须和生产隔离。如果影子 DSN 和主库 DSN 指向同一个 host:port，进程会直接拒绝连接。

### 演示账号

`CHANGEGUARD_ENABLE_DEMO_ACCOUNTS=true` 时才会创建，密码都是 `Demo1234`：

| 邮箱 | 角色 |
| --- | --- |
| `developer@example.com` | 创建、提交、整改 |
| `reviewer@example.com` | 复核、审批、签发通行证 |
| `owner@example.com` | 管应用、成员、规则，以及高风险审批 |

生产环境关掉 `CHANGEGUARD_ENABLE_DEMO_ACCOUNTS` 和 `CHANGEGUARD_ENABLE_DEMO_DATA`。

### Docker Compose

```powershell
Copy-Item .env.example .env
# 改掉 .env 里的 HMAC secret 和数据库密码
docker compose up --build
```

Compose 默认会起 PostgreSQL 影子库。带演示数据的端到端栈：

```powershell
docker compose -f compose.e2e.yml up --build
```

## CI 接入

```powershell
go build -o changeguard-gate.exe ./cmd/changeguard-gate
.\changeguard-gate.exe digest -manifest .changeguard.json
```

流水线把 `CHANGEGUARD_URL` 和 `CHANGEGUARD_TOKEN` 放进 masked secret，然后：

```text
changeguard-gate verify  -manifest .changeguard.json -consumer <pipeline-id>
changeguard-gate consume -manifest .changeguard.json -consumer <pipeline-id>
```

不要把 Token 写进仓库、请求体或构建日志。示例见 [CI/CD 接入](docs/ci-integration.md)，仓库里还有 [examples/ci-demo](examples/ci-demo)。

## 实测

数字来自 `go test` / `go test -bench`，方法就在仓库里。换机器请重跑，不要抄走当 SLA。

| 场景 | 数据规模 | 本机结果 | 怎么测 |
| --- | --- | --- | --- |
| 并发消费同一张通行证 | 100 goroutine | 1 成功 / 99 拒绝，变更单原子标为 COMPLETED | `go test ./internal/store -run TestUsePassportConcurrentConsumeCompletesChangeOnce -count=1` |
| Kubernetes 规则扫描 | 100 / 1,000 / 10,000 个 Deployment | 2.6 ms / 28.3 ms / 269 ms | `go test ./internal/checker -bench BenchmarkKubernetesRuleScan -benchmem` |

测量环境、分配次数和未测项见 [docs/benchmarks.md](docs/benchmarks.md)。

## 命名

仓库、UI 和文档叫 ChangeGuard。Go module 是 `github.com/kyfd/changeguard`。服务入口暂时仍是 `./cmd/dbguard`，发布产物仍叫 `dbguard`，避免打断已有安装脚本。

环境变量优先读 `CHANGEGUARD_*`。未设置时回退 `DBGUARD_*` 并告警。`DBGUARD_*` will be removed in v4.0.

## 测试

```powershell
go test ./...
go vet ./...
npm test
node --check internal/httpapi/web/api-adapter.js
node --check internal/httpapi/web/app.js
```

CI 还会跑 race detector、PostgreSQL / Redis 集成契约、镜像构建，以及 Playwright 黄金路径（见 `compose.e2e.yml`）。

## 目录

```text
cmd/dbguard/                服务入口
cmd/changeguard-gate/       CI 摘要、验签、消费
cmd/changeguard-evidence/   证据包导出和离线校验
cmd/changeguard-agent-eval/ 解释层离线评测
internal/                   规则、审批、存储、HTTP 和内置页面
deploy/                     Docker / Nginx / Kubernetes / 生产安装
docs/                       产品、架构、威胁模型、API、部署
examples/                   示例 SQL 和 CI 演示
tests/                      Playwright 黄金路径
```

## 文档

- [Changelog](CHANGELOG.md)
- [威胁模型](docs/threat-model.md)
- [演示剧本](docs/demo-playbook.md)
- [产品说明](docs/product.md)
- [业务流程](docs/business-flow.md)
- [系统架构](docs/architecture.md)
- [HTTP API](docs/api.md)
- [CI/CD 接入](docs/ci-integration.md)
- [部署与运维](docs/enterprise-operations.md)
- [v3.0 发布验收](docs/v3-release-acceptance.md)

## License

[MIT](LICENSE)
