# ChangeGuard 信任边界与验证步骤

ChangeGuard 校验的是：**CI 提交给 Gate 的文件摘要与独立审核人批准的变更版本一致。**

```text
开发者提交制品
    │
    ▼
确定性规则 ──阻断──> 整改并重新检查
    │通过
    ▼
SQL 影子验证（仅 SQL）
    │实际执行成功
    ▼
独立审核人批准
    │
    ▼
签发绑定摘要、环境和规则版本的一次性通行证
    │
    ▼
CI 从工作区文件重新计算摘要
    │
    ├── 不匹配：拒绝
    ▼
verify / 原子 consume ──重放或过期──> 拒绝
    │
    ▼
通行证 CONSUMED，变更 COMPLETED
```

Gate 只能保证其收到的摘要与审批内容一致。CI 仍须保证计算摘要和实际部署使用同一工作区；ChangeGuard 不控制 Git checkout、构建产物替换或集群操作。

## 模型分析边界

Evidence Navigator 和模型分析位于 Gate 判定之外：

- `ChangeRequest.risk` 由确定性规则和真实验证证据产生；
- `analysis.advisory_risk` 仅供解释和整改参考；
- 模型只能调用编译期允许的只读工具，非法参数不会执行工具；
- 回答只能引用已存在的 finding、experiment、change 或 passport；
- 工具集不包含审批、签发、Gate 消费、部署、回滚或升级操作。

模型超时、不可用、提示注入或风险判断变化都不能改变检查状态、审批条件和 Gate 结果。

## 高权限升级边界

Web 升级 `apply` 可触发 root watcher，默认关闭。只有显式设置 `DBGUARD_ENABLE_UPGRADE_APPLY=true` 才开放触发接口；查询版本、状态和历史不受影响。该开关不能替代升级包来源验证、独立审批、备份恢复、回滚演练和 watcher 权限隔离。

## 手工验证步骤

1. 创建 CONFIG 变更并上传实际配置文件，记录服务端制品摘要。
2. 提交检查，确认无阻断项时进入 `WAITING_APPROVAL`。
3. 使用提交人尝试审批，确认服务端拒绝自审。
4. 使用 reviewer 批准并签发通行证，确认明文 Token 只显示一次。
5. 运行 `changeguard-gate digest -manifest .changeguard.json`，确认摘要来自工作区文件。
6. 修改文件后运行 `verify` 或 `consume`，确认返回制品不匹配且 Token 未被消费。
7. 恢复原文件并执行 `consume`，确认通行证变为 `CONSUMED`、变更变为 `COMPLETED`。
8. 再次执行 `consume`，确认重放被拒绝。
9. 检查审计记录中提交、审批、签发、验证和消费事件是否与状态一致。
10. 对 SQL 场景确认只有真实 PostgreSQL 影子迁移和回滚成功后才能进入待审批；`DEMO_ONLY` 和 `NOT_RUN` 必须被拒绝。

## 结论限制

- `COMPLETED` 不表示线上服务健康；
- PostgreSQL 影子事务不证明生产数据分布、负载和锁行为等价；
- `DEMO_ONLY` 和 `NOT_RUN` 不是运行证据；
- 固定 Agent 测试只验证协议和安全约束，不代表模型在所有业务问题上的准确率；
- 自动部署、自动回滚和模型自动审批不在当前信任边界内。
