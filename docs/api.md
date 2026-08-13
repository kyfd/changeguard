# ChangeGuard HTTP API

本文记录仓库当前实现的 v1 接口。浏览器业务接口使用企业会话与 CSRF；CI Gate 只接受一次性通行证 Bearer Token。

## 1. 通用约束

- JSON 请求体上限为 2 MiB，并拒绝未知字段和尾随 JSON。
- 浏览器写操作需要有效会话、`X-CSRF-Token` 与服务端权限校验。
- Gate Token 只能放在 `Authorization: Bearer <token>`；请求体中的 `token` 字段会被拒绝。
- 通行证签发响应包含一次性明文 Token，并设置 `Cache-Control: no-store`；服务端仅保存 Token SHA-256。
- 新建变更只接受 `DATABASE`、`CONFIG`、`KUBERNETES` 三类制品。
- `POST /api/changes/{id}/experiment`、`POST /api/changes/{id}/approve`、`POST /api/changes/{id}/passports` 支持 `Idempotency-Key`。Key 可省略以兼容旧客户端（响应含 `Idempotency-Status: not-requested`，明确该调用非幂等）；提供时必须为 8～128 个 ASCII 字符，仅允许字母、数字、`.`、`_`、`:`、`-`。
- 幂等范围为企业、操作者、操作和资源。同 Key、同请求摘要重试返回首次成功结果并设置 `Idempotency-Replayed: true`；同 Key、不同请求摘要返回 `409 IDEMPOTENCY_KEY_CONFLICT`，不会再次排队、审批或签发。

普通错误响应：

```json
{
  "error": "请求参数不完整：示例原因",
  "code": "BAD_REQUEST",
  "message": "请求参数不完整：示例原因"
}
```

Gate 错误响应：

```json
{
  "allowed": false,
  "code": "ARTIFACT_MISMATCH",
  "reason": "制品摘要与变更通行证不匹配"
}
```

## 2. 健康、认证与企业

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | 应用健康摘要 |
| GET | `/health/live` | 存活检查 |
| GET | `/health/ready` | 就绪检查 |
| GET | `/metrics` | 受 Bearer Token 保护的指标 |
| GET | `/api/auth/status` | 认证模式与入口状态 |
| GET | `/api/auth/me` | 当前成员 |
| GET | `/api/auth/session` | 当前成员、企业与 CSRF Token |
| POST | `/api/auth/register` | 本地/混合模式注册企业 |
| POST | `/api/auth/login` | 本地/混合模式登录 |
| POST | `/api/auth/invitations/accept` | 接受企业邀请 |
| GET/PUT | `/api/enterprise` | 查询或更新企业 |
| GET | `/api/enterprise/members` | 成员列表 |
| PUT | `/api/enterprise/members/{id}` | 更新角色、状态和应用授权 |
| GET/POST | `/api/enterprise/invites` | 查询或创建邀请 |
| DELETE | `/api/enterprise/invites/{id}` | 撤销邀请 |

## 3. 应用、规则与审计

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/dashboard` | 当前成员可见的聚合数据 |
| GET/POST | `/api/apps` | 应用列表或创建应用 |
| GET/PUT | `/api/apps/{id}` | 查询或更新应用 |
| GET | `/api/users` | 当前企业可见成员 |
| GET/POST | `/api/policies` | 风险规则列表或创建规则 |
| POST | `/api/policies/test` | 只读规则试跑，不形成可签证证据 |
| GET | `/api/policies/export` | 导出规则 |
| PUT | `/api/policies/{id}` | 更新规则并提升版本 |
| POST | `/api/policies/{id}/toggle` | 启用或停用规则 |
| GET | `/api/audits?limit=250` | 审计列表 |
| GET | `/api/events` | 服务端事件流 |
| GET | `/api/operations/outbox` | 异步演练 Outbox 状态 |

新审计 JSON 在兼容既有字段的基础上增加 `request_id`、`actor_type`、`auth_method`、`resource_type`、`resource_id`、`resource_version_before/after`、`request_digest`、`result`、`reason_code`、`related_event_id`、`attempt_id`、`passport_id`、`prev_hash`、`hash`。旧事件的 hash 可以为空；新事件按企业链接。Evidence Bundle 当前通过只读 CLI 提供，而不是新增 HTTP 下载端点：

```text
go run ./cmd/changeguard-evidence export -data ./data/dbguard.json -change <change-id> -out evidence.json
go run ./cmd/changeguard-evidence verify -in evidence.json
```

`verify` 完全离线，成功退出 0；Manifest、change binding 或 audit chain 任一不匹配均非零退出。Bundle 不包含 artifact/SQL 正文、明文 Token、Token hash 或未脱敏 secret。

## 4. 变更单

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/POST | `/api/changes` | 查询或创建变更 |
| GET/PUT | `/api/changes/{id}` | 查询或更新变更 |
| POST | `/api/changes/{id}/submit` | 计算规则版本并执行确定性检查 |
| POST | `/api/changes/{id}/experiment` | SQL 变更进入 PostgreSQL 影子验证队列 |
| POST | `/api/changes/{id}/approve` | 独立审批通过 |
| POST | `/api/changes/{id}/reject` | 驳回 |
| POST | `/api/changes/{id}/comments` | 添加审计评论 |
| POST | `/api/changes/{id}/findings/{findingId}/assign` | 分派整改 |
| POST | `/api/changes/{id}/findings/{findingId}/resolve` | 提交整改说明 |
| POST | `/api/changes/{id}/findings/{findingId}/verify` | 独立复核整改 |
| GET | `/api/changes/{id}/report?format=md` | Markdown 证据报告 |
| GET | `/api/changes/{id}/report?format=xlsx` | Excel 证据报告 |

生产完成没有人工接口：只有 `/api/gate/consume` 成功后，服务端才会在同一次持久化操作中把通行证设为 `CONSUMED`，并把变更设为 `COMPLETED`。

### 4.1 创建配置变更示例

```json
{
  "title": "订单服务关闭生产调试模式",
  "application_id": "app_order",
  "environment": "生产环境",
  "change_type": "配置变更",
  "artifacts": [
    {
      "kind": "CONFIG",
      "name": "生产配置变更",
      "source": "人工提交",
      "language": "YAML",
      "content": "debug: false\nauth_enabled: true\n"
    }
  ],
  "rollback_plan": "恢复上一版本配置并滚动重启订单服务",
  "release_plan": {
    "strategy": "金丝雀发布",
    "canary_percent": 10,
    "observation_minutes": 15,
    "auto_rollback": true,
    "success_metrics": ["HTTP 5xx", "P99 延迟", "下单成功率"]
  },
  "planned_at": "2026-08-01T20:00:00+08:00"
}
```

服务端对原始制品字节计算 `content_sha256`，持久化与页面展示可以脱敏；聚合后的 `artifact_sha256` 同时绑定环境、变更类型、制品元数据与内容摘要、SQL/回滚 SQL 以及回滚方案。

### 4.2 证据语义

- 配置和 Kubernetes：`check_run.status = PASSED`、`blocking = 0`，且摘要与规则版本一致，才可进入审批。
- SQL：除确定性检查外，还必须有 `mode = POSTGRES`、`status = PASSED` 且回滚脚本已在影子事务中实际执行。
- `DEMO_ONLY`、`NOT_RUN`、`FAILED` 均不能审批为可发布证据，也不能签发生产通行证。
- 提交人不能审批自己的变更；高风险变更要求技术负责人审批。

## 5. 变更通行证

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/changes/{id}/passports` | 查询通行证元数据，不返回明文 Token |
| POST | `/api/changes/{id}/passports` | 由实际审批人签发短时一次性通行证 |
| POST | `/api/changes/{id}/passports/{passportId}/revoke` | 撤销尚未消费的通行证 |

签发请求可省略有效期，默认由 `DBGUARD_PASSPORT_TTL` 控制；自定义值必须在 60～1800 秒之间：

```json
{ "ttl_seconds": 600 }
```

签发成功只在本次响应中返回明文 Token：

```json
{
  "passport": {
    "id": "pass_...",
    "change_id": "chg_...",
    "artifact_sha256": "64位十六进制摘要",
    "environment": "生产环境",
    "rule_set_version": "sha256:...",
    "approver_id": "usr_reviewer",
    "status": "ACTIVE",
    "issued_at": "2026-07-31T12:00:00Z",
    "expires_at": "2026-07-31T12:10:00Z"
  },
  "token": "cg1...."
}
```

同一变更存在尚未过期的 `ACTIVE` 通行证时，重复签发会失败；应先消费、撤销或等待过期。

携带 `Idempotency-Key` 的首次签发成功仍只返回一次明文 Token。相同请求重试不会持久化或重显 Token，也不会再签发；返回 `200`、`Idempotency-Replayed: true` 和稳定安全结果：

```json
{
  "passport": { "id": "pass_...", "status": "ACTIVE" },
  "code": "PASSPORT_ALREADY_ISSUED_TOKEN_NOT_REPLAYABLE",
  "message": "通行证已签发；明文 Token 仅在首次成功响应中显示，不能重显或重新签发"
}
```

客户端若丢失首次响应中的 Token，应撤销该通行证后使用新的 Idempotency-Key 显式重新签发；幂等记录仅保存公开通行证快照和 `passport:<id>` 安全引用，不保存明文 Token。

## 6. CI Gate

### 6.1 只读验签

```http
POST /api/gate/verify
Authorization: Bearer cg1....
Content-Type: application/json

{
  "artifact_sha256": "由 changeguard-gate 从实际文件重算",
  "environment": "生产环境",
  "consumer": "gitlab-pipeline-1024"
}
```

### 6.2 原子消费

`POST /api/gate/consume` 使用相同请求格式。成功响应：

```json
{
  "allowed": true,
  "code": "GATE_ALLOWED",
  "reason": "制品摘要、环境、规则版本、审批人和有效期均匹配",
  "passport": {
    "status": "CONSUMED",
    "consumed_by": "gitlab-pipeline-1024"
  }
}
```

一次消费成功后，同一 Token 再次使用返回 `409 PASSPORT_REPLAY`。

### 6.3 Gate 错误码

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `INVALID_REQUEST` / `VALIDATION_ERROR` | 请求结构或 consumer 不合法 |
| 401 | `TOKEN_REQUIRED` | 缺少严格 Bearer Token |
| 403 | `ARTIFACT_MISMATCH` | 实际文件摘要与审批摘要不同 |
| 403 | `ENVIRONMENT_MISMATCH` | 目标环境不同 |
| 403 | `PASSPORT_INVALID` | 签名、声明、状态绑定或变更绑定不合法 |
| 409 | `PASSPORT_REPLAY` | 已消费，拒绝重放 |
| 409 | `PASSPORT_INACTIVE` | 已撤销或规则版本变化 |
| 410 | `PASSPORT_EXPIRED` | 已过期 |
| 503 | `PASSPORT_UNAVAILABLE` | 未配置签名密钥 |

## 7. 冲突雷达与流水线事件

### 7.1 变更冲突雷达

`GET /api/conflicts` 使用当前登录成员可见的应用与变更，默认分析最近 24 小时到未来 8 天。可选参数：

- `from`：RFC3339
- `to`：RFC3339

响应包含计划变更轨道、冲突数量、高危数量、受影响应用、等级分布和可解释原因。检测口径：

- 同一环境、同一应用的计划窗口重叠；
- SQL 写入同一表、配置修改同一文件与 Key、Kubernetes 修改同一命名空间/Kind/Name；
- 应用模型中直接上下游依赖在同一窗口变更。

计划窗口使用 `planned_at` 到发布观察期结束；观察期最低按 30 分钟、最高按 8 小时计算。已阻断、已驳回、已闭环变更不参与冲突计算。

### 7.2 集成状态与事件

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/integrations/status` | 企业会话 | 返回 GitLab/Jenkins/Operations 是否已配置、端点和最近接收时间，不返回密钥 |
| GET | `/api/integrations/events?limit=100` | 企业会话 | 最近流水线事件 |
| POST | `/api/integrations/gitlab/webhook` | GitLab Signing Token 或 `X-Gitlab-Token` | 接收 Pipeline Hook |
| POST | `/api/integrations/jenkins/events` | Bearer Token | 接收 Jenkins 构建事件 |
| GET | `/api/integrations/operations/events?limit=100` | 企业会话 | 当前成员可见应用的事故、回滚和业务 SLI 证据 |
| POST | `/api/integrations/operations/webhook` | 独立 Bearer Token | 接收权威发布后运维结果事件 |

GitLab 新版签名按 `webhook-id.webhook-timestamp.raw_body` 计算 HMAC-SHA256，并校验时间窗口；重复的 `webhook-id` 幂等处理。

Jenkins 请求体：

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

外部事件只形成集成记录和审计事件，不会直接把变更设为批准或完成。正式发布仍必须通过 `/api/gate/verify` 与 `/api/gate/consume`。

Operations 事件必须携带稳定 `event_id`、真实 `change_id`、来源与 `occurred_at`。首次写入返回 202；同一组织、来源和 `event_id` 的重试返回 200 且不重复写入。支持三类：

- `INCIDENT`：`OPEN/TRIGGERED/ACKNOWLEDGED/RESOLVED/CLOSED`，必须提供 `incident_id`；
- `ROLLBACK`：`STARTED/SUCCEEDED/FAILED/CANCELED/CANCELLED`，必须提供 `operation_id`；
- `BUSINESS_SLI`：必须提供不重叠的发布前/后窗口、方向、基线值和观察值，可选目标值与容差。

完整协议、示例和聚合口径见 [发布后运维结果接入](operations-outcomes.md)。

## 8. 状态机

```text
DRAFT
  -> CHECK_FAILED -> 整改/独立复核 -> WAITING_APPROVAL（配置/K8s）
                                \-> READY_FOR_EXPERIMENT（SQL）
  -> WAITING_APPROVAL（配置/K8s 检查直接通过）
  -> READY_FOR_EXPERIMENT -> EXPERIMENT_QUEUED -> EXPERIMENT_RUNNING
                          -> WAITING_APPROVAL / CHECK_FAILED
WAITING_APPROVAL -> APPROVED / REJECTED
APPROVED -> COMPLETED（仅 Gate consume）
```

任何制品内容、环境或规则版本变化都会使旧检查/审批/通行证失效，必须重新检查。
