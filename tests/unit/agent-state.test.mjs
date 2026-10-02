import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('../../internal/httpapi/web/agent/app.js', import.meta.url), 'utf8');
function setup() {
  const nodes = new Map();
  const document = { readyState: 'loading', addEventListener() {}, activeElement: null,
    getElementById: (id) => nodes.get(id) || null };
  function node(id, parentElement = null) {
    const result = { id, parentElement, innerHTML: '', scrollTop: 42, scrollLeft: 7,
      value: '', checked: false, selectionStart: 1, selectionEnd: 3,
      querySelectorAll: () => [], contains: (other) => other === result,
      focus() { document.activeElement = result; },
      setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; } };
    nodes.set(id, result);
    return result;
  }
  for (const id of ['draftMeta', 'taskTimeline', 'evidenceBody', 'conversation', 'taskConfirmation', 'errorBanner']) node(id);
  const context = vm.createContext({
    document,
    setInterval: () => 1,
    clearInterval() {},
    $: (id) => document.getElementById(id),
    CSS: { escape: (value) => String(value).replace(/[^a-zA-Z0-9_-]/g, '\\$&') },
  });
  vm.runInContext(source + '\n globalThis.agent = { state, adoptTask, pollOnce, preserveView, draftSql, isLocallyEdited, refreshHealth, confirmMaterial, openHistoricalTask, refreshTaskHistory, createTask, saveDraft, linkChange, renderTrace, renderVersions, loadTrace, loadVersions, loadKnowledgeList, renderKnowledgeList, renderSnapshotCard, selectSnapshot, applySnapshotChange, createFormalChange, retryChangeRecovery, changeIdempotencyKey, loadAgentChanges };', context);
  vm.runInContext('render = () => { globalThis.fullRenders = (globalThis.fullRenders || 0) + 1; };', context);
  return { ...context.agent, context, nodes, document, node };
}
test('local edits cannot confirm the server draft', async () => {
  const h = setup();
  h.adoptTask(task({ status: 'DRAFT_READY' }));
  h.state.edits.sql = 'changed';
  vm.runInContext('api = async () => { throw new Error("must not request"); }; showError = message => { globalThis.warning = message; };', h.context);
  await h.confirmMaterial();
  assert.match(h.context.warning, /本地/);
});

test('historical task restores authorized server response and URL id only', async () => {
  const h = setup();
  h.context.URL = URL;
  h.context.window = { location: { href: 'http://localhost/agent/' }, history: { replaceState(_a, _b, url) { h.context.savedURL = String(url); } } };
  h.context.nextTask = task({ task_id: 'restored', status: 'DRAFT_READY' });
  vm.runInContext('globalThis.requests = []; api = async path => { globalThis.requests.push(path); return nextTask; };', h.context);
  await h.openHistoricalTask('restored');
  assert.equal(h.state.task.task_id, 'restored');
  // 详情请求必须在其中；轨迹/版本等附加请求不应替换掉它。
  assert.ok(h.context.requests.includes('/api/agent/tasks/restored'));
  assert.equal(h.context.savedURL, 'http://localhost/agent/?task=restored');
});

test('history fetches list and escapes returned ids', async () => {
  const h = setup();
  h.node('taskHistory');
  vm.runInContext('api = async path => { globalThis.requested = path; return [{ task_id: "<unsafe>", status: "FAILED" }]; };', h.context);
  await h.refreshTaskHistory();
  assert.equal(h.context.requested, '/api/agent/tasks');
  assert.match(h.nodes.get('taskHistory').innerHTML, /&lt;unsafe&gt;/);
});

test('late history list never replaces newer filter results', async () => {
  const h = setup();
  h.node('taskHistory');
  h.node('historyFeedback');
  vm.runInContext('globalThis.pending = []; api = () => new Promise(resolve => pending.push(resolve));', h.context);
  const first = h.refreshTaskHistory();
  const second = h.refreshTaskHistory();
  h.context.pending[1]([{ task_id: 'new-result', status: 'FAILED' }]);
  await second;
  h.context.pending[0]([{ task_id: 'old-result', status: 'FAILED' }]);
  await first;
  assert.match(h.nodes.get('taskHistory').innerHTML, /new-result/);
  assert.doesNotMatch(h.nodes.get('taskHistory').innerHTML, /old-result/);
});

test('archived task stops polling even if legacy status is nonterminal', () => {
  const h = setup();
  h.adoptTask(task({ status: 'NEEDS_INFO', archived_at: '2030-01-01' }));
  assert.equal(h.state.timer, null);
});

test('new task creation invalidates an in-flight history response', async () => {
  const h = setup();
  for (const id of ['requirement', 'optApplication', 'optEnvironment', 'optDatabase', 'optTable', 'optQuerySql', 'optTimezone', 'optSchema', 'optSnapshotKnowledge', 'optPlannedAt', 'createButton']) h.node(id);
  h.nodes.get('requirement').value = 'new task';
  h.context.created = task({ task_id: 'new' });
  vm.runInContext('clearError = () => {}; browserTimezone = () => "UTC"; globalThis.pendingHistory = {}; api = path => path === "/api/agent/tasks" ? Promise.resolve(created) : new Promise(resolve => { globalThis.pendingHistory[path] = resolve; });', h.context);
  const old = h.openHistoricalTask('old');
  await h.createTask({ preventDefault() {} });
  h.context.pendingHistory['/api/agent/tasks/old'](task({ task_id: 'old' }));
  await old;
  assert.equal(h.state.task.task_id, 'new');
});

test('failed history selection resumes polling the existing task', async () => {
  const h = setup();
  h.adoptTask(task());
  vm.runInContext('handleActionError = () => {}; api = async () => { throw new Error("404"); };', h.context);
  await h.openHistoricalTask('missing');
  assert.equal(h.state.task.task_id, 'A');
  assert.equal(h.state.timer, 1);
});

const task = (extra = {}) => ({ task_id: 'A', status: 'RUNNING', draft: { sql: 'select 1', rollback_sql: '', version: 1 }, ...extra });

test('switching task clears local SQL, rollback and edit mode even for identical drafts', () => {
  const h = setup();
  h.adoptTask(task());
  h.state.edits = { sql: 'local A', rollback: 'rollback A' };
  h.state.editing = true;
  h.adoptTask(task({ task_id: 'B' }));
  assert.equal(h.state.edits.sql, null);
  assert.equal(h.state.edits.rollback, null);
  assert.equal(h.state.editing, false);
  assert.equal(h.draftSql(), 'select 1');
  assert.equal(h.isLocallyEdited(), false);
});

test('same draft refresh retains edits; new draft invalidates edits', () => {
  const h = setup();
  h.adoptTask(task());
  h.state.edits.sql = 'local';
  h.state.editing = true;
  h.adoptTask(task());
  assert.equal(h.draftSql(), 'local');
  assert.equal(h.state.editing, true);
  assert.equal(h.context.fullRenders, 1);
  h.adoptTask(task({ draft: { sql: 'select 2', rollback_sql: '', version: 2 } }));
  assert.equal(h.draftSql(), 'select 2');
  assert.equal(h.state.editing, false);
});

for (const status of ['RUNNING', 'NEEDS_INFO']) {
  test(`${status} polling updates events and evidence without replacing input or SQL`, async () => {
    const h = setup();
    h.adoptTask(task({ status }));
    const input = h.node('question-application');
    input.value = 'unfinished';
    h.document.activeElement = input;
    h.state.edits.sql = 'local SQL';
    h.state.editing = true;
    h.context.nextTask = task({ status, events: [{ kind: 'TOOL', detail: 'new evidence' }], usage: { tokens: 10 } });
    vm.runInContext('api = async () => nextTask; renderEvidence = () => { document.getElementById("evidenceBody").innerHTML = JSON.stringify(state.task.usage); }; renderConversation = () => { throw new Error("must not rebuild questions"); }; renderDraft = () => { throw new Error("must not rebuild SQL"); };', h.context);
    await h.pollOnce();
    assert.match(h.nodes.get('taskTimeline').innerHTML, /new evidence/);
    assert.match(h.nodes.get('evidenceBody').innerHTML, /10/);
    assert.equal(h.document.activeElement, input);
    assert.equal(input.value, 'unfinished');
    assert.equal(input.selectionStart, 1);
    assert.equal(h.draftSql(), 'local SQL');
    assert.equal(h.nodes.get('evidenceBody').scrollTop, 42);
    assert.equal(h.context.fullRenders, 1);
  });
}

test('partial rebuild restores input, selection, focus and ancestor scroll', () => {
  const h = setup();
  const parent = h.node('parent');
  const host = h.node('host', parent);
  let input = h.node('field', host);
  input.value = 'typed answer';
  input.checked = true;
  host.querySelectorAll = () => [input];
  host.contains = (item) => item?.parentElement === host;
  h.document.activeElement = input;
  h.preserveView(host, () => {
    input = h.node('field', host);
    input.selectionStart = 0;
    host.scrollTop = 0;
    parent.scrollTop = 0;
  });
  assert.equal(input.value, 'typed answer');
  assert.equal(input.checked, true);
  assert.equal(input.selectionStart, 1);
  assert.equal(input.selectionEnd, 3);
  assert.equal(h.document.activeElement, input);
  assert.equal(host.scrollTop, 42);
  assert.equal(parent.scrollTop, 42);
});

test('in-flight response cannot restore an old task after switching', async () => {
  const h = setup();
  h.adoptTask(task());
  vm.runInContext('globalThis.pendings = []; api = () => new Promise(resolve => { globalThis.pendings.push(resolve); });', h.context);
  const pending = h.pollOnce();
  h.adoptTask(task({ task_id: 'B' }));
  // 解析所有未决请求（含轨迹与版本），无论顺序：轮询响应都不能覆盖已切换的任务。
  h.context.pendings.forEach((resolve) => resolve(task({ events: [{ detail: 'obsolete' }] })));
  await pending;
   assert.equal(h.state.task.task_id, 'B');
 });

 test('health change refreshes evidence while the task payload stays the same', async () => {
   const h = setup();
   h.adoptTask(task({ status: 'DRAFT_READY' }));
   h.state.health = { status: 'ok', provider: { llm_configured: true } };
   h.nodes.get('evidenceBody').innerHTML = 'old health';
   vm.runInContext('api = async () => ({ status: "degraded", provider: { llm_configured: true }, degraded_reason: "store down" }); renderEvidence = () => { document.getElementById("evidenceBody").innerHTML = state.health.status; }; restoreAgentAvailability = () => {};', h.context);
   h.node('healthDot').className = '';
   h.node('healthText');
   h.node('healthChip');
   h.node('createButton').disabled = false;
   await h.refreshHealth({ quiet: true });
   assert.equal(h.nodes.get('evidenceBody').innerHTML, 'degraded');
   assert.equal(h.context.fullRenders, 1);
 });

 test('server edit posts the expected version and adopts the new version', async () => {
   const h = setup();
   h.node('draftBody');
   h.node('editReason');
   h.adoptTask(task({ status: 'DRAFT_READY' }));
   h.state.edits.sql = 'select 2';
   h.context.editResponse = task({ status: 'DRAFT_READY', draft: { sql: 'select 2', rollback_sql: '', version: 2 } });
   vm.runInContext('globalThis.calls = []; api = async (path, options) => { globalThis.calls.push({ path, options }); return editResponse; };', h.context);
   await h.saveDraft();
   const edit = h.context.calls.find((call) => call.path.endsWith('/draft'));
   assert.equal(edit.path, '/api/agent/tasks/A/draft');
   assert.equal(edit.options.method, 'POST');
   assert.equal(edit.options.body.expected_version, 1);
   assert.equal(edit.options.body.sql, 'select 2');
   assert.equal(h.state.task.draft.version, 2);
   assert.equal(h.state.edits.sql, null);
 });

 test('stale server edit (409) reloads the latest task instead of overwriting', async () => {
   const h = setup();
   h.node('draftBody');
   h.node('editReason');
   h.adoptTask(task({ status: 'DRAFT_READY' }));
   h.state.edits.sql = 'select 9';
   h.context.latest = task({ status: 'DRAFT_READY', draft: { sql: 'server', rollback_sql: '', version: 3 } });
   vm.runInContext('globalThis.calls = []; api = async (path, options) => { globalThis.calls.push(path); if (options && options.method === "POST") { const error = new Error("stale"); error.status = 409; throw error; } return latest; }; handleActionError = () => {};', h.context);
   await h.saveDraft();
   assert.ok(h.context.calls.includes('/api/agent/tasks/A'));
   assert.equal(h.state.task.draft.version, 3);
   assert.equal(h.state.edits.sql, null);
 });

 test('unmodified draft is not re-saved', async () => {
   const h = setup();
   h.adoptTask(task({ status: 'DRAFT_READY' }));
   vm.runInContext('globalThis.calls = 0; api = async () => { globalThis.calls++; return null; };', h.context);
   await h.saveDraft();
   assert.equal(h.context.calls, 0);
 });

 test('trace renders unknown values explicitly and never fabricates durations', () => {
   const h = setup();
   h.state.trace = {
     task_id: 'A', includes_model_reasoning: false, unknown: ['cost_estimate', 'step_duration_ms'],
     steps: [{ index: 1, kind: 'model_call', name: 'generate', status: 'timeout', duration_ms: null,
       usage_known: false, prompt_tokens: null, evidence_ids: [], detail: '', model: 'm' }],
   };
   const html = h.renderTrace();
   assert.match(html, /耗时未知/);
   assert.match(html, /token 未知/);
   assert.match(html, /费用/);
   assert.doesNotMatch(html, />0 ms</);
 });

 test('version history renders snapshots and a diff control', () => {
   const h = setup();
   h.state.versions = [
     { version: 1, origin: 'agent', actor: 'agent', created_at: '2026-09-28T00:00:00+00:00', reason: '', summary: '1 条', check_status: 'PASSED', content_hash: 'aaaa' },
     { version: 2, origin: 'user_edit', actor: 'alice', created_at: '2026-09-28T01:00:00+00:00', reason: '评审修订', summary: '2 条', check_status: 'PASSED', content_hash: 'bbbb' },
   ];
   const html = h.renderVersions(task({ draft: { sql: 'x', rollback_sql: '', version: 2 } }));
   assert.match(html, /人工编辑/);
   assert.match(html, /评审修订/);
   assert.match(html, /data-diff-to="2"/);
   assert.match(html, /data-diff-from="1"/);
 });

 test('linking a change is a server call carrying the change id', async () => {
   const h = setup();
   h.node('draftBody');
   h.adoptTask(task({ status: 'DRAFT_READY' }));
   h.node('changeLinkInput').value = 'chg_1';
   h.context.linkResponse = task({ status: 'DRAFT_READY', change_links: [{ change_request_id: 'chg_1', organization_id: 'o', linked_by: 'alice', linked_at: '2026-09-28T00:00:00+00:00' }] });
   vm.runInContext('globalThis.calls = []; api = async (path, options) => { globalThis.calls.push({ path, options }); return linkResponse; };', h.context);
   await h.linkChange();
   const call = h.context.calls.find((item) => item.path.endsWith('/change-links'));
   assert.equal(call.path, '/api/agent/tasks/A/change-links');
   assert.equal(call.options.body.change_request_id, 'chg_1');
   assert.equal(h.state.task.change_links[0].change_request_id, 'chg_1');
 });

 test('reordered questions keep typed answers and focus by field name', () => {
   const h = setup();
   const host = h.node('conversation');
   const oldInput = h.node('q_application_1');
   oldInput.value = 'order-service';
   oldInput.getAttribute = (name) => name === 'data-field' ? 'application' : null;
   host.querySelectorAll = () => [oldInput];
   host.contains = (item) => item === oldInput || item?.id === 'q_application_0';
   h.document.activeElement = oldInput;
   let next;
   h.preserveView(host, () => {
     next = h.node('q_application_0');
     next.getAttribute = (name) => name === 'data-field' ? 'application' : null;
     host.querySelector = (selector) => selector.includes('application') ? next : null;
   });
   assert.equal(next.value, 'order-service');
   assert.equal(h.document.activeElement, next);
 });

test('snapshot controls and requests only allow backend resumable states, with or without a draft', async () => {
  for (const draft of [null, task().draft]) {
    for (const status of ['NEEDS_INFO', 'FAILED', 'CHECK_BLOCKED', 'RUNNING', 'RECEIVED', 'DRAFT_READY', 'CANCELLED', 'INPUT_REJECTED']) {
      const h = setup();
      h.node('knowledgeListHost');
      h.node('snapshotKnowledgeSelect').value = 'schema-1';
      h.state.task = task({ status, draft });
      h.state.knowledgeList = [{ knowledge_id: 'schema-1', kind: 'schema', status: 'active', title: 'schema' }];
      h.renderKnowledgeList();
      const allowed = ['NEEDS_INFO', 'FAILED', 'CHECK_BLOCKED'].includes(status);
      assert.equal(/data-select-snapshot=/.test(h.nodes.get('knowledgeListHost').innerHTML), allowed, `${status} draft=${Boolean(draft)}`);
      assert.equal(/snapshotApply/.test(h.renderSnapshotCard(h.state.task)), allowed);
      h.context.nextTask = h.state.task;
      vm.runInContext('globalThis.calls = []; api = async (path, options) => { calls.push({ path, options }); return nextTask; };', h.context);
      await h.selectSnapshot('schema-1', false);
      await h.applySnapshotChange();
      assert.equal(h.context.calls.filter(c => c.path.endsWith('/clarify')).length, allowed ? 2 : 0);
    }
  }
});

test('archived/deleted tasks, inactive knowledge and other applications have no selection entry', async () => {
  for (const extra of [{ archived_at: '2030-01-01' }, { deleted_at: '2030-01-01' }, {}]) {
    const h = setup();
    h.node('knowledgeListHost');
    h.state.task = task({ status: 'NEEDS_INFO', slots: { application_id: 'app-a' }, ...extra });
    h.state.knowledgeList = [
      { knowledge_id: 'inactive', kind: 'schema', status: 'deprecated' },
      { knowledge_id: 'norm', kind: 'norms', status: 'active' },
      { knowledge_id: 'other-app', application_id: 'app-b', kind: 'schema', status: 'active' },
    ];
    h.renderKnowledgeList();
    assert.doesNotMatch(h.nodes.get('knowledgeListHost').innerHTML, /data-select-snapshot=/);
    if (extra.archived_at || extra.deleted_at) {
      vm.runInContext('globalThis.calls = []; api = async (...args) => { calls.push(args); };', h.context);
      await h.selectSnapshot('schema-1', false);
      assert.equal(h.context.calls.length, 0);
    }
  }
});

test('late knowledge response cannot replace the newer application/task list', { timeout: 3000 }, async () => {
  const h = setup();
  h.node('knowledgeListHost');
  h.state.task = task({ status: 'NEEDS_INFO', slots: { application_id: 'app-a' } });
  vm.runInContext('globalThis.pendingKnowledge = []; api = path => path.startsWith("/api/agent/knowledge") ? new Promise(resolve => pendingKnowledge.push({ path, resolve })) : Promise.resolve([]);', h.context);
  const oldLoad = h.loadKnowledgeList();
  h.state.task = task({ task_id: 'B', status: 'NEEDS_INFO', slots: { application_id: 'app-b' } });
  const newLoad = h.loadKnowledgeList();
  h.context.pendingKnowledge[1].resolve([{ knowledge_id: 'b', kind: 'schema', status: 'active', title: 'B' }]);
  await newLoad;
  h.context.pendingKnowledge[0].resolve([{ knowledge_id: 'a', kind: 'schema', status: 'active', title: 'A' }]);
  await oldLoad;
  assert.equal(h.state.knowledgeList[0].knowledge_id, 'b');
  assert.equal(h.context.pendingKnowledge[0].path, '/api/agent/knowledge?application_id=app-a');
  assert.equal(h.context.pendingKnowledge[1].path, '/api/agent/knowledge?application_id=app-b');
});

test('same-task application rebinding discards old knowledge response even without a second load', async () => {
  const h = setup();
  h.node('knowledgeListHost');
  h.state.task = task({ slots: { application_id: 'app-a' } });
  vm.runInContext('api = () => new Promise(resolve => { globalThis.resolveKnowledge = resolve; });', h.context);
  const loading = h.loadKnowledgeList();
  h.state.task = task({ slots: { application_id: 'app-b' } });
  h.context.resolveKnowledge([{ knowledge_id: 'a', application_id: 'app-a' }]);
  await loading;
  assert.equal((h.state.knowledgeList || []).length, 0);
});

function formalHarness() {
  const h = setup();
  h.node('changeCreateTitle');
  h.node('changeCreateCard');
  h.node('draftBody');
  h.state.task = task({ status: 'DRAFT_READY', material_hash: '12345678abcdef',
    slots: { application_id: 'app-1' }, application_binding: 'authorized',
    draft: { sql: 'select 1', rollback_sql: '', version: 2, deterministic_check: { status: 'PASSED' } },
    confirmations: [{ material_hash: '12345678abcdef' }] });
  h.context.taskResponse = { ...h.state.task, change_links: [{ change_request_id: 'chg-1' }] };
  vm.runInContext('globalThis.calls = [];', h.context);
  return h;
}

test('formal change uses a stable key across reloads and prevents double clicks', async () => {
  const h = formalHarness();
  const key = h.changeIdempotencyKey(h.state.task);
  const reloaded = formalHarness();
  assert.equal(key, reloaded.changeIdempotencyKey(reloaded.state.task));
  assert.notEqual(key, h.changeIdempotencyKey({ ...h.state.task, draft: { ...h.state.task.draft, version: 3 } }));
  vm.runInContext('api = (path, options) => { calls.push({ path, options }); if (path === "/api/changes") return new Promise(resolve => { globalThis.resolveCreate = resolve; }); return Promise.resolve(path.endsWith("/change-links") ? taskResponse : []); };', h.context);
  const first = h.createFormalChange();
  const second = h.createFormalChange();
  assert.equal(h.context.calls.filter(c => c.path === '/api/changes').length, 1);
  h.context.resolveCreate({ id: 'chg-1', status: 'DRAFT' });
  await Promise.all([first, second]);
  assert.equal(h.context.calls.find(c => c.path === '/api/changes').options.headers['Idempotency-Key'], key);
  assert.equal(h.state.task.change_links[0].change_request_id, 'chg-1');
  assert.equal(h.state.changeRecovery, null);
});

test('created change retries failed linkback without creating a duplicate', async () => {
  const h = formalHarness();
  vm.runInContext('api = async (path, options) => { calls.push({ path, options }); if (path === "/api/changes") return { id: "chg-1" }; if (path.endsWith("/change-links")) { if (calls.filter(c => c.path.endsWith("/change-links")).length === 1) throw new Error("link down"); return taskResponse; } return []; };', h.context);
  await h.createFormalChange();
  assert.equal(h.state.changeRecovery.status, 'linkback_failed');
  await h.retryChangeRecovery();
  assert.equal(h.state.changeRecovery, null);
  assert.equal(h.context.calls.filter(c => c.path === '/api/changes').length, 1);
  assert.equal(h.context.calls.filter(c => c.path.endsWith('/change-links')).length, 2);
});

test('unknown create result retries exactly the original body and key', async () => {
  const h = formalHarness();
  vm.runInContext('api = async (path, options) => { calls.push({ path, options }); if (path === "/api/changes") { if (calls.filter(c => c.path === path).length === 1) throw new Error("timeout"); return { id: "chg-1" }; } return path.endsWith("/change-links") ? taskResponse : []; };', h.context);
  await h.createFormalChange();
  assert.equal(h.state.changeRecovery.status, 'unknown');
  h.nodes.get('changeCreateTitle').value = 'changed after timeout';
  await h.retryChangeRecovery();
  const posts = h.context.calls.filter(c => c.path === '/api/changes');
  assert.equal(posts.length, 2);
  assert.deepEqual(posts[1].options, posts[0].options);
  assert.equal(h.state.changeRecovery, null);
});

test('switching tasks clears recovery and late create success cannot populate another task', async () => {
  const h = formalHarness();
  vm.runInContext('api = (path, options) => { calls.push({ path, options }); return path === "/api/changes" ? new Promise(resolve => { globalThis.resolveCreate = resolve; }) : Promise.resolve([]); };', h.context);
  const creating = h.createFormalChange();
  h.adoptTask(task({ task_id: 'B' }));
  h.context.resolveCreate({ id: 'chg-1' });
  await creating;
  assert.equal(h.state.task.task_id, 'B');
  assert.equal(h.state.changeRecovery, null);
  assert.doesNotMatch(h.nodes.get('errorBanner').innerHTML, /chg-1/);
  assert.equal(h.context.calls.filter(c => c.path.endsWith('/change-links')).length, 0);
});

test('late linkback retry response cannot switch the user back to the old task', async () => {
  const h = formalHarness();
  h.state.changeRecovery = { taskId: 'A', changeId: 'chg-1', key: 'key-a', status: 'linkback_failed' };
  vm.runInContext('api = (path, options) => { calls.push({ path, options }); return path.endsWith("/change-links") ? new Promise(resolve => { globalThis.resolveLink = resolve; }) : Promise.resolve([]); };', h.context);
  const linking = h.retryChangeRecovery();
  h.adoptTask(task({ task_id: 'B' }));
  assert.equal(h.state.changeRecovery, null);
  h.context.resolveLink(h.context.taskResponse);
  await linking;
  assert.equal(h.state.task.task_id, 'B');
  assert.equal(h.state.changeRecovery, null);
});

test('recovery scoped to another task is never sent', async () => {
  const h = formalHarness();
  h.state.changeRecovery = { taskId: 'B', changeId: 'chg-b', key: 'key-b', status: 'linkback_failed' };
  vm.runInContext('api = async (...args) => { calls.push(args); return taskResponse; };', h.context);
  await h.retryChangeRecovery();
  assert.equal(h.context.calls.length, 0);
});

test('late associated change list cannot replace the current task list', async () => {
  const h = formalHarness();
  vm.runInContext('globalThis.pending = []; api = path => new Promise(resolve => pending.push({ path, resolve }));', h.context);
  const first = h.loadAgentChanges();
  h.state.task = task({ task_id: 'B' });
  const second = h.loadAgentChanges();
  h.context.pending[1].resolve([{ id: 'chg-b' }]);
  await second;
  h.context.pending[0].resolve([{ id: 'chg-a' }]);
  await first;
  assert.equal(h.state.agentChanges[0].id, 'chg-b');
});
