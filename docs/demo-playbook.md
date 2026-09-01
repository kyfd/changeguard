# 五分钟演示剧本

目标：让别人在不临场找数据的情况下，走完“提交 → 阻断 → 整改 → 审批 → 通行证 → CI 消费”。

## 启动

本地最小集（文件存储，不需要 PostgreSQL）：

```powershell
$env:CHANGEGUARD_AUTH_MODE = "local"
$env:CHANGEGUARD_ENABLE_DEMO_ACCOUNTS = "true"
$env:CHANGEGUARD_ENABLE_DEMO_DATA = "true"
$env:CHANGEGUARD_PASSPORT_HMAC_SECRET = "changeguard-local-demo-secret-32-bytes-minimum"
go run ./cmd/dbguard
```

完整路径（主库 + Redis + 影子库 + 演示账号）：

```powershell
docker compose -f compose.e2e.yml up --build
```

打开 <http://localhost:8080> 或 e2e 的 <http://localhost:18080>。

演示密码都是 `Demo1234`：

| 账号 | 用来做什么 |
| --- | --- |
| `developer@example.com` | 创建、提交、整改 |
| `reviewer@example.com` | 复核、审批、签发通行证 |
| `owner@example.com` | 高风险审批、规则和企业设置 |

## 剧本

1. 用开发账号登录。演示数据里已经有被阻断的配置和 Kubernetes 变更，先打开「短信供应商配置安全整改」或「文件扫描 Worker 安全基线整改」，说明高风险、`NOT_RUN` 和未签发通行证。
2. 新建一张 SQL 变更，执行语句用 `DELETE FROM orders;`，回滚写一句带 WHERE 的恢复。提交后应命中无条件 DELETE，状态为已阻断。
3. 把语句改成带 WHERE 的删除，补上回滚，重新检查。没有影子库时，SQL 只能静态通过，证据仍是 `NOT_RUN`，不能签发生产通行证——把这一点说清楚。
4. 切到审核账号。对一张已经没有阻断项的配置变更做复核和审批。签发后明文 Token 只出现一次，页面上的通行证绑定 SHA-256。
5. 故意改制品后再跑：

   ```powershell
   go build -o changeguard-gate.exe ./cmd/changeguard-gate
   .\changeguard-gate.exe verify -manifest .changeguard.json -consumer demo-pipeline
   ```

   摘要对不上就会失败。改回去后再 `verify` / `consume`。第二次 `consume` 必须被拒绝。
6. 打开审计时间线或治理全景，指出阻断、审批和消费是同一条证据链，而不是三个互相不知道的页面。

## 不要现场编的三句话

- 没跑过的影子验证不是“通过”，是 `NOT_RUN`。
- 通行证消费成功不等于生产已经健康。
- 审批后改文件，门禁靠 SHA-256 拦住，不靠审核人再看一遍。
