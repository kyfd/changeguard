# ChangeGuard 风险收敛 MVP 验证记录（2026-08-13）

## 验证对象

- 基线提交：`2891e54c37e6e1e40f1d44b6056ecb89ac4809ae`
- 分支：`main`
- 验证对象：基线提交之上的未提交风险收敛 MVP 工作树
- 环境：Windows 10 / Go 1.23 工程 / Node.js 24

本记录只描述实际执行结果，不把历史报告、固定 Agent 用例或静态检查当作未执行环境的替代证据。

## 本次交付覆盖

1. 确定性治理风险与 `advisory_risk` 隔离；AI HIGH/LOW 不改变审批与阻断。
2. Evidence Navigator 四类确定性意图、按需只读证据查询、未知安全降级。
3. Agent 工具参数严格 JSON/对象校验；非法参数不调用工具并留下错误 Trace。
4. 在线升级 apply 默认关闭，显式配置后才可启用；状态查询保持可用。
5. CONFIG 黄金链路自动化：实际文件摘要、提交、自审拒绝、独立审批、签证、verify、篡改拒绝、一次 consume、重放拒绝和审计动作。
6. 统一审计 schema、企业级 SHA-256 哈希链、只读 Evidence Bundle export 和离线 verify；覆盖 Manifest/change binding、字段/文件篡改拒绝及 secret/artifact content 排除。
7. PostgreSQL 核心表保守规范化：changes/outbox/passports/audit/idempotency 独立表、事务回填、legacy JSONB fallback，以及关键路径 PostgreSQL 原子委托。
8. README、企业运维和信任边界演示说明更新。

## 实际执行结果

| 命令 | 结果 | 说明 |
| --- | --- | --- |
| `go test ./internal/store` | 通过 | 包含规范化 SQL 契约测试；可选 PostgreSQL 集成用例因未设置 `DBGUARD_TEST_POSTGRES_DSN` 明确 skipped |
| `go test ./...` | 通过 | 全部 Go 包通过 |
| `go vet ./...` | 通过 | 无 vet 错误 |
| `node --check internal/httpapi/web/api-adapter.js` | 通过 | API adapter 语法通过 |
| `node --check internal/httpapi/web/app.js` | 通过 | 前端主脚本语法通过 |
| `git diff --check` | 通过 | 无空白/补丁格式错误 |
| `go test -race ./...` | 未执行成功 | 当前 Windows Go 环境 `CGO_ENABLED=0`，命令返回 `-race requires cgo`；CI 的 Linux race job仍是权威入口 |
| `docker build -t changeguard:risk-mvp .` | 未执行成功 | Docker Desktop Linux Engine 未运行，无法连接 `dockerDesktopLinuxEngine` |

## 自动化验收证据

### 黄金门禁链路

`cmd/changeguard-gate/main_test.go` 的 `TestGoldenConfigFlowBindsApprovalToActualCIFiles` 覆盖：

- CI 从临时目录中的真实 CONFIG 文件复算摘要；
- 摘要与变更单审核对象一致；
- 提交人自审失败；
- 独立 reviewer 审批和签发通行证；
- verify 不消费；
- 文件篡改后摘要不匹配且不能消费；
- 原始摘要只能 consume 一次；
- 第二次消费判定为 replay；
- 变更原子进入 `COMPLETED`；
- 审计包含 `SUBMIT_CHECK`、`APPROVE`、`PASSPORT_ISSUED`、`PASSPORT_VERIFIED`、`PASSPORT_CONSUMED_AND_CHANGE_COMPLETED`。

### Agent 安全边界

定向测试覆盖：

- AI HIGH 不上调确定性治理审批；
- AI LOW 不降低规则阻断；
- 非法工具参数不会执行工具并进入 ToolCallLog 错误；
- 四类问题产生不同且相关的只读 Trace；
- 引用 ID 对应真实证据；
- `DEMO_ONLY` / `NOT_RUN` 不被描述为通过；
- 提示注入不能产生写工具或改变变更/通行证状态；
- 未知意图不查询证据并安全降级。

### 在线升级边界

定向测试覆盖：

- 默认 `apply_enabled=false`；
- apply 关闭时返回 HTTP 503 和稳定码 `UPGRADE_APPLY_DISABLED`；
- 关闭时不创建 watcher 触发文件；
- 显式启用后仍需管理员/技术负责人权限；
- 状态、版本和历史查询不受 apply 开关影响。

## 明确未验证或未完成

- 未连接真实付费/外部模型执行人工标注业务准确率回归；固定用例仅证明协议守卫。
- 未执行 Docker Compose + PostgreSQL + Redis 的完整部署链路。
- 未执行真实 PostgreSQL 影子库的故障注入与 kill/restart 恢复。
- SQL 演练已实现稳定 `attempt_id`、`PREPARE → APPLY → FINALIZE` checkpoint 与 `lease_generation` fencing；当前 APPLY 仍是单体隔离事务，中断或租约接管会由新 generation 从头重跑，而不是从 SQL 语句级恢复。
- 关键写接口已对 experiment、approve、passport issue 提供 `Idempotency-Key`，其余写接口尚未统一覆盖。
- 主业务审计已形成统一 schema 和企业级应用内 SHA-256 哈希链，并可导出/离线校验 Evidence Bundle；尚未实现外部 WORM、透明日志或可信时间戳锚定，因此不能宣称外部不可抵赖。
- PostgreSQL 已拆 `changeguard_changes`、outbox、passports、audit events、idempotency records；迁移和回填事务内幂等，legacy `dbguard_state` 保留为 fallback/见证。其余组织、成员、应用、规则、集成/结果信号和 Agent 等实体仍在 legacy JSONB；普通保存的核心表是兼容投影，不代表全部业务已规范化。
- 本机未设置 `DBGUARD_TEST_POSTGRES_DSN`，因此真实 PostgreSQL 双实例集成测试 skipped；SQL 契约单测通过，但不声称 Redis/数据库 failover、故障切换或真正 HA 已验证。

## 工作树说明

验证时仓库根目录原有未跟踪 `.zcode/`、`source.bundle`、`source.tar.gz`。本次没有删除、覆盖或提交这些文件，也没有创建 Git commit 或推送远端。
