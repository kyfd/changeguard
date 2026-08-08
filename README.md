# ChangeGuard

ChangeGuard 是面向小型软件企业的轻量级生产变更风险门禁。它把 SQL、应用配置和 Kubernetes 清单放进同一张变更单，执行可解释的确定性检查、整改与独立审批，再向 CI/CD 发放与实际文件摘要绑定的一次性“变更通行证”。

它解决的不是“再做一个数据库管理页面”，而是企业愿意持续付费的问题：

> 审批时看到的是 A，流水线实际部署的却可能是 B；审核标准分散在个人经验里，事故后又难以还原谁批准了什么。

## 企业为什么愿意买单

- **阻止审批后替换制品**：通行证绑定 SHA-256、目标环境、规则版本和实际审批人；CI 从真实发布文件重算摘要，任何变化立即阻断。
- **覆盖三类高频生产变更**：同一套流程检查 SQL、配置和 Kubernetes，避免企业分别购买或维护三个零散工具。
- **减少重复人工审核**：明文凭据、危险 SQL、特权容器、latest 镜像、缺失探针等问题先由确定性规则筛出。
- **避免发布窗口互相踩踏**：同一服务与直接上下游在保护窗口内发生冲突时给出阻断或提示证据。
- **形成可追溯证据链**：提交、检查、整改、复核、审批、通行证签发/消费/撤销进入同一条审计记录。
- **不替换现有基础设施**：继续使用 Git、Jenkins、GitLab CI 和 Kubernetes，只增加部署前门禁，接入成本适合小团队。
- **决策依据可信**：没有真实执行就明确标记 `NOT_RUN`/`DEMO_ONLY`；不使用随机性能数据或大模型结论放行生产。

建议按“纳管服务数”订阅，而不是按席位收费。服务数更接近风险覆盖面积，也不会抑制开发、审核和运维协作。详见 [产品说明](docs/product.md)。

## v1 核心范围

| 门禁类型 | 输入 | 典型检查 | 可签证条件 |
| --- | --- | --- | --- |
| SQL | 执行 SQL、回滚 SQL、回滚方案 | 无条件 UPDATE/DELETE、危险 DDL、缺少回滚、非并发索引等 | 确定性检查通过，并在隔离 PostgreSQL 影子事务中实际执行 SQL 与回滚脚本 |
| 配置 | YAML、JSON、ENV 风格配置 | 疑似明文凭据、生产 debug/trace、关闭鉴权、关闭 TLS 校验 | 当前规则版本的确定性检查无未闭环阻断项 |
| Kubernetes | Deployment、StatefulSet、Pod 等清单 | latest、privileged、提权、root、hostPath、宿主命名空间、资源/探针/副本 | 当前规则版本的确定性检查无未闭环阻断项 |

三类制品共享一条最小闭环：

1. 开发者创建变更单，填写发布窗口、回滚方案并提交制品。
2. ChangeGuard 对原始字节计算摘要，持久化前脱敏，并执行确定性规则。
3. 阻断项必须经过分派、整改和独立复核；SQL 还要进入真实 PostgreSQL 影子验证。
4. 非提交人核对证据后批准或拒绝；高风险变更需要技术负责人。
5. 实际审批人签发短时一次性通行证，明文 Token 只返回一次。
6. CI 使用 `.changeguard.json` 和 `changeguard-gate` 从实际文件重算摘要，再 `verify`/`consume`。
7. `consume` 成功后门禁记录原子闭环；摘要、环境、规则、状态或有效期不匹配时拒绝部署。

## 可信证据原则

- 静态检查只证明当前规则是否命中，不假装测量生产性能。
- SQL 未配置真实影子库时返回 `DEMO_ONLY / NOT_RUN`，不能推进审批或签发生产通行证。
- PostgreSQL 影子验证证明脚本可在隔离事务中执行，并实际执行回滚脚本；它不声称业务数据已完全恢复等价。
- 通行证证明“本次流水线文件与审批对象一致”，不替代备份、灰度、监控和部署后验证。
- `COMPLETED` 表示一次性治理门禁已消费，不代表部署后的服务一定健康。

## 本地运行

要求：Go 1.23.x。默认文件存储和内存会话无需 PostgreSQL/Redis。

PowerShell 本地演示：

```powershell
$env:DBGUARD_AUTH_MODE = "local"
$env:DBGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:DBGUARD_ENABLE_DEMO_DATA = "true"
# 仅限本地演示；生产必须由密钥管理系统注入随机值
$env:DBGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
# 本地联调 Token；生产请由密钥管理系统注入
$env:DBGUARD_GITLAB_WEBHOOK_SECRET = "gitlab-local-webhook-secret"
$env:DBGUARD_JENKINS_WEBHOOK_TOKEN = "jenkins-local-webhook-token-32-bytes"
$env:DBGUARD_OPERATIONS_WEBHOOK_TOKEN = "operations-local-webhook-token-32-bytes"
go run ./cmd/dbguard
```

打开 <http://localhost:8080>。终端中的服务可按 `Ctrl+C` 优雅关闭；不需要、也不应结束系统里的所有 `powershell.exe` 进程。

未配置真实 PostgreSQL 时，配置与 Kubernetes 仍可走完整检查、审批和通行证流程；SQL 只能完成静态检查，不能形成生产可签证证据。启用 SQL 影子验证：

```powershell
$env:DBGUARD_EXPERIMENT_MODE = "postgres"
$env:DBGUARD_SHADOW_DSN = "postgres://runner:password@127.0.0.1:5432/changeguard_shadow?sslmode=disable"
```

影子库必须与生产隔离，账号不得拥有生产权限。

GitLab/Jenkins 发布终态之外，事故、真实回滚执行和业务 SLI 前后对比可通过独立 Operations webhook 回传。协议与安全边界见 [发布后运维结果接入](docs/operations-outcomes.md)。

### 演示账号

仅当 `DBGUARD_ENABLE_DEMO_ACCOUNTS=true` 时创建，密码均为 `Demo1234`：

| 用户 ID | 登录邮箱 | 角色 | 典型操作 |
| --- | --- | --- | --- |
| `usr_developer` | `developer@example.com` | 后端开发 | 创建、提交、整改 |
| `usr_reviewer` | `reviewer@example.com` | 发布审核人 | 复核、审批、签发通行证 |
| `usr_owner` | `owner@example.com` | 技术负责人 | 管理应用、成员、规则和高风险审批 |

这些账号只能用于本地演示，生产必须关闭。

### Docker Compose

```powershell
Copy-Item .env.example .env
# 替换 .env 中的 DBGUARD_PASSPORT_HMAC_SECRET 与数据库密码
docker compose up --build
```

Compose 默认启用 PostgreSQL 影子验证，因此需要影子数据库服务正常就绪。

## AI Agent 工程化与离线评测

Agent 通过 OpenAI-Compatible API 接入 DeepSeek 等模型，但只承担风险解释与整改建议：三个工具均为只读，模型不能执行命令、修改审批或触发发布；最终风险由 Go 后端与确定性规则取较高值。

生产化保护包括可配置 HTTP 超时、仅针对网络/429/5xx 的有限重试、连续失败熔断、用户/组织/全局配额以及无模型时的本地证据归纳。`/metrics` 在原有 HTTP/Outbox 指标之外输出模型调用、成功/失败、重试、fallback、Tool Calling、耗时和熔断状态。

仓库内置 24 条完全离线的固定评测用例，不调用付费模型：

```powershell
go run ./cmd/changeguard-agent-eval
go run ./cmd/changeguard-agent-eval -json
```

评测覆盖三项必需工具调用、风险等级一致性、提示词注入边界、伪造证据、缺失工具、错误 JSON、超时降级和临时 5xx 重试恢复。命令返回非零退出码时可直接阻断 CI。

## CI/CD 接入

构建 CLI：

```powershell
go build -o changeguard-gate.exe ./cmd/changeguard-gate
.\changeguard-gate.exe digest -manifest .changeguard.json
```

生产流水线把 `CHANGEGUARD_URL` 与 `CHANGEGUARD_TOKEN` 放入 masked/protected secret，随后执行：

```text
changeguard-gate verify  -manifest .changeguard.json -consumer <pipeline-id>
changeguard-gate consume -manifest .changeguard.json -consumer <pipeline-id>
```

不要直接回传页面摘要，也不要把 Token 写进 JSON 请求体或命令日志。完整示例见 [CI/CD 接入指南](docs/ci-integration.md)。

流水线状态同步接口：

- GitLab：`POST /api/integrations/gitlab/webhook`，优先支持 HMAC-SHA256 Signing Token，兼容 `X-Gitlab-Token`。
- Jenkins：`POST /api/integrations/jenkins/events`，使用独立 Bearer Token 和稳定 JSON 协议。
- 状态与事件：登录后访问 `GET /api/integrations/status`、`GET /api/integrations/events`。
- 冲突雷达：登录后访问 `GET /api/conflicts`，默认分析最近 24 小时到未来 8 天的计划窗口。

## 验证与测试

仓库标准验证入口：

```powershell
go test ./...
go vet ./...
node --check internal/httpapi/web/api-adapter.js
node --check internal/httpapi/web/app.js
docker build -t changeguard:local .
```

本次已通过 `go test ./...` 与 `go vet ./...`；Docker、race、真实 PostgreSQL 影子库与浏览器端到端的执行状态，见 [系统测试记录](docs/final-test-report-2026-07-31.md)。

## 项目结构

```text
cmd/dbguard/                  服务入口
cmd/changeguard-gate/         CI 实际文件摘要、验签与消费 CLI
cmd/changeguard-agent-eval/   Agent 离线评测与 JSON 报告 CLI
cmd/loadtest/                 负载测试入口
internal/auth/                本地登录、会话、OIDC 与企业权限
internal/changegate/          摘要、脱敏、配置/K8s 检查与通行证签名
internal/checker/             SQL 与统一发布规则
internal/experiment/          PostgreSQL 影子事务验证
internal/httpapi/             HTTP API 与内置前端
internal/model/               领域模型
internal/service/             状态机、审批与权限编排
internal/store/               文件/PostgreSQL 持久化与原子消费
internal/report/              Markdown/XLSX 证据导出
internal/observability/       健康检查与指标
deploy/                       Docker、Nginx、Kubernetes 部署材料
docs/                         产品、架构、API、CI 与测试文档
```

## 产品边界

ChangeGuard v1 刻意不做：

- Git 托管、构建系统、完整发布编排或 Kubernetes 控制面；
- 连接生产数据库执行 SQL、影子压测、P99/锁等待预测；
- 用 LLM/Agent 自动批准生产变更；
- 完整 ITSM、CMDB、GRC、SIEM 或全链路监控；
- 代码质量、API 兼容性、业务回归和供应链全量扫描。

这些边界让项目保持在“小企业可试点、校招生能完整讲清、又有真实购买理由”的规模。

## 历史兼容说明

产品名称和界面统一使用 **ChangeGuard**。Go module、服务目录和环境变量仍保留 `dbguard` / `DBGUARD_` 历史名称，避免无价值的破坏性重命名。

## 文档

- [产品定位与商业化](docs/product.md)
- [业务流程](docs/business-flow.md)
- [系统架构](docs/architecture.md)
- [HTTP API](docs/api.md)
- [CI/CD 接入](docs/ci-integration.md)
- [企业部署](docs/enterprise-operations.md)
- [Agent 离线评测报告](docs/agent-evaluation-report-2026-08-03.md)
- [系统测试记录](docs/final-test-report-2026-07-31.md)
