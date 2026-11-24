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

`consume` 必须紧邻生产部署命令。`COMPLETED` 只表示 Gate 已使用，不表示后续部署成功。同一流水线在丢失 HTTP 200 后，可以用相同 Token、摘要、环境和 `consumer` 重试 `consume`；服务端返回首次消费快照，不会第二次消费。消费后部署失败时不能把 Token 交给另一条流水线，应记录失败并重新提交审批。

## CI 变量

服务端通行证配置：

```text
CHANGEGUARD_PASSPORT_HMAC_SECRET=<至少 32 字节随机值>
CHANGEGUARD_PASSPORT_TTL=10m
```

生产部署资产使用同一组 `CHANGEGUARD_*` 键名；4.0 起不再接受 `DBGUARD_*` 旧前缀，残留旧键会让核心拒绝启动（迁移见 `deploy/production/changeguard-migrate-env.sh`）。

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

ChangeGuard 可以接收 GitLab Pipeline events，用于记录流水线状态。流水线成功不是部署证据，不能解锁联合发布。

服务端配置：

```text
CHANGEGUARD_GITLAB_SIGNING_TOKEN=<GitLab 19.0+ Signing Token，形如 whsec_...>
CHANGEGUARD_GITLAB_WEBHOOK_SECRET=<迁移期 X-Gitlab-Token>
CHANGEGUARD_GITLAB_ORGANIZATION_ID=org_demo
CHANGEGUARD_GITLAB_WEBHOOK_MAX_AGE=5m
```

Webhook URL：

```text
https://changeguard.example.com/api/integrations/gitlab/webhook
```

启用 Pipeline events 并保持 SSL 校验。ChangeGuard 优先校验 `webhook-id`、`webhook-timestamp` 和 `webhook-signature`，并拒绝超出时间窗口的请求；没有签名头时才使用兼容的 `X-Gitlab-Token`。

事件优先通过 `CHANGEGUARD_CHANGE_ID` 关联变更，其次尝试本组织内的 `commit_sha`。无法关联的事件会保留并标记为未关联，不会猜测或修改变更状态。

### 5.0 联合发布：串联数据库与应用部署

创建第一个联合发布计划之前，必须先升级所有处理 Gate 请求的 ChangeGuard 实例，确保每个实例都能识别计划成员关系并强制执行步骤依赖。滚动升级期间，只要旧实例仍可能收到 `/api/gate/verify` 或 `/api/gate/consume` 请求，就不要启用联合发布；旧版本会把计划中的变更当作普通变更处理。

在启用会话认证的控制台中打开「联合发布」。技术负责人、审核人或企业管理员可创建计划，仍须对涉及的应用有审核权限。逐步固定环境、变更 ID、制品版本与 SHA-256、GitLab 项目 ID 和作业名。每条变更必须独立审批并签发各自的通行证。详情将审批、Gate 消费和 GitLab 终态分开显示；缺证据不算成功。

控制台使用 `GET /api/joint-releases`、`GET /api/joint-releases/{id}` 和 `POST /api/joint-releases`。这三个端点要求有效会话，创建请求还需要 CSRF 令牌；创建替代计划时在同一 POST 中指定 `supersedes` 与相应步骤的 `replaces_change_id`。

在 ChangeGuard 服务进程配置（从密钥管理器提供 token）：

```text
CHANGEGUARD_GITLAB_ORGANIZATION_ID=<ChangeGuard 组织 ID>
CHANGEGUARD_GITLAB_RELEASE_ORIGIN=https://gitlab.example.com
CHANGEGUARD_GITLAB_RELEASE_PROJECTS={"42":"https://gitlab.example.com/commerce/orders"}
CHANGEGUARD_GITLAB_READ_API_TOKEN=<有权读取白名单项目部署的 read_api 令牌>
```

当前配置固定一个组织和一个 HTTPS GitLab 源（不含子路径）；项目 URL 使用 GitLab 返回的完整规范地址，不带 `.git` 或尾斜杠。应用的仓库 URL 必须与绑定项目一致。在白名单项目上把 **Deployment events** 发送到上面的同一路径，启用 SSL 校验。部署事件如果配置了 Signing Token 就必须带有效签名；只使用旧版 Secret 时，确保其独立且足够随机。GitLab read API 凭据只配置在服务端，CI 不需要它；跨多个项目时该凭据须能读取每个白名单项目。

CI 在**实际执行部署、且声明相同 `environment.name` 的作业**中运行 `changeguard-gate consume -gitlab-job -manifest <文件>`，消费成功后才执行变更。作业凭据仅从环境变量 `CI_JOB_TOKEN` 读取，通过 HTTPS 请求头发送，勿把它传入命令行参数或日志；反向代理也不要记录 Gate 或 Webhook 的认证头。ChangeGuard 使用固定源的 `GET /api/v4/job` 核实仍运行的作业、项目、流水线和完整提交。CLI 用 GitLab 注入的 `CI_JOB_ID` 核对消费记录，`consumer` 不是身份凭据。普通非联合发布仍可使用原 Gate 调用；`verify` 只读，成功不代表授权执行。

[GitLab Deployment Hook](https://docs.gitlab.com/user/project/integrations/webhook_events/#deployment-events) 通常只提供短 SHA。ChangeGuard 从同一固定源的 [Deployments API](https://docs.gitlab.com/api/deployments/#retrieve-a-single-deployment) 查询具体项目和部署，核对终态、执行作业、环境及完整 SHA，不访问 Webhook 中的 URL。只有匹配本次已消费作业、并且终态时间晚于消费时刻的成功证据才解锁下一步。请保持 GitLab 和 ChangeGuard 时钟同步。API 不可用时发布保持阻断；恢复后可重发终态 Webhook，不能用流水线状态或手写成功代替。

部署失败或取消后，先查看详情中的执行结果，再用「创建替代计划」为失败步骤选择同一应用的新变更，保持其他未完成步骤的次序。已成功步骤和原证据会被新计划沿用，无需重跑；即使只剩一个步骤，也可以创建替代计划。新变更仍需审批、签发和从新启动的作业消费 Gate；旧作业迟到的成功事件不能解锁新计划。

参考 [可执行 GitLab CI 示例](../examples/ci-demo/.gitlab-ci.joint-release.yml)及[准备说明](../examples/ci-demo/README.md#联合发布示例)。

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
CHANGEGUARD_JENKINS_WEBHOOK_TOKEN=<至少 32 字节随机 Token>
CHANGEGUARD_JENKINS_ORGANIZATION_ID=org_demo
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
CHANGEGUARD_OPERATIONS_WEBHOOK_TOKEN=<至少 32 字节随机 Token>
CHANGEGUARD_OPERATIONS_ORGANIZATION_ID=org_demo
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
| `PASSPORT_REPLAY` | 停止并调查其他流水线是否已消费该通行证 |
| `PASSPORT_UNAVAILABLE` | 停止；修复服务端签名配置 |
| 5xx、超时、网络错误 | 失败关闭，不绕过 Gate |

不要对 409 或 410 自动重试部署。网络超时后，同一 `consumer` 可以重试 `consume` 以确认首次结果；HTTP 200 表示该通行证已由当前 consumer 消费，不表示可以再部署一次。不同 `consumer` 收到 `PASSPORT_REPLAY` 时必须停止。

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
