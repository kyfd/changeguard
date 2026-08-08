# ChangeGuard 企业部署与运维

## 1. 部署目标

ChangeGuard v1 是现有 Git 与 CI/CD 之间的发布门禁，不直接执行生产 SQL，也不操作 Kubernetes 集群。小型企业推荐部署为：

~~~text
Browser / CI
     |
 HTTPS Ingress or Nginx
     |
ChangeGuard instance(s)
     |-- PostgreSQL: 企业、权限、变更、规则、通行证元数据、审计
     |-- Redis: 多实例会话与登录限流
~~~

本地演示可以使用 file + memory，不依赖外部服务。生产部署建议 PostgreSQL + Redis，并在通行证原子消费测试通过后再启用多实例。

仓库保留 experiment 与 Outbox 作为 SQL 影子验证的异步执行通道。只有 mode=POSTGRES 且 SQL 与回滚脚本在隔离影子事务中均实际执行成功，结果才可作为 SQL 进入审批的证据；DEMO_ONLY、NOT_RUN 和历史模拟结果都不能放行。配置与 Kubernetes 不经过该通道。

## 2. 推荐环境

| 场景 | 服务实例 | 业务存储 | 会话 | 适用范围 |
| --- | --- | --- | --- | --- |
| 本地演示 | 1 | file | memory | 功能演示、简历答辩 |
| 团队试点 | 1 | PostgreSQL | Redis 或 memory | 5～20 个服务 |
| 小型企业 | 2+ | PostgreSQL | Redis | 20～100 个服务，需要高可用 |

多实例的前提：

- 实例不依赖本地文件保存共享业务状态；
- 状态迁移使用事务或乐观锁；
- 通行证消费是数据库级原子操作；
- 会话存储共享；
- 健康检查、备份恢复和滚动升级经过验证。

## 3. 启动与停止

本地 PowerShell：

~~~powershell
$env:DBGUARD_AUTH_MODE = "local"
$env:DBGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:DBGUARD_ENABLE_DEMO_DATA = "true"
# 仅供本地演示；生产必须从密钥管理系统注入随机值
$env:DBGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
go run ./cmd/dbguard
~~~

打开 http://localhost:8080。按 Ctrl+C 可停止当前 Go 服务，程序会处理退出信号并优雅关闭。不要为了停止应用而结束系统中所有 powershell.exe。

Docker Compose 演示：

~~~powershell
Copy-Item .env.example .env
# 编辑 .env：本地演示可设置 local + demo accounts。
docker compose up --build
~~~

停止并保留数据：

~~~powershell
docker compose down
~~~

删除数据卷会导致数据不可恢复，只有在明确需要重置演示环境并已确认目标卷后才执行。

## 4. 关键配置

历史兼容原因，环境变量继续使用 DBGUARD_ 前缀。

| 变量 | 用途 | 生产建议 |
| --- | --- | --- |
| PORT | HTTP 监听端口 | 由容器或平台注入 |
| DBGUARD_STORE_MODE | file 或 postgres | postgres |
| DBGUARD_PRIMARY_DSN | PostgreSQL DSN | 从 secret 注入 |
| DBGUARD_DB_MAX_CONNS | 最大连接数 | 按实例数和数据库容量配置 |
| DBGUARD_SESSION_MODE | memory 或 redis | 多实例使用 redis |
| DBGUARD_REDIS_URL | Redis 地址 | 从 secret 注入并限制网络 |
| DBGUARD_AUTH_MODE | local、oidc 或 hybrid | 优先 oidc/hybrid |
| DBGUARD_ENABLE_DEMO_ACCOUNTS | 是否创建演示凭据 | 必须为 false |
| DBGUARD_ENABLE_DEMO_DATA | 是否加载演示企业、应用和变更 | 必须为 false |
| DBGUARD_PUBLIC_URL | 外部 HTTPS 地址 | 与 OIDC 回调和 Cookie 配置一致 |
| DBGUARD_AUTH_SECURE_COOKIE | Secure Cookie | HTTPS 生产环境启用 |
| DBGUARD_TRUST_PROXY_HEADERS | 是否信任代理头 | 仅在只接受可信代理流量时启用 |
| DBGUARD_METRICS_TOKEN | /metrics Bearer token | 必须设置并作为 secret 管理 |
| DBGUARD_OPERATIONS_WEBHOOK_TOKEN | 事故、回滚与业务 SLI 接收 Token | 独立长随机 secret，不与其他凭据复用 |
| DBGUARD_OPERATIONS_ORGANIZATION_ID | Operations 事件归属企业 | 显式设置为目标组织 ID |
| DBGUARD_PASSPORT_HMAC_SECRET | 通行证 HMAC 密钥，至少 32 字节 | 从密钥管理系统注入并定期轮换 |
| DBGUARD_PASSPORT_TTL | 通行证默认有效期 | 默认 10m；只允许 1～30m |
| DBGUARD_EXPERIMENT_MODE | SQL 验证模式 | 生产签证场景使用 postgres；demo_only 不可放行 |
| DBGUARD_SHADOW_DSN | 隔离 PostgreSQL 影子库 DSN | 与生产完全隔离、最小权限、从 secret 注入 |
| DBGUARD_WORKERS | SQL 影子验证 Outbox worker 数；`0` 显式禁用后台协调与消费 | 生产按验证并发与影子库容量设置；只读迁移预演使用 `0` |

不要把 DSN、OIDC client secret、Redis 凭据、metrics/Operations token 或变更通行证写入镜像、Git、日志和工单正文。

文件状态升级必须可重复执行：旧 demo 制品补全仅在显式启用 demo 数据时执行；demo 制品和组织默认策略补全使用基于业务作用域的确定性迁移 ID。候选在同一数据副本上连续启动两次后，第二次不得继续改变规范化后的 JSON。

制品接入时的 `content_sha256` 必须绑定脱敏前原始字节；后续加载已持久化记录时保留该合法摘要，只对展示内容重复执行幂等脱敏，禁止用 `[REDACTED]` 文本覆盖原始完整性证据。

## 5. 健康检查与指标

~~~bash
curl --fail http://127.0.0.1:8080/health/live
curl --fail http://127.0.0.1:8080/health/ready
curl --fail   -H "Authorization: Bearer $DBGUARD_METRICS_TOKEN"   http://127.0.0.1:8080/metrics
~~~

建议：

- liveness 只判断进程是否存活；
- readiness 检查提供请求所需的关键依赖；
- metrics 只允许监控网络访问；
- 日志聚合平台对 Authorization、Cookie、password、secret、token 字段做脱敏；
- 不把随机或固定生成的实验数据作为业务指标。

核心治理告警模板位于 `deploy/production/dbguard-prometheus-alerts.yaml`，覆盖构建来源、阻断项、未解决事故、回滚失败、发布后处置率、业务 SLI 退化、目标达成率和成功发布缺少业务证据。模板不会把变更号、事故号或指标名作为 Prometheus label；加载前仍需按企业样本量与告警分级校准阈值。

## 6. HTTPS 与反向代理

生产环境必须通过 HTTPS 暴露浏览器和 Gate API。反向代理需要：

- 保留 Host 和请求 ID；
- 限制请求体大小、连接数与超时；
- 只在受控代理链中设置并信任 X-Forwarded-*；
- 对 /metrics 使用独立网络策略；
- 对 /api/gate/verify 与 /api/gate/consume 限制 CI 出口来源，但不能只靠 IP 代替令牌校验；
- 对 /api/integrations/operations/webhook 使用独立长随机 Token，并限制到运维适配器出口；
- 不记录 Authorization、Cookie 和完整请求体。

## 7. PostgreSQL 与 Redis

PostgreSQL：

- 使用专用数据库和最小权限账号；
- 连接池总量按“实例数 × 每实例最大连接数”核算；
- 关键状态迁移、审计写入和通行证消费放在一致事务中；
- 对组织、应用、状态、过期时间和审计时间建立必要索引；
- 定期执行备份和恢复演练。

Redis：

- 仅保存会话、限流等可重建或有明确 TTL 的数据；
- 开启认证、网络隔离和持久化策略；
- 不保存原始通行证；
- Redis 故障时认证相关接口应失败关闭，不能降级成匿名访问。

## 8. 通行证运维要求

目标 Gate API 上线前必须验证：

- 原始 token 只在签发响应出现一次；
- 数据库只保存强哈希和安全元数据；
- 默认短时有效、一次消费；
- 两个并发消费只有一个成功；
- 过期、吊销、变更内容改变和规则版本失效都能阻断；
- 成功消费、过期状态落盘及吊销进入业务审计；失败请求保留不含原始 Token 的最小访问日志；
- Gate 超时或 5xx 时流水线不执行生产部署。

CI secret 应设为 masked/protected，且通行证消费步骤紧邻生产部署步骤。详细示例见 [CI/CD 接入指南](ci-integration.md)。

## 9. 备份与恢复

至少备份：

- 企业、成员、邀请与应用授权；
- 应用和规则；
- 变更单、制品安全表示、检查批次和发现项；
- 审批、通行证元数据和审计事件。
- 流水线终态、事故关联、回滚执行与业务 SLI 对比事件。

不建议把疑似密钥的原始配置正文长期写入备份。恢复验收应检查：

- 组织与应用隔离仍有效；
- 已消费或已吊销通行证不会恢复为可用；
- 审计时间线与变更状态一致；
- OIDC、Redis 和 metrics secret 由部署平台重新注入；
- CI Gate 对旧令牌继续失败关闭。

## 10. 升级与回滚

升级前：

1. 备份数据库并验证可恢复；
2. 阅读数据迁移与 API 兼容说明；
3. 在测试环境运行 go test、go vet、race、镜像构建和浏览器主流程；
4. 使用一个测试服务验证 Gate 允许和阻断场景；
5. 确认 SQL 仅接受真实 PostgreSQL 影子事务证据，旧模拟 experiment/Agent 输出不会被误当作放行依据。

滚动升级时先观察 readiness 和错误率，再扩大实例范围。应用回滚不能自动回滚已执行的数据迁移；数据库恢复方案必须单独验证。

## 11. 生产上线清单

- DBGUARD_ENABLE_DEMO_ACCOUNTS=false；
- OIDC issuer、client、回调和角色映射已验证；
- HTTPS、Secure Cookie、可信代理边界正确；
- PostgreSQL/Redis 使用独立最小权限凭据；
- metrics 与 Gate API 有网络限制；
- Operations webhook 使用独立凭据，事故/回滚/业务 SLI 至少各完成一次受控接入验证；
- 通行证原子消费和重放测试通过；
- 备份恢复演练通过；
- CI 在所有非 2xx、超时和网络失败时阻断；
- 日志、报告和审计不泄露 secret；
- 当前版本的真实 CI 和系统测试结果已记录。
