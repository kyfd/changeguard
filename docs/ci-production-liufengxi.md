# ChangeGuard 生产 CI 门禁接入（liufengxi.top）

面向真实流水线的最小接入说明。平台地址：`https://liufengxi.top`。

## 1. 前置条件

1. 变更单已完成确定性检查，证据状态为 **REAL**（非 DEMO_ONLY / NOT_RUN）。
2. 已由**非提交人**的审核角色完成独立审批。
3. 技术负责人或有权账号已签发**一次性通行证**（页面「变更详情 → 签发通行证」）。
4. 流水线可访问公网 `https://liufengxi.top`（或内网回源）。

## 2. 计算制品摘要

在包含真实 SQL / 配置 / Kubernetes 文件的仓库中：

```bash
# 构建 gate CLI（仓库根目录）
go build -o changeguard-gate ./cmd/changeguard-gate

# 按 manifest 计算摘要（示例见 examples/ci-demo）
./changeguard-gate digest -manifest ./.changeguard.json
```

摘要必须来自**即将发布的真实文件字节**。任意空白变更都会导致 verify 失败。

## 3. 验签与消费

```bash
export CHANGEGUARD_URL="https://liufengxi.top"
export CHANGEGUARD_TOKEN="cg1_..."   # 页面签发的一次性 token，masked 变量
export CI_JOB_ID="${CI_JOB_ID:-$GITHUB_RUN_ID}"

./changeguard-gate verify  -manifest ./.changeguard.json
./changeguard-gate consume -manifest ./.changeguard.json
```

- `verify`：只读校验摘要、环境、有效期。
- `consume`：原子消费，成功后通行证作废；重复消费会被拒绝。

等价 HTTP：

```http
POST /api/gate/verify
Authorization: Bearer <passport_token>
Content-Type: application/json

{"artifact_sha256":"<sha256>","environment":"生产环境","consumer":"<ci-job-id>"}
```

`consume` 路径相同，改为 `/api/gate/consume`。

## 4. GitLab CI 片段

```yaml
changeguard-gate:
  stage: deploy
  image: golang:1.22
  variables:
    CHANGEGUARD_URL: "https://liufengxi.top"
  script:
    - go install ./cmd/changeguard-gate@latest || true
    - changeguard-gate verify  -manifest ./.changeguard.json
    - changeguard-gate consume -manifest ./.changeguard.json
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
```

Protected variables：`CHANGEGUARD_TOKEN`（masked）。

## 5. Jenkins 片段

```groovy
withCredentials([string(credentialsId: 'changeguard-passport', variable: 'CHANGEGUARD_TOKEN')]) {
  sh '''
    export CHANGEGUARD_URL=https://liufengxi.top
    export CI_JOB_ID=$BUILD_ID
    ./changeguard-gate verify  -manifest ./.changeguard.json
    ./changeguard-gate consume -manifest ./.changeguard.json
  '''
}
```

## 6. 验收清单

| 步骤 | 预期 |
|------|------|
| 开发提交配置/K8s 变更 | 规则检查可解释命中 |
| 证据 NOT_RUN / DEMO_ONLY | **不能**审批为可发布 |
| 审核人 ≠ 提交人 | 可批准 |
| 签发通行证 | 仅返回一次明文 token |
| verify 摘要正确 | 200 / 通过 |
| 篡改文件后 verify | 失败阻断 |
| consume 一次 | 成功，变更 COMPLETED |
| 再次 consume | 拒绝 |

## 7. 安全要求

- 通行证 token 只放在 CI masked 变量，禁止写入日志全文。
- 生产 `DBGUARD_ENABLE_DEMO_*=false`。
- 会话建议 `DBGUARD_SESSION_MODE=redis`。
- 定期轮换 `DBGUARD_PASSPORT_HMAC_SECRET` 与服务器登录凭据。
