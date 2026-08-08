# 生产加固清单（liufengxi.top / 152）

## 已落地

- `DBGUARD_ENABLE_DEMO_ACCOUNTS=false`
- `DBGUARD_ENABLE_DEMO_DATA=false`
- `DBGUARD_SESSION_MODE=redis` + `DBGUARD_REDIS_URL=redis://127.0.0.1:6379/0`
- Docker 容器 `changeguard-redis`（restart always）
- 登录页移除演示账号展示
- 清理 DEMO_ONLY / chg_demo 类变更种子
- 组织展示名调整为「星澜科技」
- 多角色员工测试账号（统一初始密码需尽快轮换）

## 操作者必须尽快完成

1. **轮换服务器 root 密码**（若曾在聊天中泄露）
   `passwd`
2. **配置 SSH 密钥登录**，确认可用后再考虑 `PasswordAuthentication no`
3. **强制所有测试账号修改密码**（当前统一 `Demo1234` 仅限内测）
4. **备份** `/opt/changeguard/data/dbguard.json` 与 `.env` 到安全存储
5. 生产流水线使用 masked 变量存放通行证 token

## 可选进阶

- `DBGUARD_STORE_MODE=postgres` + `DBGUARD_PRIMARY_DSN`（业务状态高可用）
- `DBGUARD_EXPERIMENT_MODE=postgres` + `DBGUARD_SHADOW_DSN`（SQL 真实影子验证 → REAL 证据）
- 关闭公网管理面或加 IP 允许列表 / mTLS
