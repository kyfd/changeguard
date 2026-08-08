# 生产加固状态（liufengxi.top / 152）

日期：2026-08-08
当前结论：Agent 生产闭环已运行；核心候选 v7 已通过隔离迁移/回滚验收，但生产核心仍保持 legacy 版本，灰度为 HOLD。

## 已有证据

- 生产核心软链仍为 `/opt/changeguard/releases/20260807-panorama-v3-20260807-110449`，二进制 SHA-256 为 `58d8b72f40e8ec56e82fe5cb2d12fe4079f9692a6ef715fa5cbde3c05723fe6b`。
- 候选 v7 使用真实 annotated tag/commit 构建，独立重建字节一致，并完成生产数据副本的前向迁移、双启动幂等和 `v7 → legacy → v7` 往返。
- legacy 在回滚副本上改写 41 个工件字段；v7 通过迁移证据侧车全部恢复，未恢复数为 0。
- 候选已在 UID 65534、私有网络、worker=0 的隔离 systemd 单元中运行；生产软链、PID、数据和 Nginx 哈希未改变。
- 生产使用 Redis session，Redis URL 已配置；核心对 Redis 初始化失败会拒绝启动。

## 当前生产缺口

- `changeguard.service` 仍为 `User=root`；除 `NoNewPrivileges` 外缺少主要 systemd 沙箱，`systemd-analyze security` 当前观测为 `9.4 UNSAFE`。
- `/opt/changeguard/data`、主 JSON、当前发布和 `.env` 均归 root；尚未按专用 `changeguard` 账号完成权限迁移与恢复验证。
- `.env` 有两个 `DBGUARD_EXPERIMENT_MODE`，虽然当前两个值相同，重复配置仍会掩盖变更；实际值为 `demo_only`，不能满足真实 PostgreSQL 影子验证的生产签证要求。
- 生产 `.env` 未配置 Operations webhook token/organization 和 metrics token，发布后事故、回滚与业务 SLI 仍未进入线上核心闭环。
- 已部署备份脚本尚未升级，也未从候选前真实快照恢复主 JSON、迁移侧车和 required 标记组合后启动候选。
- 生产安装版侧车感知备份/隔离恢复启动、Redis session parity、无公网候选 upstream、Nginx 小流量切换和原子回切尚未演练。

## 源码中新增的失败关闭基线

- `internal/runtimeconfig` 严格读取规范环境文件，拒绝重复键、非法行、缺失文件和 inherited override。
- `DBGUARD_ENV_PROFILE=production` 强制 HTTPS/Secure Cookie、可信代理、Redis session、真实 PostgreSQL 影子验证、非 demo 数据、长随机通行证/metrics/Operations 凭据、绝对 file/witness 路径和非零 worker。
- `dbguard --check-config` 在打开数据、Redis、HTTP 监听或 worker 前完成静态配置检查，错误信息不输出 secret。
- `deploy/production/changeguard-core-preflight.sh` 以低权限账号验证 release 全量 SHA-256、环境文件只读、数据目录可写以及 witness pair 完整性。
- `deploy/production/changeguard.service` 固定使用 `changeguard:changeguard`，启用 systemd hardening，并串联 preflight 与候选自身的配置检查。
- `deploy/production/changeguard-core.env.example` 提供规范模板；任何 `REPLACE_ME` 占位符会被 production profile 拒绝。

这些资产尚未安装到生产，不能把“源码已具备”表述为“线上已加固”。

## 下一次受控执行顺序

1. 生成 `/etc/changeguard/core.env` 的加密离线备份和规范副本，保留每个键一次；明确从 `demo_only` 切换到隔离 PostgreSQL shadow 的 DSN 与最小权限账号。
2. 在不修改生产软链的候选目录运行 `dbguard --check-config`，记录不含 secret 的结果。
3. 创建专用 `changeguard` 系统账号；release 与 env 保持 root 所有、运行账号只读，数据目录及 file/witness/marker 成组迁移给运行账号。
4. 用 `changeguard-restore.sh verify` 和 `stage` 校验最新快照，在隔离目录启动恢复数据副本；不得直接覆盖生产数据目录。
5. 安装 preflight 与 hardened unit，在独立端口、无公网 upstream 下启动候选，并验证 Redis session/限流及故障失败关闭。
6. 安装侧车感知备份脚本，生成候选前快照，在隔离恢复目录启动候选并核对业务状态与完整性摘要。
7. 建立 Nginx 0% 内部 upstream，完成登录、Gate、metrics、Operations、worker、回滚和观察窗口验收后，再决定是否进入 1%～5% 灰度。

在 1～7 完成前不替换生产核心；任何门禁失败都保留 legacy upstream 和原数据不变。
