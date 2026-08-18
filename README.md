# ChangeGuard

生产变更门禁。把 SQL、配置和 Kubernetes 清单放进同一张变更单，跑规则检查和审批，再给 CI 发一次性通行证。通行证绑的是文件 SHA-256，审批后再改文件就会被拦住。

它不接生产库执行 SQL，也不接管 Git、Jenkins 或 Kubernetes。现有流水线前面加一层即可。

## 做什么

| 类型 | 输入 | 会拦什么 | 什么情况下能签发通行证 |
| --- | --- | --- | --- |
| SQL | 执行 SQL、回滚 SQL、回滚方案 | 无条件 UPDATE/DELETE、危险 DDL、缺回滚、非并发索引 | 规则通过，并且在隔离的 PostgreSQL 影子库里真正跑过 SQL 和回滚 |
| 配置 | YAML / JSON / ENV | 明文凭据、生产 debug、关掉鉴权或 TLS 校验 | 当前规则没有未处理的阻断项 |
| Kubernetes | Deployment / StatefulSet / Pod 等 | `latest`、privileged、提权、root、hostPath、缺资源或探针 | 当前规则没有未处理的阻断项 |

大致流程：

1. 开发提交变更和制品
2. 算摘要、脱敏、跑规则；SQL 还要进影子库
3. 阻断项要整改，并由别人复核
4. 审批通过后签发一次性通行证，明文 Token 只出现一次
5. CI 用 `.changeguard.json` 和 `changeguard-gate` 对着真实文件重算摘要，再 `verify` / `consume`
6. 摘要、环境、规则版本或有效期对不上就拒绝部署

没跑过的验证会标成 `NOT_RUN` / `DEMO_ONLY`，不能当生产证据。`COMPLETED` 只表示这张通行证已经用掉，不代表上线后一定正常。

## 本地运行

需要 Go 1.23。默认用文件存储和内存会话，不用装 PostgreSQL / Redis。

```powershell
$env:DBGUARD_AUTH_MODE = "local"
$env:DBGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:DBGUARD_ENABLE_DEMO_DATA = "true"
$env:DBGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
go run ./cmd/dbguard
```

打开 <http://localhost:8080>。HMAC secret 只给本地演示用，生产请换成随机值。

没配影子库时，配置和 Kubernetes 仍可走完检查、审批和通行证；SQL 只能做静态检查。要开影子验证：

```powershell
$env:DBGUARD_EXPERIMENT_MODE = "postgres"
$env:DBGUARD_SHADOW_DSN = "postgres://runner:password@127.0.0.1:5432/changeguard_shadow?sslmode=disable"
```

影子库必须和生产隔离。

### 演示账号

`DBGUARD_ENABLE_DEMO_ACCOUNTS=true` 时才会创建，密码都是 `Demo1234`：

| 邮箱 | 角色 |
| --- | --- |
| `developer@example.com` | 创建、提交、整改 |
| `reviewer@example.com` | 复核、审批、签发通行证 |
| `owner@example.com` | 管应用、成员、规则，以及高风险审批 |

生产环境关掉这两个开关：`DBGUARD_ENABLE_DEMO_ACCOUNTS`、`DBGUARD_ENABLE_DEMO_DATA`。

### Docker Compose

```powershell
Copy-Item .env.example .env
# 改掉 .env 里的 DBGUARD_PASSPORT_HMAC_SECRET 和数据库密码
docker compose up --build
```

Compose 默认会起 PostgreSQL 影子库。

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

## 可选：解释层

可以接 OpenAI 兼容接口，用来解释阻断原因、给整改建议。规则结果、审批和通行证不受模型影响。没配模型时会走本地归纳。

离线评测不调付费接口：

```powershell
go run ./cmd/changeguard-agent-eval
```

## 测试

```powershell
go test ./...
go vet ./...
node --check internal/httpapi/web/api-adapter.js
node --check internal/httpapi/web/app.js
```

## 目录

```text
cmd/dbguard/                服务入口
cmd/changeguard-gate/       CI 摘要、验签、消费
cmd/changeguard-evidence/   证据包导出和离线校验
cmd/changeguard-agent-eval/ 解释层离线评测
internal/                   规则、审批、存储、HTTP 和内置页面
deploy/                     Docker / Nginx / Kubernetes / 生产安装
docs/                       产品、架构、API、部署
examples/                   示例 SQL 和 CI 演示
```

界面叫 ChangeGuard。Go module 和环境变量还是 `dbguard` / `DBGUARD_`，暂时没改。

## 文档

- [产品说明](docs/product.md)
- [业务流程](docs/business-flow.md)
- [系统架构](docs/architecture.md)
- [HTTP API](docs/api.md)
- [CI/CD 接入](docs/ci-integration.md)
- [部署与运维](docs/enterprise-operations.md)

## License

[MIT](LICENSE)
