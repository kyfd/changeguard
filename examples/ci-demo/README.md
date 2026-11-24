# CI Gate 示例

本目录包含一次订单服务变更所需的 SQL、回滚 SQL、Kubernetes Deployment、`.changeguard.json`、GitLab CI 配置和 Jenkinsfile。它用于验证变更单与 CI 工作区文件的摘要绑定，不会连接或修改生产环境。

## 启动 ChangeGuard

在仓库根目录执行 Compose 演示：

```powershell
Copy-Item .env.example .env
# 将 CHANGEGUARD_PASSPORT_HMAC_SECRET 替换为至少 32 字节随机值，并修改 CHANGEGUARD_POSTGRES_PASSWORD
docker compose --env-file .env up --build
```

登录账号：

- 开发者：`developer@example.com` / `Demo1234`
- 审核人：`reviewer@example.com` / `Demo1234`

这些账号只用于本地演示。

## 构建 CLI 并计算摘要

在仓库根目录执行：

```powershell
go build -o changeguard-gate.exe .\cmd\changeguard-gate
.\changeguard-gate.exe digest -manifest .\examples\ci-demo\.changeguard.json
```

输出的 `artifact_sha256` 包含迁移 SQL、回滚 SQL、Kubernetes 文件、回滚方案和清单元数据。文件字节、顺序或元数据变化都会改变摘要。

## 验证和消费通行证

在页面中创建与 `.changeguard.json` 内容一致的变更，完成静态检查、SQL 影子验证和独立审批，然后由审批人签发一次性通行证。

```powershell
$env:CHANGEGUARD_URL = "http://localhost:8080"
$env:CHANGEGUARD_TOKEN = "cg1..."
$env:CI_JOB_ID = "demo-pipeline-20260804"
.\changeguard-gate.exe verify -manifest .\examples\ci-demo\.changeguard.json -consumer $env:CI_JOB_ID
.\changeguard-gate.exe consume -manifest .\examples\ci-demo\.changeguard.json -consumer $env:CI_JOB_ID
```

`consume` 成功后 Token 立即失效，再次消费会被拒绝。若修改 `examples/ci-demo/deploy/orders.yaml` 后运行 `verify`，Gate 应返回摘要不一致且不消费 Token。

`COMPLETED` 只表示通行证已消费；本示例没有执行生产部署，也不证明服务健康。

## 接入 CI

- GitLab：参考 `.gitlab-ci.changeguard.yml`，把 `CHANGEGUARD_URL`、`CHANGEGUARD_TOKEN` 和 `CHANGEGUARD_CHANGE_ID` 配置为 protected/masked 变量。

## 联合发布示例

在该仓库根目录 `.gitlab-ci.yml` 中 `include: [{ local: 'examples/ci-demo/.gitlab-ci.joint-release.yml' }]`。示例会编译当前仓库的 Gate CLI，并在实际执行迁移和应用部署的两个作业里分别消费通行证。开始前：

1. 按[集成文档](../../docs/ci-integration.md#50-联合发布串联数据库与应用部署)配置 HTTPS、项目白名单和 Deployment events。用经过保护的默认分支与 runner。
2. 把 `deploy/orders.yaml` 的示例镜像替换为真实可拉取的镜像。从同一 GitLab 项目的实际提交，按下表用 `POST /api/changes` 登记两条变更。请使用 API 保留文件原始字节和结尾换行；控制台文本表单会修剪制品文本。分别核对 `/api/changes/{id}` 的 `artifact_sha256` 与 `changeguard-gate digest -manifest <文件>` 的输出。控制台创建 `production` 计划，顺序为 `migrate_orders`、`deploy_orders`，分别审批并签发通行证。
3. 配置 `CHANGEGUARD_URL`、`MIGRATION_PASSPORT`、`APPLICATION_PASSPORT` 为受保护变量，两个通行证设为 masked。为迁移提供 `PGHOST`、`PGDATABASE`、`PGUSER`、受保护的数据库凭据及 TLS 根证书；为部署提供受保护的 `KUBECONFIG` 文件变量（也可由 GitLab Kubernetes agent 注入）与明确的 `KUBE_CONTEXT`。不要开启 CI 调试跟踪。
4. 计划创建后才从该固定提交启动部署作业。应用作业等待匹配的迁移部署证据；若迁移失败、凭据无效或等待超时，不会运行应用部署。两步都会修改各自配置的目标，首次接入请用隔离环境和与之匹配的 manifest/环境名验证。

两次请求均传 `title`、`application_id`、`repository_url`、完整 `commit_sha` 和与 manifest 相同的 `environment`、`change_type`、`rollback_plan`。另外传入计划执行时间和满足本组织策略的 `release_plan`；迁移按现有流程完成 PostgreSQL 影子验证。

| 作业 | Manifest | 额外的变更材料字段 |
| --- | --- | --- |
| `migrate_orders` | `.changeguard.migration.json` | 将 `sql_path` 和 `rollback_sql_path` 的原始内容分别放入 `sql`、`rollback_sql`；不用再添加 `DATABASE` 制品 |
| `deploy_orders` | `.changeguard.application.json` | `artifacts[0]` 使用 manifest 中的 `kind`、`name`、`source`、`language`；将 `path` 指向文件的原始内容放入 `content`，不发送 `path` |
- Jenkins：参考 `Jenkinsfile`，在 Credentials 中保存 URL、通行证和 webhook token，并限制到对应 Folder 或 Job。
- Gate CLI 非零退出时必须停止流水线。`consume` 应紧邻实际部署步骤。

完整参数、安全要求和 webhook 说明见 [CI/CD 接入指南](../../docs/ci-integration.md)。
