# 文档索引

ChangeGuard 的文档按使用场景组织。README 提供最小启动说明；这里列出设计、接入和运维细节。

## 产品与流程

- [产品范围](product.md)：用途、支持的制品、角色和非目标。
- [业务流程](business-flow.md)：从提交、检查和审批到 CI 消费的状态变化。
- [治理结果](governance-outcomes.md)：流水线、事故、回滚和业务指标的记录口径。

## 架构与安全

- [系统架构](architecture.md)：核心组件、存储、SQL Outbox、状态机和证据语义。
- [ADR 0001](adr/0001-idempotent-passport-consume.md)：同一 consumer 的 Passport consume 重放。
- [威胁模型](threat-model.md)：TOCTOU、重放、凭据、影子库和审计等威胁与剩余风险。
- [信任边界与验证步骤](trust-boundaries-demo.md)：Gate、模型分析和升级边界的手工核对步骤。
- [Provenance baseline](provenance-baseline.md)：构建来源与发布制品校验。
- [Transaction optimization](transaction-optimization.md)：事务与存储路径的实现说明。

## API 与集成

- [HTTP API](api.md)：浏览器、管理、Gate 和集成接口。
- [CI/CD 接入](ci-integration.md)：清单格式、Gate CLI、GitLab 和 Jenkins。
- [发布后运维结果接入](operations-outcomes.md)：事故、回滚和业务 SLI 事件协议。
- [CI 示例](../examples/ci-demo/README.md)：可运行的 SQL、Kubernetes、GitLab 和 Jenkins 示例。

## 部署与验证

- [部署与运维](enterprise-operations.md)：production profile、systemd、PostgreSQL、Redis、备份、恢复和升级。
- [Benchmark](benchmarks.md)：可重复运行的性能测量及限制。
- [演示步骤](demo-playbook.md)：本地页面和 API 操作顺序。
- [14 天试运行验收](pilot-14-day-acceptance.md)：受控试运行的检查项。

版本变化见仓库根目录的 [CHANGELOG](../CHANGELOG.md)。
