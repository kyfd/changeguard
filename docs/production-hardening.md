# 生产加固状态（liufengxi.top / 152）

日期：2026-08-08
当前结论：Agent 生产闭环已运行；核心候选链已完成可复现构建、file 数据迁移/legacy 回滚、带 witness 备份恢复、原子安装和真实 Redis 会话验收。生产核心仍保持 legacy 版本，灰度为 HOLD。

## 已有证据

- 生产核心软链仍为 `/opt/changeguard/releases/20260807-panorama-v3-20260807-110449`，PID `2191733`，二进制 SHA-256 为 `58d8b72f40e8ec56e82fe5cb2d12fe4079f9692a6ef715fa5cbde3c05723fe6b`。
- v8 完成生产数据副本前向迁移、双启动和 `v8 → legacy → v8` 往返；legacy 改写的 41 个工件字段全部收敛，未恢复数为 0。
- v9 完成真实生产快照恢复、带 witness 的备份/恢复和双启动往返；v10 完成不可信权限归档的原子安装与“已安装发布从真实快照恢复启动”。
- v11 开发候选使用带密码的隔离真实 Redis，完成 loopback 监听、企业注册、会话跨核心重启、独立 namespace、登录限流和 Redis 故障注入：readiness/session/login 为 503，liveness 为 200，Redis 不可达时新核心退出码为 1。
- 已以声明式 sysusers 创建 `changeguard:changeguard`（UID/GID 990、`/usr/sbin/nologin`），并创建 `/etc/changeguard` 与 `/usr/local/libexec/changeguard`；该步骤没有 chown 生产数据、创建 canonical env、修改 unit、切换 release 或重启服务。
- 上述身份准备后，生产核心 PID、二进制、数据、Nginx 和现网 unit 哈希保持不变。

## 当前生产缺口

- `changeguard.service` 仍为 `User=root`，现网 `systemd-analyze security` 仍为 `9.4 UNSAFE`；源码 hardened unit 的离线评分为 `3.0 OK`，但尚未安装。
- `/opt/changeguard/data/dbguard.json` 仍为 `root:root:0600`；正式切换前需把 data/witness/marker 成组迁移给 `changeguard` 并完成恢复验证，不能只改单个文件。
- 正式 `/etc/changeguard/core.env` 仍不存在。legacy `.env` 有重复 `DBGUARD_EXPERIMENT_MODE`、仍为 `demo_only`，且缺少真实 Redis ACL、独立 namespace、PostgreSQL shadow、metrics、Operations 和组织配置。
- 主机已有 `changeguard-redis` Docker 容器并绑定 `127.0.0.1:6379`，但当前无认证、rootfs 可写，仅配置 RDB 快照且只在 db0 有少量 TTL key；它不能直接作为已验收的核心 production session 依赖。
- v9 备份/恢复、v10 安装器和 v11 host prepare 尚未全部安装到正式运维路径、定时任务、审批和周期恢复制度。
- 候选 Nginx 0% upstream、原子切换/回切、连接排空和观察窗口尚未演练；PostgreSQL 多实例迁移与 PITR 仍未完成。

## v11 失败关闭基线

- production 必须显式设置 `DBGUARD_LISTEN_ADDRESS` 为 loopback IP:port；wildcard 地址和同时存在的 `PORT` 被拒绝，避免核心绕过反向代理直接暴露。
- Redis URL 必须包含 ACL 用户和至少 16 字节非占位密码；非 loopback 明文 `redis://` 被拒绝，远程连接必须使用 `rediss://`。
- `DBGUARD_REDIS_PREFIX` 在 production 必填，必须是独立且以冒号结尾的 namespace；会话、OIDC flow 和登录限流共享该隔离边界。
- Redis key 缺失与 Redis 后端故障被区分：前者保持正常未登录语义，后者对受保护请求返回 503，不再误报“会话失效”。
- 启动时 Redis 不可达继续失败关闭；运行期故障使 readiness 失败，同时保持 liveness，便于编排系统停止导流而不反复误杀进程。
- `changeguard-core-host-prepare.sh` 只准备声明式身份和支持目录，显式承诺不迁移数据、不修改服务，降低生产账号切换的爆炸半径。

## 下一次受控执行顺序

1. 生成 root 所有、`changeguard` 组只读的 `/etc/changeguard/core.env`，保留每个键一次并注入真实 secret；运行候选 `--check-config`，不得连接监听器或修改数据。
2. 为核心准备带 ACL、独立 database/namespace、明确 AOF/RDB 策略和监控的 Redis；不得复用当前无认证容器作为 production 通过证据。
3. 用 v10 安装器安装 v11 不可变 release；安装正式 preflight、backup/restore 和 hardened unit 资产，但仍不切换 `current`。
4. 从最新正式快照 stage 数据，成组迁移 data/witness/marker 所有权，在 `changeguard` 身份和 0% upstream 下启动候选。
5. 复跑真实 Redis 会话、限流、重启、故障 503 和启动失败关闭，并验证 Redis key 只落入批准的 database/namespace。
6. 接入隔离 PostgreSQL shadow，完成真实事务、回滚 SQL、超时/锁等待和失败关闭验收。
7. 建立 Nginx 0% 内部 upstream，完成 login、Gate、metrics、Operations、worker、原子回切和观察窗口，再决定是否进入 1%～5% 灰度。
8. PostgreSQL 多实例路线完成向后兼容迁移、并发锁、备份恢复和 PITR 后，才允许扩大实例数。

在 1～8 完成前不替换生产核心；任何 provenance、配置、身份、Redis、shadow DB、readiness、数据或回滚门失败都保留 legacy upstream 和原数据不变。
