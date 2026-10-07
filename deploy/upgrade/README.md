# ChangeGuard 升级系统

基于 annotated tag、GitHub Release 和带校验清单的核心升级包。**发布包生成、脚本回归、生产部署是三个不同的验收项。**

## 一、发布身份

| 项目 | 3.1.3 示例 |
| --- | --- |
| Git 标签 | `v3.1.3`（必须是 `main` 上的 annotated tag） |
| 安装包 / 根目录 | `changeguard-3-1-3.tar.gz` / `changeguard-3-1-3/` |
| manifest / verification version | `3-1-3` |
| manifest / verification tag | `v3.1.3` |
| CLI `--version` | `3.1.3` 或 `3-1-3`，均安装到 `changeguard-3-1-3` |

不要移动已经发布的 `v3.1.2` 标签或替换同名资产。3.1.2 标签后的修复通过新的 3.1.3 发布。

合并前运行 CI；确认目标提交、变更日志、部署影响与回滚路径后才创建并推送标签：

```bash
git tag -a v3.1.3 <已验收的main提交SHA> -m "ChangeGuard 3.1.3"
git push origin v3.1.3
```

Release 工作流验证标签身份、Go 测试/vet/race、前端单测与语法、Agent pytest、隔离升级回归，构建核心包和 CycloneDX SBOM。发布前使用真实生产安装器将**本次生成的包**安装到 runner 的隔离目录；失败即不发布。这个步骤不切换服务，也不证明目标生产环境健康。

## 二、升级前提

- 已有受信任、可回滚的核心版本，`current` 是指向 `release-root` 内直属 `changeguard-*` 目录的软链。首次部署使用安装流程，不使用升级命令。
- Linux + root，Bash、GNU coreutils/findutils、Python 3、curl、systemd、`flock` 可用；release-root 必须由 root 所有，且组和其他用户不可写。
- 已确认目标主机、上线窗口、当前核心与 Python Agent 的提交/版本、数据备份及恢复方案。
- 暂停会产生不一致快照的写入或停止对应服务；备份 Go 数据/迁移见证、数据库、运行配置和密钥；单独备份 Python Agent 任务 JSON、检查点 SQLite（包含一致性要求）、知识和评测 JSON。
- `deploy/production/changeguard-backup.sh` / `changeguard-restore.sh` 服务于其明确配置的部署布局。不要假设它们自动覆盖 Python Agent 的所有自定义存储路径；逐项核对清单，并在独立目录验证恢复，不能直接覆盖生产新数据。

## 三、更新运维脚本

**核心 tar.gz 不携带运维脚本，也不包含 Python Agent。** 从经过核验的同一发布提交获取源码，保留仓库相对目录结构。不要仅下载一个 `changeguard-upgrade.sh` 到孤立目录：它需要同级 `../production/changeguard-core-install.sh`。

启用了 systemd watcher 的部署需在维护窗口内从该源码更新三个脚本：

```bash
# 在已核对版本的源码根目录执行；先确认没有升级正在进行。
sudo systemctl stop changeguard-upgrade-watcher
sudo install -m 0755 deploy/production/changeguard-core-install.sh \
  /usr/local/libexec/changeguard/changeguard-core-install.sh
sudo install -m 0755 deploy/upgrade/changeguard-upgrade-preflight.sh \
  /usr/local/libexec/changeguard/changeguard-upgrade-preflight.sh
sudo install -m 0755 deploy/upgrade/changeguard-upgrade-watcher.sh \
  /usr/local/libexec/changeguard/changeguard-upgrade-watcher.sh
sudo systemctl start changeguard-upgrade-watcher
```

**不要只更新核心二进制而无视预检脚本。** watcher 默认从安装器同目录加载
`changeguard-upgrade-preflight.sh`；该文件缺失时 watcher 拒绝启动（失败关闭），
必须显式设置 `CHANGEGUARD_SKIP_PREFLIGHT=1` 才跳过，而那等于放弃身份与回滚核对。

CLI 与 watcher 使用同一 `release-root/.upgrade.lock`，避免同时切换/清理版本。**旧版 watcher 不使用该锁**，因此必须先更新，或在 CLI 升级期间保持它停止。

## 四、服务器升级

从 Release 取独立核验的 64 位 SHA256，不能把刚下载包自行计算的摘要当作可信预期值。

```bash
sudo bash deploy/upgrade/changeguard-upgrade.sh \
  --version 3.1.3 \
  --archive-url https://github.com/kyfd/changeguard/releases/download/v3.1.3/changeguard-3-1-3.tar.gz \
  --expected-sha256 <可信Release提供的64位哈希>
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--version` | 必填 | `X.Y.Z` 或 `X-Y-Z`；不再接受旧日期式示例 |
| `--archive-url` | 必填 | HTTPS 地址，重定向也必须 HTTPS |
| `--expected-sha256` | 必填 | 64 位小写十六进制 SHA256 |
| `--release-root` | `/opt/changeguard/releases` | 已存在、由 root 所有的版本目录 |
| `--current-link` | `/opt/changeguard/current` | 已存在且有效的当前版本软链 |
| `--service` | `changeguard` | systemd 服务名 |
| `--health-url` | `http://127.0.0.1:8080/health/ready` | 必须指向本次重启的实例，不能用负载均衡后其他实例的健康冒充 |
| `--health-timeout` | `60` | 每次启动/回滚探活预算，1..9999 秒；单次请求最长 2 秒，可略超预算 |
| `--keep-archives` | `3` | 成功后保留的版本目录总数，2..999；始终保留当前与上一版本 |
| `--agent-health-url` | 环境变量 `CHANGEGUARD_AGENT_HEALTH_URL` | 声明后预检会核对核心与 Agent 的运行身份是否配套；未声明则记 `not_run`（incomplete） |
| `--agent-token-env` | `AGENT_UPSTREAM_TOKEN` | 存放 Agent 共享密钥的**环境变量名**；token 经环境传入，不出现在 argv、不回显 |
| `--core-only` | 关闭 | 显式声明本部署不含 Agent：Agent/配套项记 `not_applicable`，不产生 incomplete；与 Agent 地址互斥 |
| `--skip-preflight` | 关闭 | 跳过预检；仅在已用其他凭证独立核对身份与回滚能力时使用 |

### 升级前预检

下载并通过 SHA256 校验后、**安装与切换之前**，CLI 与 watcher 都会运行
`changeguard-upgrade-preflight.sh`。它只读：不写版本目录、不动软链、不重启服务、
不改业务数据。它核对安装器看不到的东西：

| 检查 | 通过条件 | 失败时 |
| `core_identity` | 响应为对象、`status=ok`、版本为合法 `X.Y.Z` 且非 `unknown`/`dev`、commit 与 source_sha256 完整 | 中止，不切换 |
| `agent_identity` | 声明 Agent 地址且凭据可用时，同样严格核对 Agent 身份与 `status=ok` | 中止，不切换；未声明则记 `not_run` |
| `identity_pairing` | 核心与 Agent 的**版本 + 提交 + 源码摘要**全部一致 | 中止，不切换 |
| `rollback_capability` | `current` 是指向 release-root 直属目录的有效软链，且 `dbguard` 是普通二进制文件 | 中止，不切换 |
| `archive_identity` | 包摘要一致；有界只读解析包内唯一 `release-manifest.json`，核对 schema / 版本 / 标签 / 提交 / 摘要 / 根目录 | 中止，不切换 |
| `target_version` | `--target-version` 与包内 manifest 版本一致 | 中止，不切换 |

退出码：`0` 全部通过；`1` 有检查失败；`3` 有检查**未执行**（例如未声明 Agent 地址）；
`64` 用法错误。**CLI 与 watcher 对 `3` 一律拒绝安装与切换**：未执行的检查不是通过；
显式 `--core-only`（watcher 为 `CHANGEGUARD_CORE_ONLY=1`）声明后 Agent 项记
`not_applicable`，不产生 incomplete。

预检可独立运行，用于上线前核对当前环境（不改动任何版本）：

```bash
sudo bash deploy/upgrade/changeguard-upgrade-preflight.sh \
  --core-health-url http://127.0.0.1:8080/health/ready \
  --agent-health-url http://127.0.0.1:8091/api/agent/healthz \
  --release-root /opt/changeguard/releases --current-link /opt/changeguard/current
```

Agent 侧需注入 `AGENT_BUILD_VERSION` / `AGENT_BUILD_COMMIT` /
`AGENT_BUILD_SOURCE_SHA256` / `AGENT_BUILD_BUILT_AT` 才有可识别的身份；未注入时
预检按"核对不了"失败关闭，而不是默认通过。这四项**只表示构建身份**，不代表镜像
已通过评测或验收。
下载放在 release-root 的私有临时目录内，退出时清理，不再复用可预测的 `/tmp` 缓存。
安装器校验压缩包摘要、成员路径/类型、文件校验清单、manifest 与 verification 身份，以及目录名/版本/tag 一致性；通过后才原子替换软链。

核心与 Python Agent 应使用同一发布提交部署，并验证共享凭据与回调地址一致。脚本**只控制核心服务**，不协调 Python Agent 回滚，也不执行数据迁移。

### 失败语义

- 预检失败（无法读取可识别身份、无有效回滚版本、包身份不符）：**不安装、不切换**，当前版本照常运行。CLI 输出 `upgrade_error=preflight failed`；watcher 写 `state=failed` 并记录历史。
- 安装或前置校验失败：不切换当前版本。
- 新核心重启失败或探活失败：恢复原软链，再重启并探测旧版本。
- CLI 输出 `upgrade_status=rolled_back` 表示旧版本已恢复健康，但升级命令仍返回非零；输出 `rollback_failed` 表示需要人工处理。升级成功才返回零和 `upgrade_status=ok`。
- watcher 只有在旧版本恢复健康后才写 `state=rollback`；回滚重启或探活失败写 `state=failed`，不会把未验证的回滚写为成功。
- 升级失败不清理旧版本或失败的新版本目录；保留后者便于诊断。重试前人工核对并处理失败目标目录，不自动覆盖同名 release。
- 单实例 `systemctl restart` 存在短暂中断，**不是零停机发布**。SIGKILL、断电及跨服务数据恢复仍需操作手册与现场验收。

## 五、人工回滚

先停止并发升级操作，核对兼容性和旧版本目录；保留升级后的数据，不要静默覆盖新记录。切回旧核心、配套 Python Agent 并重启后，分别验证 readiness、登录同源代理和业务只读检查。

```bash
sudo ln -s /opt/changeguard/releases/changeguard-3-1-2 /opt/changeguard/current.rollback
sudo mv -Tf /opt/changeguard/current.rollback /opt/changeguard/current
sudo systemctl restart changeguard
curl -f http://127.0.0.1:8080/health/ready
```

自动回滚不恢复数据库或 Python 数据。恢复备份必须评估升级后写入，并在维护窗口执行；完整步骤与证据字段见 `docs/release-3.1.3-verification.md`。

## 六、开发验证

```bash
bash -n deploy/production/changeguard-core-install.sh
bash -n deploy/upgrade/changeguard-upgrade-preflight.sh
bash -n deploy/upgrade/changeguard-upgrade.sh
bash -n deploy/upgrade/changeguard-upgrade-watcher.sh
sudo python3 -B -m unittest discover -s tests/deploy -v
```

`tests/deploy/test_upgrade.py` 中的 `PreflightTests` 直接回归预检：核心不可达、
无可回滚版本、`current` 指向 release-root 外、包摘要不符、目标版本不符、声明了
Agent 却缺少凭据，都必须失败关闭；并且每次运行后断言 release-root 未被改动。

测试使用真实安装器、合成安装包、隔离临时目录及模拟 `systemctl`/curl；不连接网络、不重启真实服务，不是生产备份恢复验收。非 Linux/root 环境明确跳过；CI 在 Linux root 下强制执行。测试文件系统必须支持 POSIX 权限，不能通过放宽生产检查来适配 Windows 挂载盘。
