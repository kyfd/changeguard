# ChangeGuard 实测记录

数字来自本仓库测试，不是对外 SLA。换机器请重跑。

## 机器

- OS: Windows
- CPU: Intel Core i7-9750H @ 2.60GHz
- Go: 1.26.3 windows/amd64
- Date: 2026-09-01

## 并发消费通行证

2026-09-05 更新：原有“1 成功／99 拒绝”描述不再适用于同 consumer 重试。下面按当前测试场景区分；这是正确性测试，不是吞吐测试。

```powershell
go test ./internal/store -run 'TestUsePassportConcurrentConsumeCompletesChangeOnce|TestUsePassportDifferentConsumersCompeteForOneConsumption' -count=1
```

| 场景 | 规模 | 结果 |
| --- | --- | --- |
| 相同 consumer 消费同一张通行证 | 100 goroutine | 100 个调用返回同一次消费结果；只写一条消费审计 |
| 不同 consumer 竞争同一张通行证 | 100 goroutine | 1 成功 / 99 重放冲突；只写一条消费审计 |

以上测试使用内存 Store。文件存储靠进程写锁；PostgreSQL 路径使用事务和条件 UPDATE，并仍维护 legacy 状态快照。原 consumer 的成功重试不是再次授权，不能据此重复部署。

## Kubernetes 规则扫描

```powershell
go test ./internal/checker -bench BenchmarkKubernetesRuleScan -benchmem -count=1
```

输入是带 `latest`、privileged、root 和 hostPath 的 Deployment 列表。

| 制品数 | ns/op | 约合 | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| 100 | 2,644,526 | 2.6 ms | 1,296,134 | 7,336 |
| 1,000 | 28,287,022 | 28.3 ms | 15,291,805 | 73,121 |
| 10,000 | 269,396,275 | 269.4 ms | 175,223,458 | 730,435 |

这是平均耗时，不是 HTTP P50/P95。API 延迟和影子库耗时还没有在这台机器上做成可提交的测量，所以不写进 README。

## HTTP 测量方法

本次工具冒烟结果见 [2026-09-05 本机记录](benchmark-results/2026-09-05-smoke/README.md)。仅 19 条合成变更、每轮 5 秒，不作为容量结论。

`cmd/loadtest` 是固定并发、请求结束后再发下一个请求的 GET 负载工具。它现在计时至响应体读取结束，读取失败不能计入成功。`-json` 输出单份机器可读报告，不输出目标 URL 或凭据。

```powershell
$env:CHANGEGUARD_LOADTEST_EMAIL = "developer@example.com"
$env:CHANGEGUARD_LOADTEST_PASSWORD = "Demo1234"
go run ./cmd/loadtest -url http://127.0.0.1:18080/api/dashboard -c 8 -d 30s -json
```

只对自己控制的隔离测试环境执行。先预热，再在相同数据和配置下重复至少三次，分别记录 1、8、32 并发。`success_latency` 只包含完整 2xx 响应，`all_attempt_latency` 包括失败结束时间和超时，不能把后者当作服务成功响应时间。

报告同时保留成功数、HTTP 失败、传输／响应体错误、超时、错误率及各状态码计数。超时是传输错误的子集，不要重复相加。分位数沿用排序后下标 `floor((n-1)*p/100)` 的口径。

GET 测量不代表 Gate 成功写入吞吐。写入压测必须准备独立的已批准变更与通行证，并将不同消费者争抢同一张通行证的测试单列。低延迟如果伴随高拒绝率，不能描述为高性能。
