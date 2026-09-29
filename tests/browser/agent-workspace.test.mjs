import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdir } from 'node:fs/promises';
import { chromium } from '@playwright/test';

// Synthetic API fixture: interaction and rendering coverage, not backend E2E.
test('task workspace: filters, lifecycle, stale preview and responsive dialog', async () => {
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const files = Object.fromEntries(await Promise.all(['index.html', 'app.js', 'styles.css'].map(async name => [name, await readFile(new URL('../../internal/httpapi/web/agent/' + name, import.meta.url), 'utf8')])));
    let task = { task_id: 'fixture-workspace', requirement: '合成测试：订单索引变更', status: 'FAILED', source: 'production', events: [], error: '合成测试失败记录' };
    let lastQuery = '', stale = false, deleteCalls = 0, listFailure = false;
    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      const name = url.pathname === '/agent/' ? 'index.html' : url.pathname.split('/').pop();
      if (files[name]) return route.fulfill({ body: files[name], contentType: name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html' });
      if (url.pathname === '/api/auth/status') return route.fulfill({ json: { enabled: false } });
      if (url.pathname === '/api/agent/tasks') {
        lastQuery = url.search;
        if (listFailure) return route.fulfill({ status: 503, json: { detail: 'fixture unavailable' } });
        return route.fulfill({ json: url.searchParams.get('q') === 'missing' ? [] : [task] });
      }
      if (url.pathname.endsWith('/delete-preview')) return route.fulfill({ json: { task_id: task.task_id, allowed: true, blockers: [], record_version: 'fixture-version', retained: ['audit_events', 'checkpoints', 'usage'] } });
      if (url.pathname.endsWith('/archive')) task = { ...task, archived_at: '2030-01-01T00:00:00Z' };
      if (url.pathname.endsWith('/restore')) task = task.deleted_at ? { ...task, deleted_at: null } : { ...task, archived_at: null };
      if (url.pathname.endsWith('/delete')) {
        deleteCalls++;
        assert.equal(route.request().postDataJSON().record_version, 'fixture-version');
        if (stale) return route.fulfill({ status: 409, json: { detail: '预览已过期，请重新预览' } });
        task = { ...task, deleted_at: '2030-01-02T00:00:00Z' };
      }
      if (url.pathname.includes('/tasks/')) return route.fulfill({ json: task });
      return route.fulfill({ json: { status: 'ok', provider: { llm_configured: false } } });
    });
    await page.goto('http://workspace.test/agent/');
    await page.locator('#taskHistory option[value="fixture-workspace"]').waitFor({ state: 'attached' });
    await page.locator('#taskHistory').selectOption(task.task_id);
    await page.locator('#archiveTask').waitFor();
    await page.locator('#requirement').fill('保留未提交需求');
    await page.locator('#archiveTask').click();
    await page.locator('#deleteTask').waitFor();
    assert.equal(await page.locator('#retryButton').count(), 0);
    assert.equal(await page.locator('#requirement').inputValue(), '保留未提交需求');
    await page.locator('#deleteTask').click();
    await page.locator('#deleteConfirm:enabled').waitFor();
    assert.match(await page.locator('#deleteDetails').innerText(), /保留：审计事件、执行检查点、用量记录/);
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#deleteDialog').evaluate(el => el.open), false);
    assert.equal(await page.locator('#deleteTask').evaluate(el => el === document.activeElement), true);
    assert.equal(deleteCalls, 0);
    stale = true;
    await page.locator('#deleteTask').click();
    await page.locator('#deleteConfirm').click();
    await page.locator('#errorBanner').filter({ hasText: '预览已过期' }).waitFor();
    assert.equal(await page.locator('#deleteDialog').evaluate(el => el.open), false);
    stale = false;
    await page.locator('#deleteTask').click();
    await page.locator('#deleteConfirm:enabled').waitFor();
    const output = process.env.CHANGEGUARD_SCREENSHOT_DIR;
    for (const width of [1440, 375, 320]) {
      await page.setViewportSize({ width, height: 900 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
      if (width <= 600) {
        for (const selector of ['#deleteConfirm', '#deleteCancel', '#restoreTask', '#deleteTask']) {
          assert.ok(await page.locator(selector).evaluate(el => el.getBoundingClientRect().height >= 44), `${selector} mobile touch target`);
        }
      }
      if (output) { await mkdir(output, { recursive: true }); await page.screenshot({ path: `${output}/v31-task-workspace-dialog-${width}.png`, fullPage: true }); }
    }
    await page.locator('#deleteConfirm').click();
    await page.locator('#restoreTask').filter({ hasText: '恢复到归档' }).waitFor();
    await page.locator('#restoreTask').click();
    await page.locator('#deleteTask').waitFor();
    await page.locator('#restoreTask').click();
    await page.locator('#archiveTask').waitFor();
    await page.locator('.history-filters summary').click();
    await page.locator('#historyQuery').fill('missing');
    await page.locator('#historySource').selectOption('evaluation');
    await page.locator('#historyWorkspace').selectOption('trash');
    await page.locator('#historyFilters button').click();
    await page.locator('#historyFeedback').filter({ hasText: '没有符合条件' }).waitFor();
    assert.match(lastQuery, /q=missing/);
    assert.match(lastQuery, /source=evaluation/);
    assert.match(lastQuery, /workspace=trash/);
    assert.equal(await page.locator('#requirement').inputValue(), '保留未提交需求');
    for (const width of [1440, 375]) {
      await page.setViewportSize({ width, height: 900 });
      if (output) await page.screenshot({ path: `${output}/v31-task-workspace-${width}.png`, fullPage: true });
    }
    listFailure = true;
    await page.locator('#refreshTasks').click();
    await page.locator('#historyFeedback').filter({ hasText: '加载失败' }).waitFor();
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
