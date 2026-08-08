# ChangeGuard CI/CD 接入指南

ChangeGuard 的核心不是让流水线回传页面上的摘要，而是让流水线在运行时读取实际发布文件，重新计算与审批端完全相同的摘要，再使用一次性通行证验签和消费。

仓库提供 `cmd/changeguard-gate` CLI，支持：

```text
changeguard-gate digest
changeguard-gate verify
changeguard-gate consume
```

## 1. 安全模型

一次性通行证绑定：

- 企业与变更单；
- 实际审批人；
- 目标环境；
- 当前启用规则版本；
- 聚合制品 SHA-256；
- 签发与过期时间；
- 单次消费状态。

`verify` 是只读检查，可以在流水线早期执行。`consume` 必须紧邻生产部署前执行，并在服务端原子完成三件事：校验绑定、把通行证设为 `CONSUMED`、把治理变更单设为 `COMPLETED`。这里的 `COMPLETED` 表示发布门禁已经一次性使用，不代表应用部署后一定健康。

如果部署在消费后失败，不得重放旧 Token；应创建或重新提交变更，补充失败证据并重新审批。这一选择牺牲少量重试便利，换取清晰的生产发布审计边界。

## 2. 配置

服务端：

```text
DBGUARD_PASSPORT_HMAC_SECRET=<至少 32 字节的随机密钥>
DBGUARD_PASSPORT_TTL=10m
```

CI 使用 masked/protected secret：

```text
CHANGEGUARD_URL=https://changeguard.intra.example
CHANGEGUARD_TOKEN=<审批人签发时只显示一次的 cg1...>
CI_JOB_ID=<流水线自带或自定义稳定标识>
```

优先让 CLI 从环境变量读取 Token，不要把 Token 写入仓库、构建产物、命令回显或普通日志。虽然 CLI 提供 `-token` 参数，但生产流水线不建议使用，以免凭据出现在进程参数中。

## 3. `.changeguard.json`

清单必须位于发布工作区，并只引用清单目录内的相对路径。CLI 会解析符号链接后的真实路径，拒绝目录逃逸、绝对路径与非普通文件。

### 3.1 配置或 Kubernetes

下面的元数据必须与变更单中提交的 `kind`、`name`、`source`、`language` 完全一致；`path` 只用于 CI 找到实际文件：

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

### 3.2 SQL

使用 `sql_path` 时，CLI 会与服务端一样自动建立名为“数据库 SQL”、来源“变更单”、语言“SQL”的数据库制品：

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

### 3.3 摘要一致性规则

- 文件使用原始字节计算 SHA-256；CRLF/LF、末尾换行、空格变化都会改变摘要。
- 制品顺序参与聚合摘要，清单顺序必须与变更单一致。
- `environment` 和 `change_type` 必须与变更单一致。
- `rollback_plan` 与 `rollback_plan_path` 只能选一个；其内容也参与摘要。
- 每个文件上限 10 MiB，全部制品合计上限 25 MiB，清单上限 1 MiB。
- v1 只接受 `DATABASE`、`CONFIG`、`KUBERNETES`。

## 4. 标准流水线

### 4.1 计算并打印非敏感摘要

```bash
changeguard-gate digest -manifest .changeguard.json
```

输出：

```json
{
  "artifact_sha256": "...",
  "environment": "生产环境",
  "change_type": "Kubernetes 变更"
}
```

摘要不是凭据，可以记录在流水线日志中；原始通行证不能记录。

### 4.2 只读验签

```bash
changeguard-gate verify \
  -manifest .changeguard.json \
  -consumer "$CI_JOB_ID"
```

CLI 会从 `CHANGEGUARD_URL` 与 `CHANGEGUARD_TOKEN` 环境变量读取服务地址和 Token，通过 `Authorization: Bearer` 调用 `/api/gate/verify`。

### 4.3 原子消费

在生产部署命令的前一阶段执行：

```bash
changeguard-gate consume \
  -manifest .changeguard.json \
  -consumer "$CI_JOB_ID"

kubectl apply -f deploy/orders.yaml
```

流水线必须启用 fail-fast；CLI 非零退出时不得继续执行任何生产修改。

## 5. GitLab 真实接入

GitLab 使用项目或群组 Webhook 主动同步流水线状态；ChangeGuard 只记录状态和关联关系，不会因为 GitLab 报告成功而绕过审批与 Gate。

服务端配置：

```text
# GitLab 19.0+ 推荐。值形如 whsec_...
DBGUARD_GITLAB_SIGNING_TOKEN=<GitLab Signing Token>
# 旧版 GitLab 或迁移期兼容，可与 Signing Token 同时配置
DBGUARD_GITLAB_WEBHOOK_SECRET=<X-Gitlab-Token>
DBGUARD_GITLAB_ORGANIZATION_ID=org_demo
DBGUARD_GITLAB_WEBHOOK_MAX_AGE=5m
```

在 GitLab 项目或群组的 **Settings → Webhooks** 中配置：

- URL：`https://changeguard.example.com/api/integrations/gitlab/webhook`
- 触发器：`Pipeline events`
- 新版优先生成 Signing Token；旧版填写 Secret Token
- 保持 SSL 校验开启

ChangeGuard 按 GitLab Standard Webhooks 规范校验 `webhook-id`、`webhook-timestamp`、`webhook-signature`，并拒绝超出时间窗口的重放请求；迁移期在没有签名头时回退校验 `X-Gitlab-Token`。

流水线变量推荐携带变更单号：

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

将 `CHANGEGUARD_URL` 与 `CHANGEGUARD_TOKEN` 配置为 protected/masked 变量。不要在 `set -x` 模式下运行含凭据的步骤。

Webhook 会优先使用 `CHANGEGUARD_CHANGE_ID` 关联变更；没有该变量时，ChangeGuard 会尝试使用 `commit_sha` 匹配本企业变更单。找不到关联时仍保留事件并标记“未关联”，不会猜测或自动修改变更状态。

## 6. Jenkins 真实接入

Jenkins 没有统一的出站流水线 Webhook 载荷，因此 ChangeGuard 提供一个稳定、版本可控的接收协议。

服务端配置：

```text
DBGUARD_JENKINS_WEBHOOK_TOKEN=<至少 32 字节随机 Token>
DBGUARD_JENKINS_ORGANIZATION_ID=org_demo
```

接收地址：

```text
POST https://changeguard.example.com/api/integrations/jenkins/events
Authorization: Bearer <Token>
Content-Type: application/json
```

请求体：

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

在 Jenkins Credentials 中创建 Secret text `changeguard-webhook-token`，只授权需要发布的 Folder/Job 使用。下面使用 HTTP Request Plugin，Header 开启 `maskValue`，请求体通过 `JsonOutput` 生成，避免手工拼接 JSON：

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

Gate 阶段仍使用一次性通行证：

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

这里使用 Groovy 单引号字符串，让 Shell 从环境变量读取凭据，避免 Groovy 在命令参数中提前插值敏感值。

## 7. 发布后结果闭环

流水线终态不能替代事故、真实回滚执行或业务收益证据。将 ServiceNow/PagerDuty、Argo CD/发布编排器和 Prometheus/业务指标平台通过只读适配器接入：

```text
DBGUARD_OPERATIONS_WEBHOOK_TOKEN=<至少 32 字节独立随机 Token>
DBGUARD_OPERATIONS_ORGANIZATION_ID=org_demo
```

适配器调用 `POST /api/integrations/operations/webhook`，每条事件携带稳定幂等 `event_id` 和实际 `change_id`。事件只追加脱敏证据与审计，不修改审批或 Gate 状态。三类载荷、窗口约束与验收步骤见 [发布后运维结果接入](operations-outcomes.md)。

## 8. 失败处置

| Gate 结果 | 流水线动作 |
| --- | --- |
| `ARTIFACT_MISMATCH` | 阻断；确认实际文件、换行、元数据与回滚方案是否与审批一致 |
| `ENVIRONMENT_MISMATCH` | 阻断；不能把预发布授权用于生产 |
| `PASSPORT_INACTIVE` | 阻断；规则已变化或 Token 已撤销，重新检查审批 |
| `PASSPORT_EXPIRED` | 阻断；由原审批人重新签发 |
| `PASSPORT_REPLAY` | 阻断并调查重复流水线或重试配置 |
| `PASSPORT_UNAVAILABLE` | 阻断；修复服务端签名密钥配置 |
| 5xx / 网络错误 | 保持 fail-closed，不得绕过 Gate |

不要对 409/410 自动重试部署。网络类失败可以重试只读 `verify`；`consume` 返回结果不明确时，应先查询通行证状态，不得盲目重复生产命令。

## 9. 本地构建 CLI

```powershell
go build -o changeguard-gate.exe ./cmd/changeguard-gate
.\changeguard-gate.exe digest -manifest .changeguard.json
```

Linux/macOS：

```bash
go build -o changeguard-gate ./cmd/changeguard-gate
./changeguard-gate digest -manifest .changeguard.json
```

## 10. 官方协议参考

- GitLab Webhooks 与 Signing Token：https://docs.gitlab.com/user/project/integrations/webhooks/
- GitLab Pipeline Hook 载荷：https://docs.gitlab.com/user/project/integrations/webhook_events/#pipeline-events
- Jenkins HTTP Request Pipeline Step：https://www.jenkins.io/doc/pipeline/steps/http_request/
- Jenkinsfile 凭据与安全插值：https://www.jenkins.io/doc/book/pipeline/jenkinsfile/
