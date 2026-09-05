const { test, expect } = require("@playwright/test");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const http = require("node:http");
const { execFile } = require("node:child_process");
const { promisify } = require("node:util");
const execute = promisify(execFile);

// This flow handles a short-lived test credential. Do not retain it in traces,
// videos or automatic failure screenshots.
test.use({ trace: "off", video: "off", screenshot: "off" });

const fixture = name => fs.readFileSync(path.join(__dirname, "..", "fixtures", name), "utf8").trim();

async function changeFromPage(page, changeId) {
  return page.evaluate(async id => {
    const response = await fetch(`/api/changes/${encodeURIComponent(id)}`, {
      credentials: "same-origin",
      cache: "no-store"
    });
    if (!response.ok) throw new Error(`GET change failed: ${response.status} ${await response.text()}`);
    return response.json();
  }, changeId);
}

test("CONFIG and SQL: shadow validation, independent approval, CLI gate and audit", async ({ page, browser }, testInfo) => {
  test.setTimeout(180_000);
  const baseURL = testInfo.project.use.baseURL;
  const replicaURL = process.env.CHANGEGUARD_E2E_REPLICA_URL || "http://127.0.0.1:18081";
  expect(["localhost", "127.0.0.1", "[::1]"]).toContain(new URL(baseURL).hostname);
  expect(["localhost", "127.0.0.1", "[::1]"]).toContain(new URL(replicaURL).hostname);
  expect(new URL(replicaURL).origin).not.toBe(new URL(baseURL).origin);
  expect((await page.request.get(`${replicaURL}/health/ready`)).ok()).toBe(true);
  const title = `E2E CONFIG SQL ${Date.now()}`;
  const config = fixture("golden-config.yaml");
  const forwardSQL = fixture("golden-forward.sql");
  const rollbackSQL = fixture("golden-rollback.sql");

  await page.goto("/");
  await page.getByLabel("企业邮箱").fill("developer@example.com");
  await page.locator("#authLoginForm input[name='password']").fill("Demo1234");
  await page.getByRole("button", { name: "登录工作空间" }).click();
  await expect(page.getByRole("button", { name: /新建变更/ }).first()).toBeVisible();

  await page.getByRole("button", { name: /新建变更/ }).first().click();
  const form = page.locator("#createForm");
  await expect(form).toBeVisible();
  await form.getByLabel("变更标题 *").fill(title);
  await form.getByLabel("变更类型").selectOption({ label: "混合发布" });
  await form.getByLabel("目标环境").selectOption({ label: "预发布环境" });
  await form.getByLabel("配置 Diff").fill(config);
  await form.getByLabel("数据库 SQL").fill(forwardSQL);
  await form.getByLabel("数据库回滚 SQL").fill(rollbackSQL);
  await form.getByLabel("整体回滚方案 *").fill("恢复上一版配置，并执行登记的回滚 SQL 删除本次影子验证表。");
  await form.getByRole("button", { name: "保存变更单" }).click();

  await expect(page).toHaveURL(/#\/changes\/[^/]+$/);
  const changeId = page.url().split("/").pop();
  expect(changeId).toBeTruthy();

  const created = await changeFromPage(page, changeId);
  expect(created.title).toBe(title);
  expect(created.sql.trim()).toBe(forwardSQL);
  expect(created.rollback_sql.trim()).toBe(rollbackSQL);
  expect(created.artifacts).toEqual(expect.arrayContaining([
    expect.objectContaining({ kind: "CONFIG", content: config })
  ]));

  await page.getByRole("button", { name: /提交规则检查/ }).click();
  await expect.poll(async () => (await changeFromPage(page, changeId)).status, { timeout: 30_000 })
    .toBe("READY_FOR_EXPERIMENT");

  await page.reload();
  await page.getByRole("button", { name: /开始预发布验证/ }).click();
  await expect.poll(async () => {
    const change = await changeFromPage(page, changeId);
    return change.experiment?.status || change.status;
  }, { timeout: 60_000 }).toBe("PASSED");

  const rehearsed = await changeFromPage(page, changeId);
  expect(rehearsed.experiment).toEqual(expect.objectContaining({
    mode: "POSTGRES",
    status: "PASSED",
    rollback_verified: true,
    checks_passed: 5,
    checks_total: 5
  }));
  expect(rehearsed.status).toBe("WAITING_APPROVAL");

  const session = await (await page.request.get("/api/auth/session")).json();
  const selfApproval = await page.request.post(`/api/changes/${changeId}/approve`, {
    headers: { "X-CSRF-Token": session.csrf_token }, data: { comment: "self approval must fail" }
  });
  expect(selfApproval.status()).toBe(403);
  expect((await changeFromPage(page, changeId)).status).toBe("WAITING_APPROVAL");

  const reviewerContext = await browser.newContext({ baseURL });
  const reviewer = await reviewerContext.newPage();
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "changeguard-gate-e2e-"));
  try {
    await reviewer.goto("/");
    await reviewer.getByLabel("企业邮箱").fill("reviewer@example.com");
    await reviewer.locator("#authLoginForm input[name='password']").fill("Demo1234");
    await reviewer.getByRole("button", { name: "登录工作空间" }).click();
    await expect(reviewer.locator("#mainContent")).toBeVisible();
    await reviewer.goto(`/#/changes/${changeId}`);
    await reviewer.getByRole("button", { name: "审批通过", exact: true }).first().click();
    await reviewer.locator("#reviewForm textarea[name='comment']").fill("已核对配置、迁移与回滚验证结果。");
    await reviewer.getByRole("button", { name: "确认批准", exact: true }).click();
    await expect.poll(async () => (await changeFromPage(reviewer, changeId)).status).toBe("APPROVED");

    await reviewer.reload();
    await reviewer.getByRole("button", { name: "签发通行证", exact: true }).first().click();
    const tokenField = reviewer.locator("#passportTokenValue");
    await expect(tokenField).toBeVisible();
    await expect.poll(async () => (await tokenField.inputValue()).length > 0).toBe(true);
    const token = await tokenField.inputValue();
    await reviewer.getByRole("button", { name: "我已安全保存", exact: true }).click();
    await expect(tokenField).toHaveValue("");

    const manifest = {
      version: 1, environment: created.environment, change_type: created.change_type,
      rollback_plan: created.rollback_plan,
      sql_path: "forward.sql", rollback_sql_path: "rollback.sql",
      artifacts: created.artifacts.map((artifact, i) => ({
        kind: artifact.kind, name: artifact.name, source: artifact.source,
        language: artifact.language, path: `artifact-${i}.txt`
      }))
    };
    created.artifacts.forEach((artifact, i) => fs.writeFileSync(path.join(scratch, `artifact-${i}.txt`), artifact.content));
    fs.writeFileSync(path.join(scratch, "forward.sql"), forwardSQL);
    fs.writeFileSync(path.join(scratch, "rollback.sql"), rollbackSQL);
    const manifestPath = path.join(scratch, ".changeguard.json");
    fs.writeFileSync(manifestPath, JSON.stringify(manifest));
    const gate = path.join(scratch, process.platform === "win32" ? "gate.exe" : "gate");
    await execute("go", ["build", "-o", gate, "./cmd/changeguard-gate"], { cwd: path.resolve(__dirname, "../.."), timeout: 90_000 });
    const runGate = (command, consumer = `e2e-${changeId}`, url = baseURL) => execute(gate, [command, "-manifest", manifestPath, "-consumer", consumer], {
      env: { ...process.env, CHANGEGUARD_URL: url, CHANGEGUARD_TOKEN: token }, timeout: 20_000
    });
    const digest = JSON.parse((await runGate("digest")).stdout);
    expect(digest.artifact_sha256).toBe(created.artifact_sha256);
    expect(JSON.parse((await runGate("verify")).stdout).allowed).toBe(true);

    const firstArtifact = path.join(scratch, "artifact-0.txt");
    const original = fs.readFileSync(firstArtifact, "utf8");
    fs.writeFileSync(firstArtifact, original + "\n# changed after approval\n");
    await expect(runGate("consume")).rejects.toMatchObject({ code: 1, stderr: expect.stringContaining("ARTIFACT_MISMATCH") });
    fs.writeFileSync(firstArtifact, original);

    manifest.environment = "wrong-environment";
    fs.writeFileSync(manifestPath, JSON.stringify(manifest));
    await expect(runGate("consume")).rejects.toMatchObject({ code: 1 });
    manifest.environment = created.environment;
    fs.writeFileSync(manifestPath, JSON.stringify(manifest));
    expect((await changeFromPage(reviewer, changeId)).status).toBe("APPROVED");

    // Drop the downstream connection only after the real backend has returned
    // a complete response. This is a lost-response test, not a mocked commit.
    let committed;
    const dropProxy = http.createServer(async (request, response) => {
      try {
        const chunks = [];
        for await (const chunk of request) chunks.push(chunk);
        const upstream = await fetch(new URL("/api/gate/consume", baseURL), {
          method: "POST", headers: { "Content-Type": "application/json", Authorization: request.headers.authorization },
          body: Buffer.concat(chunks), signal: AbortSignal.timeout(10_000)
        });
        const body = await upstream.json();
        committed = { status: upstream.status, body };
      } catch {
        // Leave committed unset: the assertion below must distinguish an
        // upstream failure from the intended post-commit response loss.
      } finally {
        response.destroy();
      }
    });
    await new Promise((resolve, reject) => {
      dropProxy.once("error", reject);
      dropProxy.listen(0, "127.0.0.1", resolve);
    });
    try {
      const proxyURL = `http://127.0.0.1:${dropProxy.address().port}`;
      await expect(runGate("consume", `e2e-${changeId}`, proxyURL)).rejects.toMatchObject({ code: 1 });
      expect(committed?.status).toBe(200);
      expect(committed?.body.allowed).toBe(true);
    } finally {
      dropProxy.closeAllConnections();
      await new Promise(resolve => dropProxy.close(resolve));
    }
    const first = committed.body;
    const replay = JSON.parse((await runGate("consume", `e2e-${changeId}`, replicaURL)).stdout);
    expect(first.allowed).toBe(true);
    expect(replay.allowed).toBe(true);
    expect(replay.passport.consumed_at).toBe(first.passport.consumed_at);
    expect(replay.passport.consumed_by).toBe(first.passport.consumed_by);
    await expect(runGate("consume", "different-pipeline", replicaURL)).rejects.toMatchObject({ code: 1, stderr: expect.stringContaining("PASSPORT_REPLAY") });
    expect((await changeFromPage(reviewer, changeId)).status).toBe("COMPLETED");

    const audits = await (await reviewer.request.get("/api/audits?limit=500")).json();
    const events = audits.filter(event => event.change_id === changeId);
    for (const action of ["APPROVE", "PASSPORT_ISSUED", "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED"]) {
      expect(events.filter(event => event.action === action)).toHaveLength(1);
    }
    expect(JSON.stringify(events).includes(token)).toBe(false);
    await reviewer.reload();
    await expect(reviewer.locator("#mainContent")).toContainText("通行证已消费");
    await expect(reviewer.locator("#mainContent")).toContainText("规则检查通过，无阻断项");
    await expect(reviewer.locator("#mainContent")).toContainText(rehearsed.rule_set_version);
    await expect(reviewer.locator("#impactGraphBody")).toContainText("当前版本未提供逐变更影响图谱");
    await expect(reviewer.locator("#mainContent")).not.toContainText("METHOD_NOT_ALLOWED");
    expect(await reviewer.locator(".side-info-row").evaluateAll(rows => rows.every(row => row.scrollWidth <= row.clientWidth + 1))).toBe(true);
    await reviewer.screenshot({ path: testInfo.outputPath("gate-completed.png"), fullPage: true });
  } finally {
    await reviewerContext.close();
    fs.rmSync(scratch, { recursive: true, force: true });
  }
});
