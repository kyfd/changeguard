# 测试

本轮实际结果与未覆盖项见 [2026-09-05 验证记录](verification-2026-09-05.md)。

普通测试不需要数据库；集成测试和 SQL 浏览器流程需要隔离的 PostgreSQL、Redis。缺少连接参数时部分测试会跳过，跳过不能当成外部依赖验证通过。

## 单元与服务测试

```powershell
go test ./...
go vet ./...
npm.cmd test
```

Linux CI 另运行 `go test -race ./...`。Windows 上 `npm.ps1` 被执行策略阻止时使用 `npm.cmd`，无需修改系统执行策略。

两个并发场景要分开看：

```powershell
go test ./internal/store -run 'TestUsePassportConcurrentConsumeCompletesChangeOnce|TestUsePassportDifferentConsumersCompeteForOneConsumption' -count=1 -v
```

- 同一 consumer：100 个调用可返回同一次消费结果；消费审计只有一条。
- 不同 consumer：100 个调用竞争，只有一个获取首次消费，其余收到重放冲突。

这些测试使用内存 Store，证明的是状态与并发约束，不是 HTTP 吞吐或 PostgreSQL 容量。

## 浏览器与 CLI

需要 Go、Node.js 和 Docker。先启动专用环境：

```powershell
docker compose -p changeguard-demo -f compose.e2e.yml up --build --wait
npm.cmd ci
npx.cmd playwright install chromium
npm.cmd run test:e2e
```

主实例地址为 `http://127.0.0.1:18080`，第二实例为 `http://127.0.0.1:18081`，共享专用 PostgreSQL 和 Redis。Compose 只绑定本机地址。测试会创建变更、运行影子 SQL、审批和消费测试通行证，不得指向线上服务。自定义端口时同时设置 `CHANGEGUARD_E2E_BASE_URL` 和 `CHANGEGUARD_E2E_REPLICA_URL`。

`config-sql-golden.spec.js` 依次检查：

1. 开发账号登录并创建配置＋SQL 变更。
2. 规则检查及真实 PostgreSQL 迁移／回滚验证。
3. 禁止提交人自审；另一浏览器会话完成审批和签发。
4. 编译 Gate CLI，按实际文件计算摘要；篡改或更换环境时拒绝。
5. 通过本机代理在主实例消费，等后端返回完整成功响应后断开下游连接；确认 CLI 报错，再用同 consumer 直连第二实例重试，核对首次消费快照；不同 consumer 仍被拒绝。
6. 核对最终状态、首次消费时间和审计数量。

Token 只保留在测试进程及 CLI 子进程环境中。该流程关闭自动 trace、视频和失败截图，避免保存签发响应；仅在消费后、Token 弹窗清空后保存结果截图。失败时查看控制台和服务日志，不要打印凭据排错。

## PostgreSQL / Redis 集成

参照 `.github/workflows/ci.yml` 设置 `DBGUARD_TEST_POSTGRES_DSN` 和 `DBGUARD_REDIS_TEST_URL`，然后运行：

```powershell
go test ./internal/store -run '^TestPostgresNormalizedMultiInstance$' -count=1 -v
go test ./internal/auth -run '^TestRedisSessionRepositoryIntegration$' -count=1 -v
```

**PG 测试会删除并重建指定数据库中的项目表。只使用专用测试数据库，不可使用业务主库或线上 DSN。**

两个 Store 共享 PostgreSQL 的集成测试，不等同于两个独立 HTTP 进程的故障恢复。报告结果时注明测试层次、依赖版本、提交和是否发生跳过。

浏览器流程中的响应丢失实验使用两个后端容器和共享 PostgreSQL，加一个只丢响应的本机代理。它覆盖提交后客户端没有收到结果、向另一实例重试的窗口，不覆盖数据库提交失败、后端进程重启或部署重复执行。

空库初始化时两个实例不能同时执行当前迁移，可能发生 PostgreSQL 系统目录唯一键冲突。Compose 用 `service_healthy` 依赖先完成主实例初始化，再启动第二实例；运行阶段两实例同时服务。这不是并发迁移验证。

## 记录结果

性能测量见 [benchmarks.md](benchmarks.md)。每次记录至少包括版本、命令、环境、测试结果和未测内容。不要从规则扫描均值推导 API P95，也不要从 Gate 消费成功推导部署成功。
