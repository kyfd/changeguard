# ChangeGuard CI/CD 接入

CI 必须从实际发布工作区读取文件，按照与服务端相同的规则计算摘要，再使用一次性通行证调用 Gate。不要从页面复制摘要作为流水线输入。

`cmd/changeguard-gate` 提供三个命令：

```text
changeguard-gate digest
changeguard-gate verify
changeguard-gate consume
```

## Gate 语义

通行证绑定组织、变更、审批信息、目标环境、规则版本、聚合制品 SHA-256、有效期和单次消费状态。

- `digest` 只读取本地清单和文件并输出非敏感摘要；
- `verify` 对服务端状态执行只读检查，可在部署流水线较早阶段运行；
- `consume` 原子校验绑定、将通行证置为 `CONSUMED`，并将变更置为 `COMPLETED`。

`consume` 必须紧邻生产部署命令。`COMPLETED` 只表示 Gate 已使用，不表示后续部署成功。消费后部署失败时不能重放旧 Token，应记录失败并重新提交审批。

## CI 变量

服务端通行证配置：

```text
CHANGEGUARD_PASSPORT_HMAC_SECRET=<至少 32 字节随机值>
CHANGEGUARD_PASSPORT_TTL=10m
```

生产部署资产仍可使用对应的 `DBGUARD_*` 兼容键。

CI masked/protected secret 和标识：

```text
CHANGEGUARD_URL=https://changeguard.intra.example
CHANGEGUARD_TOKEN=<签发时只显示一次的 cg1...>
CI_JOB_ID=<流水线稳定标识>
```

优先从环境变量读取 Token。不要把 Token 写入仓库、构建产物、命令回显、请求体或普通日志；生产流水线也不建议使用 `-token` 参数，以免出现在进程参数中。

## `.changeguard.json`

清单位于发布工作区，并且只能引用清单目录内的相对路径。CLI 解析符号链接后的真实路径，拒绝目录逃逸、绝对路径和非普通文件。

### 配置或 Kubernetes 示例

`kind`、`name`、`source` 和 `language` 必须与变更单中的制品元数据完全一致；`path` 只用于 CI 读取文件。

```json
{
  "version": 1,
  "environment": "生产环境",
  "change_type": "Kubernetes 变更",
  "artifacts": [
    {
      "kind": "KUBERNETES",
      "name": "Kubernetes Manifest",
      "source": "人工提交",
      "language": "YAML",
      "path": "deploy/orders.yaml"
    }
  ],
  "rollback_plan": "kubectl rollout undo deployment/orders"
}
```

### SQL 示例

使用 `sql_path` 时，CLI 按服务端约定建立数据库制品，并将回滚 SQL 和回滚方案纳入聚合摘要。

```json
{
  "version": 1,
  "environment": "生产环境",
  "change_type": "数据库变更",
  "sql_path": "migrations/20260731_add_idempotency.sql",
  "rollback_sql_path": "migrations/20260731_add_idempotency.down.sql",
  "rollback_plan": "先停止写入，再执行 down.sql；异常时恢复上一版本应用"
}
```

### 摘要规则

- 文件按原始字节计算 SHA-256；换行、末尾空格和编码变化都会改变摘要；
- 制品顺序参与聚合摘要，清单顺序必须与变更单一致；
- `environment`、`change_type` 和制品元数据必须一致；
- `rollback_plan` 与 `rollback_plan_path` 二选一，其内容参与摘要；
- 单个文件上限 10 MiB，制品总计 25 MiB，清单上限 1 MiB；
- v1 只接受 `DATABASE`、`CONFIG` 和 `KUBERNETES`。

## 标准命令

计算摘要：

```bash
changeguard-gate digest -manifest .changeguard.json
```

输出包含 `artifact_sha256`、`environment` 和 `change_type`。摘要不是凭据，可以出现在构建日志中。

只读校验：

```bash
changeguard-gate verify \
  -manifest .changeguard.json \
  -consumer "$CI_JOB_ID"
```

生产部署前原子消费：

```bash
set -eu
changeguard-gate consume \
  -manifest .changeguard.json \
  -consumer "$CI_JOB_ID"
kubectl apply -f deploy/orders.yaml
```

流水线必须尊重 CLI 非零退出码。不要使用 `|| true` 或在 Gate 失败后继续生产修改。

## GitLab CI

将 `CHANGEGUARD_URL` 和 `CHANGEGUARD_TOKEN` 配置为 protected/masked 变量，不要在 `set -x` 模式下运行 Gate。

```yaml
variables:
  CHANGEGUARD_CHANGE_ID: "chg_replace_me"

changeguard_gate:
  stage: deploy
  image: your-registry/changeguard-gate:1
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
  script:
    - changeguard-gate digest -manifest .changeguard.json
    - changeguard-gate verify -manifest .changeguard.json -consumer "$CI_PIPELINE_ID"
    - changeguard-gate consume -manifest .changeguard.json -consumer "$CI_PIPELINE_ID"
    - kubectl apply -f deploy/orders.yaml
  environment:
    name: production
```

### GitLab 流水线事件

ChangeGuard 可以接收 GitLab Pipeline events，用于记录部署终态；Webhook 不会绕过审批或 Gate。

服务端配置：

```text
DBGUARD_GITLAB_SIGNING_TOKEN=<GitLab 19.0+ Signing Token，形如 whsec_...>
DBGUARD_GITLAB_WEBHOOK_SECRET=<迁移期 X-Gitlab-Token>
DBGUARD_GITLAB_ORGANIZATION_ID=org_demo
DBGUARD_GITLAB_WEBHOOK_MAX_AGE=5m
```

Webhook URL：

```text
https://changeguard.example.com/api/integrations/gitlab/webhook
```

启用 Pipeline events 并保持 SSL 校验。ChangeGuard 优先校验 `webhook-id`、`webhook-timestamp` 和 `webhook-signature`，并拒绝超出时间窗口的请求；没有签名头时才使用兼容的 `X-Gitlab-Token`。

事件优先通过 `CHANGEGUARD_CHANGE_ID` 关联变更，其次尝试本组织内的 `commit_sha`。无法关联的事件会保留并标记为未关联，不会猜测或修改变更状态。

## Jenkins

Gate 阶段示例：

```groovy
stage("ChangeGuard gate") {
  environment {
    CHANGEGUARD_URL = credentials("changeguard-url")
    CHANGEGUARD_TOKEN = credentials("changeguard-passport")
  }
  steps {
    sh '''
      set -eu
      changeguard-gate verify -manifest .changeguard.json -consumer "$BUILD_TAG"
      changeguard-gate consume -manifest .changeguard.json -consumer "$BUILD_TAG"
      kubectl apply -f deploy/orders.yaml
    '''
  }
}
```

Groovy 使用单引号字符串，让 shell 从环境变量读取凭据，避免 Groovy 提前插值。

### Jenkins 流水线事件

Jenkins 没有统一出站载荷，ChangeGuard 提供固定接收协议：

```text
DBGUARD_JENKINS_WEBHOOK_TOKEN=<至少 32 字节随机 Token>
DBGUARD_JENKINS_ORGANIZATION_ID=org_demo
```

```text
POST https://changeguard.example.com/api/integrations/jenkins/events
Authorization: Bearer <Token>
Content-Type: application/json
```

```json
{
  "change_id": "chg_123",
  "job_name": "orders-production",
  "build_number": 128,
  "build_url": "https://jenkins.example.com/job/orders/128/",
  "status": "SUCCESS",
  "commit_sha": "abcdef123456",
  "environment": "production",
  "occurred_at": "2026-08-08T10:00:00Z"
}
```

在 Jenkins Credentials 中保存 Secret text，只授权需要发布的 Folder 或 Job。使用 HTTP Request Plugin 时开启 `maskValue`，并通过 `JsonOutput` 生成请求体，避免手工拼接 JSON：

```groovy
post {
  always {
    script {
      withCredentials([string(
        credentialsId: 'changeguard-webhook-token',
        variable: 'CG_WEBHOOK_TOKEN'
      )]) {
        def payload = groovy.json.JsonOutput.toJson([
          change_id: env.CHANGEGUARD_CHANGE_ID,
          job_name: env.JOB_NAME,
          build_number: env.BUILD_NUMBER as Integer,
          build_url: env.BUILD_URL,
          status: currentBuild.currentResult,
          commit_sha: env.GIT_COMMIT,
          environment: 'production',
          occurred_at: new Date().format("yyyy-MM-dd'T'HH:mm:ssXXX", TimeZone.getTimeZone('UTC'))
        ])
        httpRequest(
          httpMode: 'POST',
          contentType: 'APPLICATION_JSON',
          customHeaders: [[
            name: 'Authorization',
            value: 'Bearer ' + env.CG_WEBHOOK_TOKEN,
            maskValue: true
          ]],
          requestBody: payload,
          url: env.CHANGEGUARD_JENKINS_EVENT_URL,
          validResponseCodes: '200:299'
        )
      }
    }
  }
}
```

## 发布后事件

流水线终态不能替代事故、实际回滚和业务 SLI。外部适配器可以使用独立凭据调用：

```text
DBGUARD_OPERATIONS_WEBHOOK_TOKEN=<至少 32 字节随机 Token>
DBGUARD_OPERATIONS_ORGANIZATION_ID=org_demo
POST /api/integrations/operations/webhook
```

每条事件携带稳定 `event_id` 和实际 `change_id`。事件只追加脱敏证据和审计，不修改审批或 Gate 状态。载荷和窗口约束见[发布后运维结果接入](operations-outcomes.md)。

## 错误处理

| 结果 | 流水线动作 |
| --- | --- |
| `ARTIFACT_MISMATCH` | 停止；检查实际文件、换行、元数据和回滚方案 |
| `ENVIRONMENT_MISMATCH` | 停止；不能把其他环境的授权用于生产 |
| `PASSPORT_INACTIVE` | 停止；检查规则变化或吊销状态，重新审批 |
| `PASSPORT_EXPIRED` | 停止；由原审批人重新签发 |
| `PASSPORT_REPLAY` | 停止并调查重复流水线或重试配置 |
| `PASSPORT_UNAVAILABLE` | 停止；修复服务端签名配置 |
| 5xx、超时、网络错误 | 失败关闭，不绕过 Gate |

不要对 409 或 410 自动重试部署。网络错误可以重试只读 `verify`；`consume` 结果不明确时先查询通行证状态，不要盲目重复生产命令。

## 构建 CLI

Windows：

```powershell
go build -o changeguard-gate.exe ./cmd/changeguard-gate
.\changeguard-gate.exe digest -manifest .changeguard.json
```

Linux/macOS：

```bash
go build -o changeguard-gate ./cmd/changeguard-gate
./changeguard-gate digest -manifest .changeguard.json
```

仓库内的完整示例见 [`examples/ci-demo`](../examples/ci-demo/README.md)。

## 外部协议参考

- [GitLab Webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/)
- [GitLab Pipeline events](https://docs.gitlab.com/user/project/integrations/webhook_events/#pipeline-events)
- [Jenkins HTTP Request step](https://www.jenkins.io/doc/pipeline/steps/http_request/)
- [Jenkinsfile credentials](https://www.jenkins.io/doc/book/pipeline/jenkinsfile/)
