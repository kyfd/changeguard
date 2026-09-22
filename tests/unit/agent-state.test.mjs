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
  for (const id of ['draftMeta', 'taskTimeline', 'evidenceBody', 'conversation', 'taskConfirmation']) node(id);
  const context = vm.createContext({
    document,
    setInterval: () => 1,
    clearInterval() {},
    $: (id) => document.getElementById(id),
    CSS: { escape: (value) => String(value).replace(/[^a-zA-Z0-9_-]/g, '\\$&') },
  });
  vm.runInContext(source + '\n globalThis.agent = { state, adoptTask, pollOnce, preserveView, draftSql, isLocallyEdited, refreshHealth };', context);
  vm.runInContext('render = () => { globalThis.fullRenders = (globalThis.fullRenders || 0) + 1; };', context);
  return { ...context.agent, context, nodes, document, node };
}
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
  vm.runInContext('api = () => new Promise(resolve => { globalThis.resolvePoll = resolve; });', h.context);
  const pending = h.pollOnce();
  h.adoptTask(task({ task_id: 'B' }));
  h.context.resolvePoll(task({ events: [{ detail: 'obsolete' }] }));
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
