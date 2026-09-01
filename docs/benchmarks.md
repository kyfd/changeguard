# ChangeGuard 实测记录

数字来自本仓库测试，不是对外 SLA。换机器请重跑。

## 机器

- OS: Windows
- CPU: Intel Core i7-9750H @ 2.60GHz
- Go: 1.26.3 windows/amd64
- Date: 2026-09-01

## 并发消费通行证

```powershell
go test ./internal/store -run TestUsePassportConcurrentConsumeCompletesChangeOnce -count=1
```

| 场景 | 规模 | 结果 |
| --- | --- | --- |
| 同时消费同一张 ACTIVE 通行证 | 100 goroutine | 1 成功 / 99 拒绝；变更单原子变为 COMPLETED |

文件存储靠写锁；PostgreSQL 路径走条件 UPDATE。两种后端都是 fail-closed：第二次消费得到 inactive/consumed，而不是再发一次授权。

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
