# ChangeGuard 最终测试与缺陷排查记录

- 日期：2026-07-31
- 项目目录：outputs/DBGuard
- 结论：已完成面向小型企业的可运行 MVP 代码收敛、浅色企业工作台、生产变更治理全景、全站字体可读性改造、前端验证和安全/一致性扫描；`go test ./...` 与 `go vet ./...` 已通过，真实 HTTP 登录、会话及多尺寸浏览器布局也已验证。Docker、真实 PostgreSQL 影子库和完整业务链路端到端仍未执行，本文分别记录。

## 1. 本次完成的风险修复

- 产品从单一数据库检查扩展为 SQL、配置、Kubernetes 三类生产变更门禁。
- SQL 只有真实 PostgreSQL 影子事务中 SQL 与回滚脚本均执行成功，才可进入审批；DEMO_ONLY、NOT_RUN 不可签证。
- 配置与 Kubernetes 使用确定性规则，不生成伪造 P99、锁等待或随机通过数据。
- 服务端按原始制品字节计算 SHA-256，保存与页面展示可脱敏，CI 从实际文件重新计算聚合摘要。
- 通行证绑定企业、变更、制品摘要、环境、规则版本、实际审批人和有效期；原始 Token 只返回一次，持久化仅保存摘要。
- 禁止提交人自审；只有实际审批人可以签发或撤销通行证。
- 通行证在锁内复核当前规则版本，原子消费并更新治理状态，阻断规则变化与并发重放。
- 最后一个阻断项复核通过后同步更新 CheckRun 为 PASSED、Blocking=0，并按 SQL/非 SQL 推进状态。
- 删除人工“完成发布”入口；只有 CI consume 能把治理变更单设为 COMPLETED。
- Gate 仅接受 Authorization: Bearer，不接受 JSON 请求体 Token；CI 示例不在命令参数或日志中回显 Token。
- CLI 拒绝绝对路径、目录穿越、符号链接逃逸、非普通文件及超限文件。
- 前端修复过期/撤销/已消费通行证仍阻止重新签发、非实际审批人可见签发按钮等问题，并增加移动端卡片布局。
- 修复变更表“待评估”竖排及风控规则代码、名称、说明互相覆盖的问题；列表按实际容器宽度切换完整卡片，长规则代码保留完整提示。
- 日常页面学习授权企业系统的浅灰画布、白色侧栏/卡片和蓝色主色，但未复制 Logo、IP、专有图标或业务数据。
- 新增“生产变更治理全景”，将应用、SQL、Kubernetes、配置、CI/CD、规则、证据、审批和通行证数据汇总为拓扑与风险态势。
- 全站正文、表格与详情值提升为 14px，字段标签为 13px，徽标/代码最小 12px；修复详情返回按钮 SVG 尺寸、全景面板裁切和隐藏横向滚动。
- 修复演示邮箱、6 位旧密码与前端 8 位约束不一致的问题；演示数据和持久化凭据可自动迁移到 `Demo1234`。
- Kubernetes PodSpec 片段不再静默放行；即使缺少 `kind`，仍会检查 latest、privileged 和资源约束。

## 2. 已实际执行的检查

| 检查 | 实际结果 |
| --- | --- |
| 项目文件清点 | 108 个文件；47 个 Go 文件；20 个测试文件，合计 100 个 Test* 用例 |
| Go 自动化测试 | `go test ./...` 全部通过 |
| Go 静态检查 | `go vet ./...` 通过，无输出 |
| 真实登录流程 | `developer@example.com / Demo1234` 登录 200，会话 200；错误邮箱 `usr_developer@example.com` 返回 401 |
| JavaScript 模块解析 | api-adapter.js、app.js 均通过 vm.SourceTextModule 解析 |
| 前端证据映射运行用例 | CONFIG=REAL；Kubernetes=REAL；SQL 无真实影子库=NOT_RUN；SQL 真实 PostgreSQL 且回滚成功=REAL；DEMO_ONLY 不放行 |
| 通行证前端状态用例 | 未过期 ACTIVE 可用；客户端识别过期后状态为 EXPIRED 且不可用；artifact_sha256 正确映射为摘要 |
| HTML/CSS 静态检查 | index.html 正确引用三个资源；CSS 花括号平衡；文本风险标签保持单行；规则列表使用容器查询切换卡片 |
| 浏览器响应式矩阵 | Edge/Chromium 实测 1440、1024、768、640（200% 缩放等效）、390、320px；全景横向溢出/节点重叠/面板裁切均为 0；日常页横向溢出、徽标重叠、规则越界均为 0；可见文字不低于 12px；控制台无错误 |
| 前端安全扫描 | 未发现手工 complete、请求体 Token 示例、-token 命令示例或页面摘要直接冒充实际文件摘要；独立证据复评结论为 PASS |
| Go 编译与解析 | 47 个 Go 文件已由 `go test ./...` 与 `go vet ./...` 实际解析和检查 |
| Go 导入近似检查 | 未发现疑似未使用导入；IssuePassport 的实现和 5 处调用均为 3 个参数 |
| Go 正则兼容扫描 | 未发现 RE2 不支持的非捕获组、前后向断言或反向引用 |
| 敏感文件/文本扫描 | 未发现 .env、私钥文件、AWS Access Key、OpenAI 风格密钥或硬编码长 Bearer Token |
| 配置一致性 | .env.example 默认 demo_only；Compose 默认 postgres 影子验证并强制提供至少 32 字节通行证密钥 |

## 3. 已实际执行的 Go 测试覆盖

新增或扩展的重点覆盖包括：

- 配置明文凭据、生产 debug/trace、安全 Secret 引用；
- Kubernetes Service 误报、多容器、latest、特权、资源、探针与副本；
- 原始字节摘要、Secret/ConfigMap 脱敏边界、换行变化；
- HMAC 弱密钥、篡改、Token 摘要；
- SQL 事务逃逸、psql 元命令、demo_only 失败关闭；
- 整改分派、独立复核、CheckRun 闭环和非 SQL 审批签证；
- 通行证并发仅一次消费、保存失败内存回滚、规则版本变化拒绝、过期落盘，以及审批后编辑撤销旧通行证；
- Gate 严格 Bearer、请求体 Token 拒绝、前端无人工完成入口；
- CLI 目录穿越、绝对路径、超限文件和实际文件重新摘要。

完整测试名称可在各 *_test.go 文件中查看。

本节测试已通过 `go test ./...` 实际执行，不只是静态清点。

## 4. 仍未执行及原因

尝试执行：

~~~powershell
& "C:\Program Files\Go\bin\go.exe" test ./...
~~~

默认命令工具仍会先尝试调用以下无效 PowerShell 启动器：

~~~text
C:\Users\Administrator\.grok\bin\powershell.exe
~~~

该文件实测只有 64 字节，文件头不是 Windows PE 的 MZ 标记；工具会返回 Windows 错误 193。此次已绕过该启动器，直接调用系统 Go 1.26.3 完成测试、构建与启动。因此当前仍未执行的是：

- go test -race ./...：未执行；
- Docker build / Compose 集成：未执行；
- 真实 PostgreSQL 影子库集成：未执行；
- 完整的“创建 → 整改 → 审批 → 签发 → CI 消费”浏览器业务链路：未执行；登录、变更列表、规则列表及多尺寸视觉矩阵已执行。

PowerShell 启动器故障仍应修复，但已不再阻止本次 Go 测试与本地服务验收。

## 5. 可重复执行的验收命令

~~~powershell
go test ./...
go vet ./...
go test -race ./...
node --check internal/httpapi/web/api-adapter.js
node --check internal/httpapi/web/app.js
docker build -t changeguard:local .
docker compose up --build
~~~

随后至少手工验证：

1. 配置安全/危险各一单，确认规则结果和审批流。
2. Kubernetes 安全 Deployment 与 privileged/latest 反例。
3. SQL 在真实隔离 PostgreSQL 中执行与回滚，确认失败关闭。
4. 开发者不能自审，非实际审批人不能签发。
5. CI 对真实文件 digest、verify、consume 成功一次；再次消费返回冲突。
6. 审批后修改文件、改变换行、改变环境、更新规则、Token 过期或撤销均阻断。
7. 桌面端与 390px 宽度完成创建、整改、审批、签发和审计查看。

## 6. 当前剩余风险

- 尚未执行 race 检测，高并发路径仍需补充竞争检查。
- PostgreSQL 影子验证需在真实数据库上确认权限、超时、DDL 行为和回滚语义。
- 多实例场景需补充 PostgreSQL 后端的并发/故障注入验证后再宣称高可用。
- Kubernetes/配置规则属于轻量确定性检查，不替代完整 YAML schema、OPA、集群准入或运行时安全。
- 企业总览、变更列表、规则列表、变更详情和治理全景已完成真实浏览器视觉验收；完整创建、审批、签发与 CI 消费仍需业务链路级浏览器端到端测试。

## 7. 关于 PowerShell 停止方式

正常运行 go run 或本地服务时，在对应终端按 Ctrl+C 即可优雅停止当前子进程。不要结束系统中所有 powershell.exe；本次问题也不是有一个 PowerShell 进程“停不掉”，而是工具指向了一个无效的 powershell.exe 启动文件。
