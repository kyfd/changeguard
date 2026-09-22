import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";

const require = createRequire(process.cwd() + "/package.json");
const { chromium } = require("@playwright/test");

// 隔离的控制台 UI 回归：使用真实 HTML/CSS/事件处理，拦截全部网络，不代表真实后端。
test("console: notification bounds, drawer focus, finding modal and auth canvas", async () => {
const root = new URL("../../internal/httpapi/web/", import.meta.url);
const html = (await readFile(new URL("index.html", root), "utf8")).replace(/<script\b[^>]*>[\s\S]*?<\/script>/g, "").replace(/<link[^>]*stylesheet[^>]*>/g, "");
const css = await readFile(new URL("styles.css", root), "utf8");
const app = (await readFile(new URL("app.js", root), "utf8")).replace(/start\(\);\s*$/, "bindEvents();");
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/*', route => route.fulfill({ contentType: 'text/html', body: html }));
  await page.goto('http://console-ui.test/');
  await page.addStyleTag({ content: css });
  await page.evaluate(() => {
    window.pendingFrames = new Set();
    const request = window.requestAnimationFrame.bind(window), cancel = window.cancelAnimationFrame.bind(window);
    window.requestAnimationFrame = cb => {
      const id = request(time => { pendingFrames.delete(id); cb(time); });
      pendingFrames.add(id); return id;
    };
    window.cancelAnimationFrame = id => { pendingFrames.delete(id); cancel(id); };
    window.listenerCounts = { resize: 0, mousemove: 0, mouseleave: 0 };
    for (const target of [window, document.querySelector('#authGate')]) {
      const add = target.addEventListener.bind(target), remove = target.removeEventListener.bind(target);
      target.addEventListener = (type, ...args) => { if (type in listenerCounts) listenerCounts[type]++; add(type, ...args); };
      target.removeEventListener = (type, ...args) => { if (type in listenerCounts) listenerCounts[type]--; remove(type, ...args); };
    }
  });
  await page.addScriptTag({ content: app });
  await page.evaluate(() => {
    document.querySelector('#navList').innerHTML = '<button class="nav-item">演示导航（隔离测试）</button>';
    document.querySelector('#notifyPanelBody').textContent = '演示通知（隔离测试，不是真实后端数据）';
  });
  for (const width of [320, 375, 390, 420, 768, 860, 900, 1100, 1440, 1920]) {
    await page.setViewportSize({ width, height: 800 });
    await page.evaluate(() => { document.querySelector('#notifyPanel').hidden = false; });
    const box = await page.locator('#notifyPanel').boundingBox();
    assert(box.x >= 0 && box.x + box.width <= width, `notification bounds at ${width}: ${JSON.stringify(box)}`);
  }
  await page.setViewportSize({ width: 390, height: 800 });
  // 等到关闭位移真正到达终点，再验证重新打开时的同步焦点转移。
  const waitForDrawerClosed = () => page.waitForFunction(() => {
    const sidebar = document.querySelector('#sidebar');
    const style = getComputedStyle(sidebar);
    return sidebar.inert && style.visibility === 'hidden' &&
      Math.abs(new DOMMatrixReadOnly(style.transform).m41 + sidebar.getBoundingClientRect().width) < 0.01;
  });
  await page.waitForFunction(() => document.querySelector('#sidebar').inert);
  await page.evaluate(() => { document.querySelector('#notifyPanel').hidden = true; });
  assert.equal(await page.locator('#sidebar').evaluate(el => el.inert), true);
  await page.locator('#menuButton').focus();
  await page.keyboard.press('Tab');
  assert.equal(await page.evaluate(() => !!document.activeElement.closest('#sidebar')), false);
  await waitForDrawerClosed();
  await page.locator('#menuButton').click();
  assert.equal(await page.locator('#menuButton').getAttribute('aria-expanded'), 'true');
  assert.equal(await page.evaluate(() => !!document.activeElement.closest('#sidebar')), true, await page.evaluate(() => document.activeElement.outerHTML + ' / ' + document.querySelector('#sidebar').outerHTML.slice(0, 150) + ' / ' + getComputedStyle(document.querySelector('#sidebar')).visibility));
  await page.keyboard.press('Escape');
  assert.equal(await page.evaluate(() => document.activeElement.id), 'menuButton');
  await waitForDrawerClosed();
  await page.locator('#menuButton').click();
  assert.equal(await page.evaluate(() => !!document.activeElement.closest('#sidebar')), true);
  await page.locator('#mobileBackdrop').click({ position: { x: 370, y: 400 } });
  assert.equal(await page.locator('#sidebar').evaluate(el => el.inert), true);
  await waitForDrawerClosed();
  await page.locator('#menuButton').click();
  assert.equal(await page.evaluate(() => !!document.activeElement.closest('#sidebar')), true);
  await page.evaluate(() => closeAllOverlays());
  assert.equal(await page.locator('#sidebar').evaluate(el => el.inert), true);
  assert.equal(await page.evaluate(() => document.activeElement.id), 'menuButton');
  await waitForDrawerClosed();
  await page.locator('#menuButton').click();
  assert.equal(await page.evaluate(() => !!document.activeElement.closest('#sidebar')), true);
  await page.setViewportSize({ width: 1100, height: 800 });
  await page.waitForFunction(() => !document.querySelector('#sidebar').inert && document.querySelector('#menuButton').getAttribute('aria-expanded') === 'false');
  assert.equal(await page.locator('#sidebar').evaluate(el => el.inert), false);
  assert.equal(await page.locator('#menuButton').getAttribute('aria-expanded'), 'false');
  await page.setViewportSize({ width: 667, height: 375 });
  await page.evaluate(() => document.querySelector('#findingModal').classList.add('open'));
  await page.locator('#findingContent').focus();
  await page.waitForTimeout(400);
  const layout = await page.locator('#findingModal').evaluate(el => {
    const body = el.querySelector('.modal-body'), modal = el.querySelector('.modal').getBoundingClientRect();
    const footer = el.querySelector('.modal-footer').getBoundingClientRect();
    return { scroll: body.scrollHeight > body.clientHeight, overflow: getComputedStyle(body).overflowY, top: modal.top, bottom: modal.bottom, footerBottom: footer.bottom, scrollTop: body.scrollTop };
  });
  assert(layout.scroll && layout.overflow === 'auto' && layout.scrollTop > 0);
  assert(layout.top >= 0 && layout.bottom <= 375 && layout.footerBottom <= layout.bottom);
  await page.evaluate(() => document.querySelector('#findingModal').classList.remove('open'));
  await page.setViewportSize({ width: 1440, height: 900 });
  for (let i = 0; i < 5; i++) {
    await page.evaluate(() => renderAuthGate('演示登录错误（隔离测试）'));
    await page.waitForTimeout(40);
    assert.equal(await page.locator('#authCanvas').count(), 1);
    assert.equal(await page.evaluate(() => pendingFrames.size), 1);
    assert.deepEqual(await page.evaluate(() => listenerCounts), { resize: 1, mousemove: 1, mouseleave: 1 });
    assert(await page.locator('#authCanvas').evaluate(c => c.width > 0 && c.getContext('2d').getImageData(0, 0, c.width, c.height).data.some(n => n > 0)));
  }
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.waitForFunction(() => pendingFrames.size === 0);
  assert.equal(await page.evaluate(() => pendingFrames.size), 0);
  await page.evaluate(() => { document.querySelector('#authGate').hidden = true; });
  await page.waitForTimeout(40);
  assert.deepEqual(await page.evaluate(() => listenerCounts), { resize: 0, mousemove: 0, mouseleave: 0 });
  await page.evaluate(() => renderAuthGate());
  assert.equal(await page.evaluate(() => pendingFrames.size), 0);
  assert.deepEqual(await page.evaluate(() => listenerCounts), { resize: 1, mousemove: 1, mouseleave: 1 });
  await page.evaluate(() => stopAuthParticles());
  assert.deepEqual(await page.evaluate(() => listenerCounts), { resize: 0, mousemove: 0, mouseleave: 0 });
  assert.equal(await page.evaluate(() => pendingFrames.size), 0);
  assert.deepEqual(errors, []);
  console.log('PASS: notification 10 widths; drawer keyboard/Escape/backdrop/breakpoint; 667x375 scrollable finding modal; 5 auth rerenders, RAF/listener cleanup, reduced motion. Isolated HTML; no backend E2E.');
} finally { await browser.close(); }
});
