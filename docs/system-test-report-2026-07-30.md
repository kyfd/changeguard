# ChangeGuard 系统验证记录

> 历史记录：本文件保留 2026-07-30 阶段性结果，最终交付状态以 [2026-07-31 最终测试记录](final-test-report-2026-07-31.md) 为准。

- 记录文件名保留历史日期：system-test-report-2026-07-30.md
- 本次整理日期：2026-07-31
- 文档性质：执行记录与待验证清单，不是“项目已通过全部测试”的证明

## 1. 当前结论

本轮文档整理完成了静态核对，但没有把未执行命令写成“测试通过”。

已完成的静态核对：

- 服务入口使用 Go 1.23，并监听默认 8080 端口；
- 进程处理 Ctrl+C/SIGTERM，并调用 HTTP Server.Shutdown；
- 演示账号只有在 DBGUARD_ENABLE_DEMO_ACCOUNTS=true 时创建；
- 三个演示账号 ID、登录邮箱与测试中的演示密码 Demo1234 已核对；
- 当前 HTTP 路由、POST 退出登录要求和前端脚本文件名已核对；
- README、产品、架构、流程、API 和 CI 接入文档已统一为 ChangeGuard；
- CI 接入文档明确把通行证接口标记为目标 v1 契约。

本次文档子任务未实际执行以下命令，因此状态均为**待执行**：

| 验证项 | 命令/方式 | 当前状态 |
| --- | --- | --- |
| Go 单元与集成测试 | go test ./... | 待执行 |
| Go 静态检查 | go vet ./... | 待执行 |
| Linux race detector | go test -race ./... | 待 CI/Linux 执行 |
| 前端 JavaScript 语法 | node --check internal/httpapi/web/api-adapter.js；node --check internal/httpapi/web/app.js | 待执行 |
| 镜像构建 | docker build -t changeguard:test . | 待执行 |
| Docker Compose 启动 | docker compose up --build | 待执行 |
| 浏览器主流程 | 创建、检查、整改、审批、审计 | 待执行 |
| CI 门禁端到端 | GitLab/Jenkins 消费通行证并阻断不匹配制品 | 待 Gate API 实现后执行 |

最终结果应以本地命令输出或 GitHub Actions 运行记录为准。

## 2. 必测功能矩阵

### 2.1 三类确定性检查

- DATABASE：安全 SQL、危险 DDL、无条件 UPDATE/DELETE、缺少回滚；
- CONFIG：正常引用、疑似明文 secret、debug/trace 开关；
- KUBERNETES：固定镜像、latest、privileged、资源约束；
- 同一输入和同一规则版本重复检查结果一致；
- 不生成未真实测量的耗时、锁等待、扫描行数或 P99；
- 无命中规则时不创建“开放风险项”假阳性。

### 2.2 状态与权限

- 草稿可以更新，提交后按规则进入失败或待审批；
- 阻断项存在时无法批准；
- 提交人不能自审；
- 没有应用审核权限的成员不能批准；
- 停用成员无法继续操作；
- 修改制品或目标环境会使旧审批和旧通行证失效；
- 跨企业、跨应用访问返回 403 或 404，且不泄露记录是否存在。

### 2.3 通行证

- 只有已批准变更可以签发；
- 原始 token 只返回一次，持久化与日志只保留哈希/安全标识；
- 正确环境和完全匹配摘要可以成功消费一次；
- 过期、吊销、错误 token、摘要不匹配、重复消费均失败；
- 两个并发请求消费同一 token 只有一个成功；
- Gate 服务异常时 CI 默认阻断；
- 失败与成功消费均有不含原始 token 的审计事件。

### 2.4 认证与安全

- 本地登录限流、错误密码、停用成员；
- OIDC state、nonce、PKCE、issuer、audience；
- POST /auth/logout 的 CSRF；
- Cookie 的 HttpOnly、SameSite 和生产 Secure 配置；
- JSON 体积限制、异常 JSON 和超长制品；
- 日志、报告、审计和错误响应不泄露 secret、Cookie 或 token；
- DBGUARD_ENABLE_DEMO_ACCOUNTS=false 时演示凭据不可用。

### 2.5 存储与恢复

- 文件存储首次启动、并发保存、损坏文件报错；
- PostgreSQL 事务、约束和版本冲突；
- Redis 会话创建、过期和退出；
- 备份恢复后企业、授权、变更、发现项、通行证元数据和审计一致；
- 多实例下通行证消费仍具原子性。

### 2.6 前端与可用性

- 登录、创建变更、三类制品编辑、发现项定位、审批和审计主流程；
- 前端 API 适配层不使用不存在的接口或字段；
- 网络失败、401/403/409/422 有明确提示；
- 原始通行证只在签发成功后展示一次，并提示立即保存到 CI secret；
- 1366×768、1920×1080 和窄屏布局无关键遮挡；
- 键盘导航、焦点状态和基本对比度可用。

## 3. 建议执行顺序

1. go test ./...
2. go vet ./...
3. 在 Linux CI 执行 go test -race ./...
4. node --check 两个前端脚本
5. docker build
6. Docker Compose 冒烟
7. 浏览器主流程
8. Gate API 与 GitLab/Jenkins 端到端
9. 备份恢复和并发消费专项

任何一步失败都应记录：命令、环境、失败输出、根因、修复提交和复测结果。不要只写“已修复”而没有可复现证据。

## 4. 发布判定

只有满足以下条件才可把项目描述为“已完成测试”：

- CI 的 test、vet、race、JavaScript 语法和镜像构建全部通过；
- 三类规则有正反例自动化测试；
- 通行证安全与并发用例全部通过；
- 至少一次真实浏览器主流程通过；
- 至少一个 CI 平台完成允许与阻断两种端到端场景；
- 文档中的接口、状态和页面与当前代码一致；
- 没有高危 secret 泄露、越权或放行绕过问题。

在这些证据齐全前，简历和面试应表述为“已实现/已设计哪些部分，并完成了哪些具体测试”，不要笼统声称生产级或全部通过。
