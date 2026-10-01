import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdir } from 'node:fs/promises';
import { chromium } from '@playwright/test';

// 合成 API fixture：项目知识导入/检索与评测中心的前端闭环。
// 只做交互与渲染回归，不代表后端端到端（真实后端联调另行记录）。
test('workbench M3: knowledge import/search and eval center', async () => {
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage();
    const errors = [];
    const posts = [];
    page.on('pageerror', error => errors.push(error.message));
    const files = Object.fromEntries(await Promise.all(['index.html', 'app.js', 'styles.css'].map(async name => [name, await readFile(new URL('../../internal/httpapi/web/agent/' + name, import.meta.url), 'utf8')])));
    let liveStatus = 'completed';
    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      const name = url.pathname === '/agent/' ? 'index.html' : url.pathname.split('/').pop();
      if (files[name]) return route.fulfill({ body: files[name], contentType: name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html' });
      if (url.pathname === '/api/auth/status') return route.fulfill({ json: { enabled: false } });
      if (url.pathname === '/api/apps') return route.fulfill({ json: [{ id: 'order-service', name: '订单服务' }] });
      if (url.pathname === '/api/agent/tasks') return route.fulfill({ json: [] });
      if (url.pathname === '/api/agent/knowledge' && route.request().method() === 'POST') {
        posts.push(route.request().postDataJSON());
        return route.fulfill({ status: 201, json: { knowledge_id: 'kb_fixture_1', organization_id: 'org_demo', kind: 'norms', title: '索引并发规范', status: 'active', snippet_count: 2, injection_hits: [] } });
      }
      if (url.pathname === '/api/agent/knowledge/search') {
        return route.fulfill({ json: [{ knowledge_id: 'kb_fixture_1', doc_id: 'norms/kb_fixture_1', title: '索引并发规范', section: '1.1 热表并发建索引', status: 'active', snippet: '并发建索引必须使用 CREATE INDEX CONCURRENTLY', score: 3.2 }] });
      }
      if (url.pathname === '/api/agent/evals' && route.request().method() === 'POST') {
        posts.push(route.request().postDataJSON());
        if (route.request().postDataJSON().provider === 'live') {
          liveStatus = 'not_run';
          return route.fulfill({ status: 201, json: { job_id: 'eval_live', organization_id: 'org_demo', created_by: 'alice', provider: 'live', strategy: 'bounded_agent', split: 'dev', status: 'not_run', task_source: 'evaluation', case_count: 0, failure_classes: {}, notes: ['真实模型评测未启用或缺少凭据：记为 NOT_RUN，不伪造通过率或质量提升'] } });
        }
        return route.fulfill({ status: 201, json: { job_id: 'eval_fixture_1', organization_id: 'org_demo', created_by: 'alice', provider: 'scripted', strategy: 'bounded_agent', split: 'dev', status: 'completed', task_source: 'evaluation', case_count: 3, summary: { total: 3, executed: 3, passed: 2, failed: 1, not_run: 0, skipped: 0 }, failure_classes: { check_blocked: 1 }, notes: [] } });
      }
      return route.fulfill({ json: { status: 'ok', provider: { llm_configured: false }, knowledge_chunks: 0 } });
    });

    await page.goto('http://m3.test/agent/');

    // 项目知识：导入 → 服务端返回 id 与片段数。
    await page.locator('#knowledgeBox summary').click();
    await page.locator('#knowledgeTitle').fill('索引并发规范');
    await page.locator('#knowledgeBody').fill('# 索引并发规范\n\n文档版本：v3.1\n适用范围：PostgreSQL 生产库\n\n## 1.1 热表并发建索引\n\n并发建索引必须使用 CREATE INDEX CONCURRENTLY。\n');
    await page.locator('#knowledgeApplication').selectOption('order-service');
    await page.locator('#knowledgeImport').click();
    await page.locator('#knowledgeFeedback').filter({ hasText: 'kb_fixture_1' }).waitFor();
    assert.equal(posts[0].kind, 'norms');
    assert.equal(posts[0].application_id, 'order-service');

    // 检索：结果在服务端过滤后返回。
    await page.locator('#knowledgeQuery').fill('并发建索引');
    await page.locator('#knowledgeSearch').click();
    await page.locator('#knowledgeResults').filter({ hasText: 'norms/kb_fixture_1' }).waitFor();

    // 评测中心：离线运行 → 汇总与失败分类。
    await page.locator('#evalBox summary').click();
    await page.locator('#evalRun').click();
    await page.locator('#evalResult').filter({ hasText: 'eval_fixture_1' }).waitFor();
    const resultText = await page.locator('#evalResult').innerText();
    assert.match(resultText, /2 \/ 3/);
    assert.match(resultText, /失败分类：check_blocked×1/);

    // 真实模型未启用 → NOT_RUN，不伪造结果。
    await page.locator('#evalProvider').selectOption('live');
    await page.locator('#evalRun').click();
    await page.locator('#evalResult').filter({ hasText: 'eval_live' }).waitFor();
    // 徽章以大写呈现状态：NOT_RUN 必须可见，而不是被省略成看起来有结果。
    assert.match(await page.locator('#evalResult').innerText(), /NOT_RUN/);
    assert.match(await page.locator('#evalFeedback').innerText(), /NOT_RUN/);
    assert.equal(liveStatus, 'not_run');

    const output = process.env.CHANGEGUARD_SCREENSHOT_DIR;
    for (const width of [1440, 375]) {
      await page.setViewportSize({ width, height: 900 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
      if (output) { await mkdir(output, { recursive: true }); await page.screenshot({ path: `${output}/v31-m3-workbench-${width}.png`, fullPage: true }); }
    }
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
