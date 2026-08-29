const { test, expect } = require("@playwright/test");
const fs = require("node:fs");
const path = require("node:path");

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

test("CONFIG and SQL change completes deterministic check and real shadow rehearsal", async ({ page }) => {
  const title = `E2E CONFIG SQL ${Date.now()}`;
  const config = fixture("golden-config.yaml");
  const forwardSQL = fixture("golden-forward.sql");
  const rollbackSQL = fixture("golden-rollback.sql");

  await page.goto("/");
  await page.getByLabel("企业邮箱").fill("developer@example.com");
  await page.getByLabel("密码").fill("Demo1234");
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
    checks_passed: 4,
    checks_total: 4
  }));
  expect(rehearsed.status).toBe("WAITING_APPROVAL");
});
