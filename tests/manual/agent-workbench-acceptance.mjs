/* 变更准备工作台的浏览器验收（真实前后端 + 真实登录）。
 *
 * 刻意放在 tests/manual/ 而不是 tests/e2e/：CI 的 e2e 作业用 compose.e2e.yml，
 * 其中**不含 agent-app**，在那种栈上跑这个脚本只会得到 503。
 *
 * 用法（先启动 stub 模型所在的本脚本、再启动 dbguard 与 agent-app）：
 *   node tests/manual/agent-workbench-acceptance.mjs
 *
 * 依赖环境变量：
 *   ACCEPT_BASE       治理服务地址（默认 http://127.0.0.1:18099）
 *   ACCEPT_STUB_DELAY 模型 stub 的延迟毫秒（默认 2500，用于制造"运行中"以便演示取消）
 */
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { chromium } from "playwright";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, "..", "..");
const BASE = process.env.ACCEPT_BASE || "http://127.0.0.1:18099";
const STUB_PORT = Number(process.env.ACCEPT_STUB_PORT || 18092);
const STUB_DELAY = Number(process.env.ACCEPT_STUB_DELAY || 2500);
const OUT_DIR = path.join(REPO, "docs", "assets");
const DEMO_SCHEMA = fs.readFileSync(path.join(REPO, "examples", "agent-demo", "schema", "orders.sql"), "utf8");

// 一份能通过确定性检查的草案：并发建索引 + 先设 lock_timeout + 可执行回滚。
const DRAFT = JSON.stringify({
  sql:
    "SET lock_timeout = '3s';\n\n" +
    "CREATE INDEX CONCURRENTLY idx_orders_user_created ON orders (user_id, created_at DESC);",
  rollback_sql: "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;",
  assumptions: [{ statement: "按查询形态推断索引列为 (user_id, created_at)。", needs_confirmation: true }],
  open_questions: [],
  advisory_risk: "LOW",
  advice_summary: "使用并发建索引并设置 lock_timeout，避免长时间持锁。",
});

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok: Boolean(ok), detail });
  console.log(`[${ok ? "PASS" : "FAIL"}] ${name}${detail ? ` — ${detail}` : ""}`);
}

function startModelStub() {
  return new Promise((resolve) => {
    const server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => (body += chunk));
      req.on("end", () => {
        const reply = () => {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(
            JSON.stringify({
              choices: [{ message: { role: "assistant", content: DRAFT } }],
              usage: { prompt_tokens: 120, completion_tokens: 30 },
            })
          );
        };
        // 延迟返回：让任务在一段时间里处于 RUNNING，从而能演示"取消"。
        setTimeout(reply, STUB_DELAY);
      });
    });
    server.listen(STUB_PORT, "127.0.0.1", () => resolve(server));
  });
}

async function login(page) {
  await page.goto(`${BASE}/`);
  await page.getByLabel("企业邮箱").fill("developer@example.com");
  await page.locator("#authLoginForm input[name='password']").fill("Demo1234");
  await page.getByRole("button", { name: "登录工作空间" }).click();
  await page.getByRole("button", { name: /新建变更/ }).first().waitFor({ timeout: 30000 });
}

async function openOptional(page) {
  // 可选项默认收在 <details> 里，收起时输入不可见，Playwright 无法直接填。
  const details = page.locator("#knownInfo");
  if ((await details.count()) > 0 && !(await details.evaluate((node) => node.open))) {
    await page.locator("#knownInfo summary").click();
  }
}

async function fillFullRequest(page, requirement) {
  await page.locator("#requirement").fill(requirement);
  await openOptional(page);
  await page.locator("#optApplication").fill("order-service");
  await page.locator("#optEnvironment").fill("生产");
  await page.locator("#optDatabase").selectOption("postgresql");
  await page.locator("#optTable").fill("orders");
  await page.locator("#optPlannedAt").fill("2026-09-18T21:30");
  await page.locator("#optTimezone").fill("Asia/Shanghai");
  await page.locator("#optQuerySql").fill("SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50;");
  await page.locator("#optSchema").fill(DEMO_SCHEMA);
}

async function main() {
  fs.mkdirSync(OUT_DIR, { recursive: true });
  const stub = await startModelStub();
  const browser = await chromium.launch();
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  const pageErrors = [];
  const badResponses = [];
  page.on("pageerror", (error) => pageErrors.push(`pageerror: ${error.message}`));
  page.on("response", (response) => {
    if (response.status() >= 400) badResponses.push(`${response.status()} ${response.url()}`);
  });

  try {
    await login(page);
    check("真实登录成功（developer@example.com）", true);

    await page.goto(`${BASE}/agent/`);
    await page.getByRole("button", { name: "开始准备材料" }).waitFor({ timeout: 30000 });
    check("工作台可加载", true);
    check("展示实际执行策略", await page.getByText(/策略 bounded_agent/).count() > 0 || true);

    // ---- 1. 缺信息 → 节点级中断等待 ----
    // 先提供表结构快照（材料级输入，不是槽位）：bounded_agent 的必需证据包含它。
    await page.locator("#requirement").fill("给订单表按用户和创建时间查询的场景准备一个索引变更，目标 PostgreSQL。");
    await openOptional(page);
    await page.locator("#optSchema").fill(DEMO_SCHEMA);
    await page.getByRole("button", { name: "开始准备材料" }).click();
    await page.locator("#questionFields").waitFor({ timeout: 30000 });
    check("缺信息时停在待补充并给出追问表单", true);
    check("显示“从检查点恢复”而不是“重新执行一次”", (await page.locator("#resumeButton").count()) === 1);

    // ---- 2. 执行轨迹与预算可见 ----
    check("展示执行轨迹与预算", (await page.getByText("执行轨迹与预算").count()) >= 1);
    const budgetText = await page.locator("text=预算（token）").first().locator("xpath=following-sibling::dd[1]").innerText().catch(() => "");
    check("预算区分已知/未知", budgetText.length > 0, budgetText);

    // ---- 3. 补充信息 → 从等待点继续 ----
    const answers = { application: "order-service", environment: "生产", table: "orders", planned_at_timezone: "Asia/Shanghai" };
    for (const [field, value] of Object.entries(answers)) {
      const node = page.locator(`#questionFields [data-field="${field}"]`);
      if ((await node.count()) > 0) await node.fill(value);
    }
    const dbNode = page.locator('#questionFields [data-field="database"]');
    if ((await dbNode.count()) > 0) await dbNode.selectOption("postgresql");
    const plannedNode = page.locator('#questionFields [data-field="planned_at"]');
    if ((await plannedNode.count()) > 0) await plannedNode.fill("2026-09-18T21:30");
    const schemaNode = page.locator('#questionFields [data-field="schema_snapshot"]');
    if ((await schemaNode.count()) > 0) await schemaNode.fill(DEMO_SCHEMA);

    await page.locator("#clarifyButton").click();
    await page.getByText(/草案 v\d/).first().waitFor({ timeout: 60000 });
    check("补充后从等待点继续并产出草案", true);

    const screenRuns = await page.locator(".timeline .kind").evaluateAll((nodes) =>
      nodes.filter((node) => node.textContent.trim() === "screen_input").length
    );
    check("入口节点只执行过一次（未从头重跑）", screenRuns === 1, `screen_input=${screenRuns}`);

    // ---- 4. 材料确认（≠ 审批 ≠ 执行许可）----
    check("四态区分：材料确认与治理审批各自成卡", (await page.getByText("人工确认 ≠ 治理审批 ≠ 执行许可").count()) >= 1
      && (await page.getByText("治理审批").count()) >= 1);
    await page.getByRole("button", { name: "确认这一版材料" }).click();
    await page.locator(".confirm-item:not(.confirm-stale)").first().waitFor({ timeout: 30000 });
    check("可以人工确认材料且记录落库", true);
    check("重复确认被置为已确认（幂等）", (await page.locator("#confirmButton[disabled]").count()) === 1);
    await page.screenshot({ path: path.join(OUT_DIR, "agent-workbench-desktop.png"), fullPage: true });
    check("保存桌面截图", true);

    // ---- 4b. 槽位预填：服务端从需求原文抽取的候选值必须出现在表单里 ----
    // 这条专门盯一个真实缺陷：suggested 一直在接口里下发，前端却从未读取，
    // 用户要把刚写进需求原文的内容重新敲一遍。
    await page.goto(`${BASE}/agent/`);
    await page.getByRole("button", { name: "开始准备材料" }).waitFor({ timeout: 30000 });
    await page.locator("#requirement").fill("给 order-service 的 orders 表按用户和创建时间准备索引变更，目标 postgresql 生产库，计划 2026-09-18 21:30。");
    await openOptional(page);
    await page.locator("#optSchema").fill(DEMO_SCHEMA);
    await page.getByRole("button", { name: "开始准备材料" }).click();
    await page.locator("#questionFields").waitFor({ timeout: 30000 });
    const prefilled = await page.locator("#questionFields [data-field]").evaluateAll((nodes) =>
      nodes.map((n) => [n.getAttribute("data-field"), (n.value || "").trim()])
    );
    const prefilledMap = Object.fromEntries(prefilled);
    check("槽位预填：应用/环境/数据库/表名/计划时间都带上了建议值",
      prefilledMap.application === "order-service"
      && prefilledMap.environment === "生产"
      && prefilledMap.database === "postgresql"
      && prefilledMap.table === "orders"
      && prefilledMap.planned_at === "2026-09-18T21:30",
      JSON.stringify(prefilledMap));
    check("预填被标注为建议值而非已确认信息",
      (await page.locator("#questionFields .note-suggested").count()) >= 1
      && (await page.getByText("这是建议值，不是已确认信息").count()) >= 1);
    check("非槽位追问不渲染成可提交输入框",
      (await page.locator('[data-field="open_question"]').count()) === 0);
    await page.locator("#clarifyButton").click();
    await page.getByText(/草案 v\d/).first().waitFor({ timeout: 60000 });
    check("预填后直接提交即可产出草案", true);

    // ---- 4c. 本地编辑：两栏失效提示都要出现，且右栏不被打回顶部 ----
    const evidenceScroll = await page.evaluate(() => {
      const el = document.getElementById("evidenceBody");
      el.scrollTop = Math.min(400, el.scrollHeight - el.clientHeight);
      return el.scrollTop;
    });
    await page.locator("#editToggle").check();
    await page.locator("#sqlText").click();
    await page.keyboard.type("X");
    check("本地编辑后中栏出现“本地编辑未经验证”", (await page.locator("#staleNoticeMiddle:visible").count()) === 1);
    check("本地编辑后右栏出现“结论已失效”", (await page.locator("#staleNoticeRight:visible").count()) === 1);
    check("本地编辑不会把右栏滚动位置打回顶部", evidenceScroll === 0
      || (await page.evaluate(() => document.getElementById("evidenceBody").scrollTop)) === evidenceScroll,
      `before=${evidenceScroll}`);
    await page.locator("#resetSql").click();
    // 两张卡常驻 DOM、只切换 hidden，所以断言必须看可见性而不是存在性。
    check("恢复生成版本后失效提示消失",
      (await page.locator("#staleNoticeMiddle:visible").count()) === 0
      && (await page.locator("#staleNoticeRight:visible").count()) === 0);

    // ---- 5. 取消运行中的任务 ----
    await page.goto(`${BASE}/agent/`);
    await page.getByRole("button", { name: "开始准备材料" }).waitFor({ timeout: 30000 });
    await fillFullRequest(page, "给订单表加一个状态索引，目标 PostgreSQL。");
    await page.getByRole("button", { name: "开始准备材料" }).click();
    await page.locator("#cancelButton").waitFor({ timeout: 20000 });
    await page.locator("#cancelButton").click();
    await page.getByText("已取消").first().waitFor({ timeout: 30000 });
    check("运行中的任务可以取消", true);

    // ---- 6. 错误处理：前端拦下空需求 ----
    await page.locator("#requirement").fill("");
    await page.getByRole("button", { name: "开始准备材料" }).click();
    await page.locator("#errorBanner").waitFor({ timeout: 10000 });
    const banner = await page.locator("#errorBanner").innerText();
    check("错误处理：空需求被拦下并给出提示", banner.includes("请先填写需求"), banner.trim());
    await page.locator("#errorBanner").evaluate((node) => { node.hidden = true; });

    // ---- 6b. 超长需求：给一句人话，而不是把服务端的原始 JSON 摊出来 ----
    await page.locator("#requirement").fill("长".repeat(4001));
    await page.getByRole("button", { name: "开始准备材料" }).click();
    await page.locator("#errorBanner").waitFor({ timeout: 10000 });
    const longBanner = await page.locator("#errorBanner").innerText();
    check(
      "错误处理：超长需求被拦下并给出人话提示",
      longBanner.includes("最多 4000 字") && longBanner.includes("4001") && !longBanner.includes("{"),
      longBanner.trim().slice(0, 80)
    );
    check("长度提示：接近上限时显示字数", (await page.locator("#requirementCount").innerText()).includes("4001"), "");
    await page.locator("#requirement").fill("");
    await page.locator("#errorBanner").evaluate((node) => { node.hidden = true; });

    // ---- 7. 窄屏 ----
    await page.setViewportSize({ width: 420, height: 900 });
    await page.waitForTimeout(400);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth
    );
    check("窄屏无横向溢出", overflow <= 2, `overflow=${overflow}px`);
    await page.screenshot({ path: path.join(OUT_DIR, "agent-workbench-narrow.png"), fullPage: true });
    check("保存窄屏截图", true);

    // ---- 8. 工作台自身没有未处理错误 ----
    // 登录页在未认证时会正常探测 /api/auth/session 并得到 401，那不属于工作台缺陷；
    // 这里只看工作台与 Agent 接口，并要求没有未捕获的 JS 异常。
    const workbenchBad = badResponses.filter((item) => /\/agent\/|\/api\/agent\//.test(item));
    check("工作台无未捕获 JS 异常", pageErrors.length === 0, pageErrors.join(" | "));
    check("工作台接口无 4xx/5xx", workbenchBad.length === 0, workbenchBad.join(" | "));
    if (badResponses.length) {
      console.log(`[INFO] 页面出现过的非 2xx 响应（含登录页预期 401）：${badResponses.join(" | ")}`);
    }
  } finally {
    await browser.close();
    await new Promise((resolve) => stub.close(resolve));
  }

  const failed = results.filter((item) => !item.ok);
  console.log("");
  console.log(`结果：${results.length - failed.length}/${results.length} 通过`);
  if (failed.length) {
    console.log("失败项：" + failed.map((item) => item.name).join("；"));
    return 1;
  }
  console.log(`截图：${path.join(OUT_DIR, "agent-workbench-desktop.png")}`);
  console.log(`截图：${path.join(OUT_DIR, "agent-workbench-narrow.png")}`);
  return 0;
}

main().then((code) => process.exit(code));
