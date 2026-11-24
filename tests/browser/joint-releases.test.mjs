import assert from "node:assert/strict";
import test from "node:test";
import { mkdir, readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { chromium } from "@playwright/test";

const root = new URL("../../internal/httpapi/web/", import.meta.url);
const environment = "production";
const change = (id, title, app, digit) => ({id, title, environment, application_id: app, status: "APPROVED",
  artifact_version: 2, artifact_sha256: digit.repeat(64), commit_sha: digit.repeat(40)});
const migration = change("chg_migration", "订单数据库迁移", "app_db", "a");
const application = change("chg_app", "订单服务部署", "app_orders", "b");
const replacement = change("chg_retry", "修订后的数据库迁移", "app_db", "c");
const appReplacement = change("chg_app_retry", "修订后的订单服务部署", "app_orders", "d");
const step = (item, number, name) => ({change_id: item.id, application_id: item.application_id, title: item.title,
  artifact_version: item.artifact_version, artifact_sha256: item.artifact_sha256, commit_sha: item.commit_sha,
  gitlab_project_id: number, gitlab_job: name});
const plan = {id: "release_original", name: "订单生产发布", environment, status: "IN_PROGRESS", created_at: "2026-11-24T07:00:00Z",
  steps: [{...step(migration, 42, "migrate_orders"),
    consumption: {job_id: 720, pipeline_id: 42, at: "2026-11-24T07:02:00Z"},
    deployment: {deployment_id: 81, status: "FAILED", occurred_at: "2026-11-24T07:04:00Z"}}, step(application, 42, "deploy_orders")],
  timeline: [{approval_status: "COMPLETED", reviewer_name: "发布审核员", gate_status: "CONSUMED", deployment_status: "FAILED", blocking_cause: "GitLab 部署未成功；需要替代计划"},
    {approval_status: "APPROVED", gate_status: "NOT_CONSUMED", deployment_status: "NOT_RUN", blocking_cause: "前置步骤 1 的 GitLab 部署结果为 FAILED"}]};
const partial = {...plan, id: "release_partial", name: "仅末步失败的联合发布", steps: [
  {...plan.steps[0], deployment: {...plan.steps[0].deployment, status: "SUCCESS"}},
  {...plan.steps[1], consumption: {...plan.steps[0].consumption, job_id: 721},
    deployment: {...plan.steps[0].deployment, deployment_id: 82}}],
  timeline: [{approval_status: "COMPLETED", gate_status: "CONSUMED", deployment_status: "SUCCESS"},
    {...plan.timeline[0], deployment_status: "FAILED"}]};

test("联合发布：创建、失败替代、固定制品、刷新与只读权限", async t => {
  const browser = await chromium.launch({headless: true, executablePath: process.env.CHANGEGUARD_BROWSER_EXECUTABLE || undefined});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 980}});
    const errors = [], posted = [];
    let user = {id: "actor", name: "审核人", role: "数据库审核人"};
    let newPlan;
    const changes = [migration, application, replacement, appReplacement];
    page.on("pageerror", e => errors.push(e.message));
    page.on("console", message => { if (message.type() === "error") errors.push(message.text()); });
    await page.route("http://joint-release.test/**", async request => {
      const {pathname: path} = new URL(request.request().url());
      if (path === "/api/events") return request.fulfill({contentType: "text/event-stream", body: "retry: 60000\n\n: ready\n\n"});
      if (!path.startsWith("/api/")) {
        const name = path === "/" ? "index.html" : path.slice(1);
        if (name === "favicon.ico") return request.fulfill({status: 204});
        return request.fulfill({path: fileURLToPath(new URL(name, root)),
          ...(name.endsWith(".mjs") ? {contentType: "application/javascript"} : {})});
      }
      const data = {"/api/auth/status": {enabled: true}, "/api/auth/session": {user, csrf_token: "browser-fixture-csrf"},
        "/api/users": [user], "/api/apps": [], "/api/dashboard": {}, "/api/governance/outcomes": {},
        "/api/changes": {items: changes}, "/api/policies": [], "/api/audits": [], "/api/config/status": {},
        "/api/joint-releases": {items: [plan, partial]}, ["/api/joint-releases/" + plan.id]: plan,
        ["/api/joint-releases/" + partial.id]: partial};
      if (path === "/api/joint-releases" && request.request().method() === "POST") {
        assert.equal(request.request().headers()["x-csrf-token"], "browser-fixture-csrf");
        const payload = request.request().postDataJSON();
        posted.push(payload);
        newPlan = {...plan, ...payload, id: "release_new", timeline: [], steps: payload.steps.map(pin => ({...pin, title: pin.change_id}))};
        if (payload.supersedes === partial.id) {
          newPlan.steps.unshift({...partial.steps[0], inherited_from: partial.id});
          newPlan.timeline = [partial.timeline[0], {approval_status: "APPROVED", gate_status: "NOT_CONSUMED", deployment_status: "NOT_RUN"}];
        }
        return request.fulfill({status: 201, json: newPlan});
      }
      if (path === "/api/joint-releases/release_new" && newPlan) return request.fulfill({json: newPlan});
      const item = changes.find(item => path === "/api/changes/" + item.id);
      return request.fulfill({json: item || data[path] || []});
    });
    const main = page.locator("#mainContent");
    const screenshot = async name => {
      if (!process.env.CHANGEGUARD_JOINT_SCREENSHOTS) return;
      await mkdir("test-results", {recursive: true});
      await page.screenshot({path: `test-results/joint-release-${name}.png`, fullPage: true});
      assert((await readFile(`test-results/joint-release-${name}.png`)).length > 0);
    };
    await page.goto("http://joint-release.test/#/joint-releases");
    await main.getByRole("link", {name: "新建联合发布"}).click();
    const verify = async (number, item, project, job) => {
      await main.getByLabel(`步骤 ${number} 变更 ID`).fill(item.id);
      if (project) await main.getByLabel(`步骤 ${number} GitLab 项目 ID`).fill(project);
      if (job) await main.getByLabel(`步骤 ${number} GitLab 作业名`).fill(job);
      const row = main.locator(".joint-edit-steps > li").nth(number - 1);
      await row.getByRole("button", {name: "核对当前制品"}).click();
      await row.getByText(new RegExp(item.artifact_sha256)).waitFor();
    };
    await main.getByLabel("计划名称").fill("第一次联合发布");
    await main.getByLabel("发布环境").fill(environment);
    await verify(1, migration, "42", "migrate_orders");
    await verify(2, application, "42", "deploy_orders");
    await main.getByRole("button", {name: "上移步骤 2"}).click();
    await main.getByRole("button", {name: "下移步骤 1"}).click();
    await main.getByRole("button", {name: "创建计划", exact: true}).click();
    await main.getByRole("heading", {name: "第一次联合发布"}).waitFor();
    assert.deepEqual(posted[0].steps.map(item => item.change_id), [migration.id, application.id]);

    await main.getByRole("link", {name: "返回发布列表"}).click();
    await main.getByRole("link", {name: plan.name}).click();
    await main.getByText("部署失败", {exact: true}).waitFor();
    await main.getByText("未收到证据", {exact: true}).waitFor();
    await main.getByRole("button", {name: "刷新证据"}).click();
    await main.getByRole("link", {name: "创建替代计划"}).waitFor();
    await screenshot("timeline");
    await main.getByRole("link", {name: "创建替代计划"}).click();
    await page.setViewportSize({width: 390, height: 844});
    await verify(1, replacement);
    await verify(2, application);
    await screenshot("replacement-mobile");
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await main.getByRole("button", {name: "确认替代并创建"}).click();
    await main.getByText(`替代变更 ${migration.id}`).waitFor();
    assert.equal(posted[1].supersedes, plan.id);
    assert.equal(posted[1].steps[0].artifact_version, 2);
    assert.equal(posted[1].steps[0].replaces_change_id, migration.id);
    assert.equal(posted[1].steps[1].change_id, application.id);

    await t.test("只替换最后失败的一步，并沿用成功前序", async () => {
      await main.getByRole("link", {name: "返回发布列表"}).click();
      await main.getByRole("link", {name: partial.name}).click();
      await main.getByRole("link", {name: "创建替代计划"}).click();
      await main.getByLabel("步骤 1 变更 ID").waitFor();
      assert.equal(await main.locator(".joint-edit-steps > li").count(), 1);
      await verify(1, appReplacement);
      await main.getByRole("button", {name: "确认替代并创建"}).click();
      const timeline = main.locator(".joint-timeline > li");
      await timeline.nth(0).getByText("已核实成功", {exact: true}).waitFor();
      await timeline.nth(0).getByRole("link", {name: partial.id, exact: true}).waitFor();
      await timeline.nth(1).getByText(`替代变更 ${application.id}`).waitFor();
      assert.deepEqual(posted[2].steps, [{change_id: appReplacement.id, artifact_version: appReplacement.artifact_version,
        artifact_sha256: appReplacement.artifact_sha256, gitlab_project_id: 42, gitlab_job: "deploy_orders", replaces_change_id: application.id}]);
      assert.equal(posted[2].supersedes, partial.id);
      assert.equal(await timeline.count(), 2);
      await screenshot("inherited-predecessor");
    });

    user = {...user, role: "后端开发"};
    await page.goto("http://joint-release.test/#/joint-releases/" + plan.id + "/supersede");
    await page.reload();
    await main.getByRole("heading", {name: plan.name}).waitFor();
    assert.equal(await main.getByRole("link", {name: "创建替代计划"}).count(), 0);
    await main.getByRole("link", {name: "返回发布列表"}).click();
    await main.getByRole("link", {name: plan.name}).waitFor();
    assert.equal(await main.getByRole("link", {name: "新建联合发布"}).count(), 0);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
