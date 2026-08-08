# 发布后运维结果接入

ChangeGuard 的审批和 Gate 证明“部署内容被授权”，GitLab/Jenkins 终态证明“流水线结束”，但两者都不能单独证明发布后没有事故、确实执行了回滚，或业务指标改善。本协议接收这些发布后证据，并保持与变更单一一关联。

## 1. 配置与安全边界

```text
DBGUARD_OPERATIONS_WEBHOOK_TOKEN=<至少 32 字节独立随机 Token>
DBGUARD_OPERATIONS_ORGANIZATION_ID=org_replace_me
```

接收地址：

```text
POST https://changeguard.example.com/api/integrations/operations/webhook
Authorization: Bearer <Token>
Content-Type: application/json
```

该 Token 只授予发布后结果写入能力，不与浏览器会话、Gate 通行证、Jenkins、metrics 或数据库凭据复用。生产代理应限制调用方出口、关闭请求体日志并在密钥管理系统中轮换 Token。

请求体上限为 1 MiB。未知字段、未知变更单、跨组织变更、未来超过 5 分钟或早于 400 天的事件均被拒绝。`event_id` 是来源系统生成的稳定幂等键；同一组织、来源和 `event_id` 的重试返回 200，不追加第二条业务或审计记录。

## 2. 事故关联

```json
{
  "event_id": "servicenow:INC0012345:resolved:20260808T103000Z",
  "source": "SERVICENOW",
  "kind": "INCIDENT",
  "status": "RESOLVED",
  "change_id": "chg_123",
  "incident_id": "INC0012345",
  "severity": "SEV2",
  "external_url": "https://itsm.example.com/incidents/INC0012345",
  "detail": "订单错误率恢复到告警阈值以内",
  "occurred_at": "2026-08-08T10:30:00Z"
}
```

支持 `OPEN`、`TRIGGERED`、`ACKNOWLEDGED`、`RESOLVED`、`CLOSED`。同一 `source + change_id + incident_id` 的最早未解决事件和最新状态用于计算当前状态；只有同时存在打开与解决证据时才形成事故解决时长样本。

不要发送告警原文、日志、请求参数、客户标识或密钥。`detail` 只写不超过 1,000 字符的脱敏结论。

## 3. 显式回滚执行

```json
{
  "event_id": "argocd:rollback-20260808-42:succeeded",
  "source": "ARGOCD",
  "kind": "ROLLBACK",
  "status": "SUCCEEDED",
  "change_id": "chg_123",
  "operation_id": "rollback-20260808-42",
  "external_url": "https://argocd.example.com/applications/orders?operation=true",
  "occurred_at": "2026-08-08T10:12:00Z"
}
```

支持 `STARTED`、`SUCCEEDED`、`FAILED`、`CANCELED`、`CANCELLED`。`STARTED` 可以证明发布后开始处置，但不会被计算为回滚成功或失败；同一回滚操作只采用最新终态。

## 4. 发布前后业务 SLI

```json
{
  "event_id": "prometheus:chg_123:checkout_success_rate:20260808",
  "source": "PROMETHEUS",
  "kind": "BUSINESS_SLI",
  "status": "OBSERVED",
  "change_id": "chg_123",
  "metric_name": "checkout_success_rate",
  "metric_unit": "percent",
  "metric_direction": "HIGHER_IS_BETTER",
  "baseline_value": 98.7,
  "observed_value": 99.4,
  "objective_value": 99.0,
  "tolerance": 0.1,
  "baseline_window_start": "2026-08-08T08:00:00Z",
  "baseline_window_end": "2026-08-08T08:30:00Z",
  "observation_window_start": "2026-08-08T09:30:00Z",
  "observation_window_end": "2026-08-08T10:00:00Z",
  "occurred_at": "2026-08-08T10:05:00Z"
}
```

方向只能是 `HIGHER_IS_BETTER` 或 `LOWER_IS_BETTER`。前后窗口必须各自有序且互不重叠，观察窗口结束时间不得晚于事件时间；治理聚合还要求窗口实际夹住同一变更的成功发布终态（允许 5 分钟时钟/投递偏差）。`tolerance` 使用指标自身单位；前后差值绝对值不超过容差时记为稳定。只有提供 `objective_value` 的样本才进入目标达成率分母。

指标名称和值应由只读可观测适配器计算，不允许由 ChangeGuard 随机生成，也不要把带用户、订单、URL 等高基数标签的时序数据复制进事件。

## 5. 返回与审计

首次接收：

```json
{
  "accepted": true,
  "duplicate": false,
  "event_id": "servicenow:INC0012345:resolved:20260808T103000Z",
  "signal_id": "outcome_...",
  "change_id": "chg_123",
  "kind": "INCIDENT"
}
```

首次写入返回 202，幂等重试返回 200。事故、回滚、业务 SLI 分别生成 `INCIDENT_LINKED`、`ROLLBACK_EXECUTION_RECORDED`、`BUSINESS_SLI_RECORDED` 审计事件。外部事件不会批准、完成或重新打开变更，也不会绕过 Gate。

企业成员可通过 `GET /api/integrations/operations/events?limit=100` 查看其应用授权范围内的脱敏事件；聚合结果通过 `GET /api/governance/outcomes?window_days=30` 和受保护的 `/metrics` 提供。

## 6. 上线验收

1. 使用不存在的变更号验证 400 且不落盘。
2. 对同一事件连续发送两次，验证 202/200 且存储与审计只增加一次。
3. 分别发送事故打开/解决、回滚开始/终态、业务 SLI 对比。
4. 验证治理结果的四个 observability 标志与自然样本数。
5. 验证 `/metrics` 不出现 change ID、incident ID、metric name 或 URL。
6. 轮换 Token，并确认旧 Token 立即返回 401。
