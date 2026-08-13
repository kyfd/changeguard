# ChangeGuard 信任边界与演示验收

## 一条核心问题

ChangeGuard 要证明的是：**CI 实际部署的文件，就是独立审核人检查并批准的文件。**

```text
开发者提交制品
    │
    ▼
Go 确定性规则 ──阻断──> finding 整改与独立复核
    │通过
    ▼
SQL 真实影子演练（仅 SQL）
    │可信证据
    ▼
独立审批人批准
    │
    ▼
签发摘要/环境/规则版本/审批人绑定的一次性通行证
    │
    ▼
CI 从仓库实际文件重新计算摘要
    │
    ├── 不匹配：拒绝
    ▼
verify / 原子 consume ──重放──> 拒绝
    │
    ▼
门禁记录 COMPLETED
```

## Agent 边界

Evidence Navigator 和模型分析位于门禁旁路：

- `ChangeRequest.risk` 是治理风险，只由确定性规则和真实验证证据产生；
- `analysis.advisory_risk` 是 AI 建议风险，只供解释和整改参考；
- 模型只能调用编译期允许的只读工具；非法 JSON 参数不执行工具并写入 Trace 错误；
- 回答只能引用真实存在的 finding、experiment、change 或 passport；
- Evidence Navigator 只处理阻断原因、下一步、finding 整改、passport/CI Gate 四类问题；
- 未知问题安全降级，不执行证据查询；
- Agent 没有审批、签证、消费 Gate、部署、回滚或升级工具。

因此，模型超时、返回 HIGH/LOW、发生提示注入或不可用，都不能改变检查状态、审批层级、通行证条件与 Gate 结果。

## 高权限升级边界

Web 在线升级 apply 可触发 root watcher，因此默认失败关闭。只有显式设置 `DBGUARD_ENABLE_UPGRADE_APPLY=true` 才开放触发能力；版本、状态和历史查询保持可用。该开关不能替代升级包签名、独立审批、备份恢复、回滚演练和最小权限。

## 5～10 分钟演示脚本

1. **创建安全 CONFIG 变更**：上传实际 `application.yaml`，展示服务端制品摘要。
2. **提交检查**：展示确定性规则结果进入 `WAITING_APPROVAL`；指出 AI 建议风险是独立字段。
3. **演示自审失败**：使用提交人审批，确认被拒绝。
4. **独立审批与签证**：使用 reviewer 批准并签发一次性通行证；明文 Token 只返回一次。
5. **CI 复算摘要**：运行 `changeguard-gate digest -manifest .changeguard.json`，证明摘要来自仓库实际文件。
6. **篡改反例**：修改 YAML 后 consume，确认 `ErrArtifactMismatch`，且通行证未被消耗。
7. **恢复原文件并消费**：consume 成功，变更原子进入 `COMPLETED`。
8. **重放反例**：再次 consume，确认 `ErrPassportReplay`。
9. **证据助手**：分别询问“为什么被阻断/下一步/finding 如何整改/Gate 为什么不可用”，展示不同的真实只读 Tool Trace；输入越权提示，确认状态不变。
10. **审计证据**：展示 `SUBMIT_CHECK`、`APPROVE`、`PASSPORT_ISSUED`、`PASSPORT_VERIFIED`、`PASSPORT_CONSUMED_AND_CHANGE_COMPLETED`。

## 不能夸大的结论

- `COMPLETED` 表示治理通行证已消费，不表示线上服务一定健康；
- PostgreSQL 影子事务证明脚本和回滚脚本可执行，不证明生产数据完全等价；
- `DEMO_ONLY` / `NOT_RUN` 不是可签证证据；
- 固定 Agent 评测证明协议和安全守卫，不证明真实模型业务准确率；
- 当前 PostgreSQL 整体 JSONB 存储未完成规范化拆表前，不宣称多实例高可用；
- 自动部署、自动回滚和 Agent 自动审批不属于当前边界。
