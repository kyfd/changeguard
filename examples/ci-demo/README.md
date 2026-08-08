# ChangeGuard 真实流水线演示

这个目录模拟一个订单服务的生产变更：同一次发布同时包含 SQL 索引、Kubernetes Deployment 和回滚方案。它用于演示“审批对象”和“流水线实际文件”之间的摘要绑定，不需要连接真实生产环境。

## 1. 本地启动

在仓库根目录执行：

```powershell
Copy-Item .env.example .env
# 将 .env 中的 DBGUARD_PASSPORT_HMAC_SECRET 替换为至少 32 字节随机值
docker compose --env-file .env up --build
```

使用 `developer@example.com` / `Demo1234` 登录。审核和签发通行证使用 `reviewer@example.com` / `Demo1234`。

## 2. 计算实际制品摘要

从仓库根目录构建 Gate CLI，并在本目录执行：

```powershell
go build -o changeguard-gate.exe .\cmd\changeguard-gate
.\changeguard-gate.exe digest -manifest .\examples\ci-demo\.changeguard.json
```

输出的 `artifact_sha256` 来自 SQL、回滚 SQL、Kubernetes 文件和清单元数据的原始字节。仅修改一个空格，摘要也会变化。

## 3. 完成一次 Gate

在 ChangeGuard 页面创建或选择对应变更，完成检查、整改、独立复核和审批后复制一次性通行证：

```powershell
$env:CHANGEGUARD_URL = "http://localhost:8080"
$env:CHANGEGUARD_TOKEN = "cg1..."
$env:CI_JOB_ID = "demo-pipeline-20260804"
.\changeguard-gate.exe verify -manifest .\examples\ci-demo\.changeguard.json
.\changeguard-gate.exe consume -manifest .\examples\ci-demo\.changeguard.json
```

`consume` 成功后，通行证立即失效；重复执行会被拒绝。修改 `deploy/orders.yaml` 的镜像标签后再执行 `verify`，会因为摘要不一致而阻断。

## 4. 接入真实 CI

- GitLab：将 `.gitlab-ci.changeguard.yml` 合并到项目流水线，配置 `CHANGEGUARD_URL`、`CHANGEGUARD_TOKEN` 和 `CHANGEGUARD_CHANGE_ID` 为 protected/masked 变量。
- Jenkins：将 `Jenkinsfile` 复制到流水线项目，安装 HTTP Request Plugin，并在 Credentials 中创建 `changeguard-url` 与 `changeguard-webhook-token`。
- 真实流水线状态会进入“流水线”页面和审计链；流水线成功不会绕过人工审批。

## 5. 面试演示顺序

1. 展示同一变更单里的 SQL、Kubernetes 文件和回滚方案。
2. 修改镜像标签，说明 Gate 从真实文件重算摘要并阻断制品替换。
3. 恢复文件并完成审批，执行 `verify` 后 `consume`。
4. 重复消费一次，展示一次性凭证和审计事件。
5. 打开冲突雷达，说明相同资源、上下游依赖和发布窗口重叠如何形成可解释冲突。
