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
- Jenkins：参考 `Jenkinsfile`，在 Credentials 中保存 URL、通行证和 webhook token，并限制到对应 Folder 或 Job。
- Gate CLI 非零退出时必须停止流水线。`consume` 应紧邻实际部署步骤。

完整参数、安全要求和 webhook 说明见 [CI/CD 接入指南](../../docs/ci-integration.md)。
