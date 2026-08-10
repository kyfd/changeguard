# ChangeGuard 升级系统

基于 GitHub Releases 的升级包发布 + 服务器一键升级/自动回滚。

## 架构

```text
开发者打 tag（v2026.08.10.1）
        │
        ▼
GitHub Actions (.github/workflows/release.yml)
  1. 全量测试 + race + vet
  2. 生成 verification.json（验证证据）
  3. 离线构建（复用 deploy/production/build-core-git-release.sh）
  4. 打包 changeguard-<version>.tar.gz
        │
        ▼
GitHub Releases 发布（tar.gz + sha256）
        │
        ▼
服务器执行（root）：
  deploy/upgrade/changeguard-upgrade.sh
  1. 下载 + SHA256 校验
  2. changeguard-core-install.sh 原子安装（防篡改/路径穿越/缺文件检查）
  3. 切换 current 软链 + 重启服务
  4. 健康检查（超时自动回滚上一版本）
  5. 清理旧版本（默认保留 3 个）
```

## 一、发布新版本（开发者）

```bash
# 1. 打 tag（必须 annotated tag）
git tag -a v2026.08.10.1 -m "release 2026.08.10.1"
git push origin v2026.08.10.1

# 2. GitHub Actions 自动构建并发布到 Releases
#    完成后在 Releases 页面复制：
#    - 升级包下载 URL
#    - 升级包 SHA256（.sha256 文件内容）
```

## 二、服务器升级（运维）

```bash
# 下载升级脚本（首次）
sudo mkdir -p /opt/changeguard/scripts
sudo curl -fL -o /opt/changeguard/scripts/changeguard-upgrade.sh \
  https://raw.githubusercontent.com/<owner>/<repo>/main/deploy/upgrade/changeguard-upgrade.sh
sudo chmod +x /opt/changeguard/scripts/changeguard-upgrade.sh

# 执行升级
sudo bash /opt/changeguard/scripts/changeguard-upgrade.sh \
  --version 2026.08.10.1 \
  --archive-url https://github.com/<owner>/<repo>/releases/download/v2026.08.10.1/changeguard-2026.08.10.1.tar.gz \
  --expected-sha256 <从 Releases 复制的 64 位哈希>
```

### 升级脚本参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--version` | 必填 | 目标版本号（对应 release_id 后缀） |
| `--archive-url` | 必填 | 升级包下载地址 |
| `--expected-sha256` | 必填 | 升级包 SHA256（防篡改） |
| `--release-root` | `/opt/changeguard/releases` | release 存放目录 |
| `--current-link` | `/opt/changeguard/current` | 当前版本软链 |
| `--service` | `changeguard` | systemd 服务名 |
| `--health-url` | `http://127.0.0.1:8080/health/ready` | 健康检查端点 |
| `--health-timeout` | `60` | 健康检查超时秒数 |
| `--keep-archives` | `3` | 保留的旧版本数量 |

## 三、回滚（手动）

升级脚本在健康检查失败时自动回滚。手动回滚：

```bash
# 1. 查看可用版本
ls -1 /opt/changeguard/releases/

# 2. 切回旧版本
sudo ln -sfn /opt/changeguard/releases/changeguard-<旧版本> /opt/changeguard/current
sudo systemctl restart changeguard

# 3. 验证
curl -sf http://127.0.0.1:8080/health/ready
```

## 四、安全设计

1. **下载校验**：升级包 SHA256 与 GitHub Releases 发布值比对，防止下载源被篡改
2. **安装校验**：`changeguard-core-install.sh` 解压前检查路径穿越/软链/特殊文件/成员数量/大小上限；解压后 `sha256sum -c` 全量校验；manifest 与 verification.json 交叉校验（version/tag/commit/source_sha256）
3. **原子切换**：新版本先完整安装到独立目录，校验通过才切软链；失败不影响当前运行版本
4. **自动回滚**：重启后健康检查失败，自动切回上一版本并重启
5. **无停机**：软链切换 + systemd 重启，升级窗口仅重启几秒

## 五、升级包内容

```
changeguard-<version>.tar.gz
└── changeguard-<version>/
    ├── dbguard              # Linux amd64 二进制（755）
    ├── SHA256SUMS           # 全部文件 SHA256
    ├── release-manifest.json # 版本/commit/各文件哈希
    ├── verification.json    # 构建验证证据（测试/race/vet 全过）
    ├── source.bundle        # git bundle（完整提交历史）
    ├── source.tar.gz        # 源码归档
    ├── modules.txt          # 依赖清单
    ├── module-verify.txt    # go mod verify 输出
    ├── binary-buildinfo.txt # go version -m 输出
    ├── bundle-verify.txt    # git bundle verify 输出
    └── build.log            # 构建日志
```
