import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";

const require = createRequire(process.cwd() + "/package.json");
const { chromium } = require("@playwright/test");
const root = new URL("../../internal/httpapi/web/agent/", import.meta.url);

// 浏览器布局回归；所有网络请求均被拦截，fixture 不代表真实后端。
test("agent: responsive forms, long content, keyboard focus and reduced motion", async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ locale: "zh-CN" });
    const files = Object.fromEntries(await Promise.all(["index.html", "styles.css", "app.js"].map(async name => [name, await readFile(new URL(name, root), "utf8")])));
    // 跟随真实页面的脚本列表，包含共享 time.js；仍仅从本地读取并拦截请求。
    for (const [, src] of files['index.html'].matchAll(/<script\b[^>]*\bsrc="([^"]+)"/g)) {
      const url = src.startsWith('/') ? new URL(`..${src}`, root) : new URL(src, root);
      files[src.split('/').pop()] = await readFile(url, 'utf8');
    }
    await page.route("**/*", async route => {
      const path = new URL(route.request().url()).pathname;
      const name = path === "/agent/" ? "index.html" : path.split("/").pop();
      if (files[name]) return route.fulfill({ body: files[name], contentType: name.endsWith("css") ? "text/css" : name.endsWith("js") ? "text/javascript" : "text/html" });
      const body = path === "/api/auth/status" ? { enabled: false } : { status: "ok", provider: { llm_configured: false }, knowledge_chunks: 0 };
      return route.fulfill({ json: body });
    });
    await page.goto("http://agent-layout.test/agent/");
    await page.locator("#knownInfo summary").focus();
    await page.keyboard.press("Enter");
    assert.equal(await page.locator("#knownInfo").getAttribute("open"), "");
    assert.equal(await page.locator("summary").evaluate(el => getComputedStyle(el).outlineStyle), "solid");
    await page.locator("#optPlannedAt").fill("2030-12-31T23:59");
    assert.equal(await page.locator("#optPlannedAt").inputValue(), "2030-12-31T23:59");
    await page.evaluate(() => {
      const long = "demo_fixture_long_identifier_".repeat(12);
      state.task = {
        task_id: long, requirement: "演示布局测试 " + long, status: "CHECK_BLOCKED", error: long,
        events: [
          { at: "2020-01-01T12:34:56Z", kind: long, detail: long },
          // agent-app/app/service.py: RECOVERY_UNCERTAINTY_NOTE，经真实 renderTimeline 渲染。
          { at: "2020-01-01T12:35:56Z", kind: "recovery_at_least_once",
            detail: "恢复不宣称 exactly-once：被中断的节点可能已经开始执行，外部模型请求可能已经发出；" +
              "重新执行该节点属于 at-least-once。已发生的消耗以调用账本（usage）为准，不当作没有发生过。" }
        ],
        investigation: { tool_observations: [{ tool: long, kind: "generic", error: long }] },
        draft: { version: 1, application: long, sql: "-- demo SQL", rollback_sql: "-- demo rollback", ai_advice: { summary: long.repeat(8) } }
      };
      render();
    });
    for (const width of [320, 375, 420, 768, 860, 900, 1100, 1440, 1920]) {
      await page.setViewportSize({ width, height: 900 });
      const result = await page.evaluate(() => {
        const date = document.querySelector("#optPlannedAt");
        const grid = document.querySelector(".grid-2");
        const overflow = [...document.querySelectorAll(".col-body, .card, .timeline, .timeline li, .check-item, .evidence-item")]
          .filter(el => el.scrollWidth > el.clientWidth + 1).map(el => el.className);
        // 原生日期编辑内容的固有宽度；不能仅断言外框未溢出。
        const probe = date.cloneNode();
        probe.value = date.value;
        probe.style.cssText = "position:absolute;width:max-content;max-width:none;min-width:max-content;visibility:hidden";
        document.body.append(probe);
        const intrinsic = probe.getBoundingClientRect().width;
        probe.remove();
        const timeline = [...document.querySelectorAll(".timeline li")].map(li => {
          const at = li.querySelector(".at").getBoundingClientRect();
          const kind = li.querySelector(".kind").getBoundingClientRect();
          const detail = li.querySelector(".detail").getBoundingClientRect();
          const box = li.getBoundingClientRect(), style = getComputedStyle(li);
          const left = box.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft);
          const right = box.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight);
          return { kind: li.querySelector(".kind").textContent, available: right - left,
            detailWidth: detail.width, belowMetadata: detail.top >= Math.max(at.bottom, kind.bottom),
            withinBounds: [at, kind, detail].every(rect => rect.left >= left - 1 && rect.right <= right + 1),
            aligned: Math.abs(detail.left - left) <= 1 && Math.abs(detail.right - right) <= 1 };
        });
        return { timeline, overflow, pageOverflow: document.documentElement.scrollWidth > innerWidth,
          dateWidth: date.getBoundingClientRect().width, intrinsic,
          columns: getComputedStyle(grid).gridTemplateColumns.split(" ").length,
          radius: getComputedStyle(document.querySelector("#optApplication")).borderRadius };
      });
      assert.deepEqual(result.overflow, [], `${width}px local overflow`);
      assert.equal(result.pageOverflow, false, `${width}px page overflow`);
      assert.ok(result.timeline.some(item => item.kind === "recovery_at_least_once"), `${width}px recovery event rendered`);
      for (const item of result.timeline) {
        const label = `${width}px ${item.kind}`;
        assert.ok(Math.abs(item.detailWidth - item.available) <= 1, `${label}: detail ${item.detailWidth} != available ${item.available}`);
        assert.ok(item.belowMetadata, `${label}: detail must be below time and event name`);
        assert.ok(item.withinBounds, `${label}: timeline children overflow horizontally`);
        assert.ok(item.aligned, `${label}: detail must span the content row`);
      }
      assert.ok(result.dateWidth >= result.intrinsic, `${width}px date ${result.dateWidth} < intrinsic ${result.intrinsic}`);
      assert.equal(result.columns, width === 768 || width === 860 ? 2 : 1, `${width}px columns`);
      assert.equal(result.radius, "6px");
      if (process.env.AGENT_LAYOUT_SCREENSHOTS) await page.screenshot({ path: `${process.env.AGENT_LAYOUT_SCREENSHOTS}/agent-layout-${width}.png`, fullPage: true });
    }
    await page.setViewportSize({ width: 1440, height: 900 });
    const desktop = await page.locator('.col').evaluateAll(cols => cols.map(col => {
      const body = col.querySelector('.col-body');
      return { height: col.getBoundingClientRect().height, scrollHeight: body.scrollHeight, clientHeight: body.clientHeight };
    }));
    for (const col of desktop) assert.ok(col.height <= 768, `1440x900 column exceeds viewport cap: ${JSON.stringify(col)}`);
    const draftBody = page.locator('#draftBody');
    assert.ok(await draftBody.evaluate(body => body.scrollHeight > body.clientHeight), 'long draft must scroll inside its column');
    await draftBody.evaluate(body => { body.scrollTop = body.scrollHeight; });
    const reachable = await page.locator('#confirmButton').evaluate(button => {
      const body = button.closest('.col-body'), box = button.getBoundingClientRect(), bounds = body.getBoundingClientRect();
      return { atBottom: Math.abs(body.scrollHeight - body.clientHeight - body.scrollTop) <= 1,
        visible: box.top >= bounds.top && box.bottom <= bounds.bottom && box.bottom <= innerHeight,
        hit: button.contains(document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2)) };
    });
    assert.deepEqual(reachable, { atBottom: true, visible: true, hit: true });
    await page.locator('#confirmButton').click({ trial: true });
    await page.locator("#optApplication").focus();
    const focus = await page.evaluate(() => {
      const style = getComputedStyle(document.activeElement);
      return { color: style.outlineColor, width: style.outlineWidth, style: style.outlineStyle };
    });
    assert.equal(focus.style, "solid");
    assert.equal(focus.width, "2px");
    assert.match(focus.color, /47,\s*109,\s*246|2f6df6/i);
    await page.setViewportSize({ width: 1100, height: 280 });
    const short = await page.evaluate(() => {
      document.getElementById("identityBanner").hidden = false;
      document.getElementById("errorBanner").hidden = false;
      const col = document.querySelector(".col");
      const box = col.getBoundingClientRect();
      return { maxHeight: getComputedStyle(col).maxHeight, bottom: box.bottom, height: box.height };
    });
    assert.equal(short.maxHeight, "none");
    assert.ok(short.height > 140, `short viewport collapsed the column: ${short.height}`);
    await page.emulateMedia({ reducedMotion: "reduce" });
    assert.equal(await page.locator("#createButton").evaluate(el => getComputedStyle(el).transitionDuration), "0s");
  } finally {
    await browser.close();
  }
});
