import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdir } from 'node:fs/promises';
import { chromium } from '@playwright/test';

// 合成 API fixture：覆盖 M2 的执行轨迹、版本历史、服务端编辑与关联交互，
// 不代表后端端到端（真实后端联调另行记录）。检验渲染、交互与失败路径。
test('workbench M2: trace, version history, server edit and change link', async () => {
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const files = Object.fromEntries(await Promise.all(['index.html', 'app.js', 'styles.css'].map(async name => [name, await readFile(new URL('../../internal/httpapi/web/agent/' + name, import.meta.url), 'utf8')])));

    const baseDraft = { version: 2, requirement: '合成', application: 'order-service', environment: '生产', database: 'postgresql' };
    let task = {
      task_id: 'fixture-m2', requirement: '合成 M2', status: 'DRAFT_READY', source: 'production',
      events: [], error: null, revisions: 1, confirmations: [], material_hash: 'hash2', planned_at_missing: false,
      draft: { ...baseDraft, sql: 'CREATE INDEX CONCURRENTLY idx_a ON orders (a);', rollback_sql: 'DROP INDEX CONCURRENTLY idx_a;',
        deterministic_check: { status: 'PASSED', source: 'local_scan', items: [], blocking_count: 0 }, assumptions: [], open_questions: [], evidence: [], revision_notes: [] },
      usage: { known: false, requests: 1, reported_responses: 0, missing_responses: 1, cost_known: false, cost_estimate: null, calls: [] },
      change_links: [],
    };
    let versions = [
      { version: 1, origin: 'agent', actor: 'agent', created_at: '2026-09-28T00:00:00+00:00', reason: '', summary: '1 条变更语句', check_status: 'PASSED', content_hash: 'aaaa' },
      { version: 2, origin: 'user_edit', actor: 'alice', created_at: '2026-09-28T01:00:00+00:00', reason: '评审修订', summary: '1 条变更语句', check_status: 'PASSED', content_hash: 'bbbb' },
    ];
    let stale = false;
    let editPosts = 0;

    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      const name = url.pathname === '/agent/' ? 'index.html' : url.pathname.split('/').pop();
      if (files[name]) return route.fulfill({ body: files[name], contentType: name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html' });
      if (url.pathname === '/api/auth/status') return route.fulfill({ json: { enabled: false } });
      if (url.pathname === '/api/agent/tasks') return route.fulfill({ json: [task] });
      if (url.pathname.endsWith('/trace')) return route.fulfill({ json: {
        task_id: task.task_id, includes_model_reasoning: false, unknown: ['cost_estimate', 'step_duration_ms'],
        steps: [
          { index: 1, kind: 'event', name: 'created', status: 'ok', duration_ms: null, detail: '任务已创建', evidence_ids: [] },
          { index: 2, kind: 'model_call', name: 'generate', status: 'timeout', duration_ms: null, usage_known: false, prompt_tokens: null, error: 'timeout', evidence_ids: [], model: 'm' },
        ] } });
      if (url.pathname.endsWith('/draft/versions')) return route.fulfill({ json: versions });
      if (url.pathname.endsWith('/draft/diff')) return route.fulfill({ json: {
        task_id: task.task_id, from_version: 1, to_version: 2,
        sql_diff: '--- sql:before\n+++ sql:after\n-CREATE INDEX idx_a;\n+CREATE INDEX CONCURRENTLY idx_a;', rollback_diff: '' } });
      if (url.pathname.endsWith('/draft') && route.request().method() === 'POST') {
        editPosts++;
        if (stale) return route.fulfill({ status: 409, json: { detail: '草案已被更新（当前 v3），您看到的是旧版本；请刷新后重试' } });
        const body = route.request().postDataJSON();
        assert.equal(body.expected_version, task.draft.version);
        const nextVersion = task.draft.version + 1;
        task = { ...task, material_hash: 'hash' + nextVersion, draft: { ...task.draft, version: nextVersion, sql: body.sql, rollback_sql: body.rollback_sql } };
        versions = [...versions, { version: nextVersion, origin: 'user_edit', actor: 'alice', created_at: '2026-09-28T02:00:00+00:00', reason: body.reason, summary: '1 条变更语句', check_status: 'PASSED', content_hash: 'cccc' }];
        return route.fulfill({ json: task });
      }
      if (url.pathname.endsWith('/change-links')) {
        const body = route.request().postDataJSON();
        task = { ...task, change_links: [{ change_request_id: body.change_request_id, organization_id: 'org_demo', linked_by: 'alice', linked_at: '2026-09-28T03:00:00+00:00' }] };
        return route.fulfill({ json: task });
      }
      if (url.pathname.includes('/tasks/')) return route.fulfill({ json: task });
      return route.fulfill({ json: { status: 'ok', provider: { llm_configured: false } } });
    });

    await page.goto('http://m2.test/agent/');
    await page.locator('#taskHistory option[value="fixture-m2"]').waitFor({ state: 'attached' });
    await page.locator('#taskHistory').selectOption('fixture-m2');

    // 执行轨迹：真实步骤 + 未知显式标注，且不展示思维链。
    await page.locator('#tracePanel').waitFor();
    const traceText = await page.locator('#tracePanel').innerText();
    assert.match(traceText, /执行轨迹/);
    assert.match(traceText, /耗时未知/);
    assert.match(traceText, /未知项/);
    // 轨迹显式声明不含模型内部思维链，且确实没有推理内容字段。
    assert.match(traceText, /不含模型思维链/);
    assert.doesNotMatch(traceText, /推理过程|chain.of.thought/i);

    // 版本历史：生成版 + 人工编辑版。
    await page.locator('#versionHistory').waitFor();
    assert.match(await page.locator('#versionHistory').innerText(), /人工编辑/);
    await page.locator('[data-diff-to="2"]').click();
    await page.locator('#versionHistory pre').first().waitFor();
    assert.match(await page.locator('#versionHistory pre').first().innerText(), /CREATE INDEX CONCURRENTLY/);
    await page.locator('#closeDiff').click();

    // 服务端编辑：期望版本随请求发送，成功后出现新版本。
    await page.locator('#editToggle').check();
    await page.locator('#sqlText').fill('CREATE INDEX CONCURRENTLY idx_b ON orders (b);');
    await page.locator('#editReason').fill('按评审意见调整');
    await page.locator('#saveDraft:enabled').click();
    await page.locator('#versionHistory').filter({ hasText: 'v3' }).waitFor();
    assert.equal(editPosts, 1);

    // 陈旧版本（409）：显示服务端原因，不静默覆盖。
    await page.locator('#editToggle').check();
    await page.locator('#sqlText').fill('SELECT 1;');
    stale = true;
    await page.locator('#saveDraft:enabled').click();
    await page.locator('#errorBanner').filter({ hasText: '旧版本' }).waitFor();

    // 关联正式变更单：走服务端接口并回填。
    await page.waitForTimeout(300);
    await page.locator('#changeLinkInput').fill('chg_fixture_1');
    await page.locator('#linkChange').click();
    await page.locator('#changeLinkCard').filter({ hasText: 'chg_fixture_1' }).waitFor();

    const output = process.env.CHANGEGUARD_SCREENSHOT_DIR;
    for (const width of [1440, 375]) {
      await page.setViewportSize({ width, height: 900 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
      if (output) { await mkdir(output, { recursive: true }); await page.screenshot({ path: `${output}/v31-m2-workbench-${width}.png`, fullPage: true }); }
    }
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
