import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('../../internal/httpapi/web/agent/app.js', import.meta.url), 'utf8');
function setup() {
  const nodes = new Map();
  const fields = [];
  const requests = [];
  for (const id of ['requirement', 'optApplication', 'optEnvironment', 'optDatabase', 'optTable', 'optQuerySql', 'optTimezone', 'optSchema', 'optPlannedAt', 'createButton', 'errorBanner', 'clarifyButton', 'clarifyNote']) {
    nodes.set(id, { value: '', addEventListener(type, fn) { this[type] = fn; } });
  }
  const context = vm.createContext({ document: { readyState: 'loading', addEventListener() {}, getElementById: id => nodes.get(id), querySelectorAll: () => fields },
    fetch: async (path, options) => {
      requests.push({ path, ...options, payload: JSON.parse(options.body) });
      return { ok: true, text: async () => JSON.stringify({ task_id: 'T', status: 'NEEDS_INFO' }) };
    } });
  vm.runInContext(source + '\nglobalThis.agent = { plannedTimePayload, formatPlannedDate, browserTimezone, createTask, wireQuestionForm, renderQuestions, state }; adoptTask = () => {};', context);
  return { ...context.agent, nodes, fields, requests };
}

test('wall clock conversion is independent of browser timezone', () => {
  const { plannedTimePayload: convert } = setup();
  assert.equal(convert('2026-09-18T21:30', 'UTC').planned_at, '2026-09-18T21:30:00.000Z');
  assert.equal(convert('2026-09-18T21:30', 'Asia/Shanghai').planned_at, '2026-09-18T13:30:00.000Z');
  assert.equal(convert('2026-09-18T21:30:12.123', '', 'Asia/Shanghai').planned_at, '2026-09-18T13:30:12.123Z');
});

test('invalid dates, times, zones and unsupported years fail closed', () => {
  const { plannedTimePayload: convert } = setup();
  for (const wall of ['2026-02-29T12:00', '2026-04-31T12:00', '2026-13-01T12:00', '2026-01-01T24:00', '2026-01-01T12:60', '2026-01-01T12:00:60', 'nonsense', '1999-01-01T00:00']) {
    assert.throws(() => convert(wall, 'UTC'), /计划/);
  }
  for (const zone of ['Invalid/Zone', '+08:00', '']) assert.throws(() => convert('2026-01-01T12:00', zone), /时区/);
  assert.equal(convert('2024-02-29T00:00', 'UTC').planned_at, '2024-02-29T00:00:00.000Z');
});

test('DST gaps and folds including half-hour and date-line transitions are rejected', () => {
  const { plannedTimePayload: convert } = setup();
  assert.throws(() => convert('2026-03-08T02:30', 'America/New_York'), /不存在/);
  assert.throws(() => convert('2026-11-01T01:30', 'America/New_York'), /出现两次/);
  assert.throws(() => convert('2026-10-04T02:15', 'Australia/Lord_Howe'), /不存在/);
  assert.throws(() => convert('2026-04-05T01:45', 'Australia/Lord_Howe'), /出现两次/);
  assert.throws(() => convert('2011-12-30T12:00', 'Pacific/Apia'), /不存在/);
});

test('draft display uses actual specified zone and tolerates invalid legacy data', () => {
  const { formatPlannedDate: format } = setup();
  assert.match(format('2026-09-18T13:30:00Z', 'Asia/Shanghai'), /21:30:00.*Asia\/Shanghai/);
  assert.match(format('2026-09-18T13:30:00Z', 'UTC'), /13:30:00.*UTC/);
  assert.match(format('2026-09-18T13:30:00Z', 'Invalid/Zone'), /无效/);
  assert.match(format('2026-09-18T13:30:00', 'UTC'), /缺少有效偏移/);
});

test('createTask actual JSON payload carries converted instant and explicit zone', async () => {
  const h = setup();
  h.nodes.get('requirement').value = '准备索引变更';
  h.nodes.get('optPlannedAt').value = '2026-09-18T21:30';
  h.nodes.get('optTimezone').value = 'Asia/Shanghai';
  await h.createTask({ preventDefault() {} });
  assert.equal(h.requests[0].payload.planned_at, '2026-09-18T13:30:00.000Z');
  assert.equal(h.requests[0].payload.planned_at_timezone, 'Asia/Shanghai');
  h.nodes.get('optTimezone').value = '';
  await h.createTask({ preventDefault() {} });
  assert.equal(h.requests[1].payload.planned_at_timezone, h.browserTimezone());
  h.nodes.get('optPlannedAt').value = '2026-02-30T21:30';
  await h.createTask({ preventDefault() {} });
  assert.equal(h.requests.length, 2);
  assert.match(h.nodes.get('errorBanner').innerHTML, /不存在/);
});

for (const override of ['', 'UTC']) {
  test(`clarify actual payload uses ${override || 'task slots timezone'} regardless of field order`, async () => {
    const h = setup();
    h.state.task = { task_id: 'T', slots: { planned_at_timezone: 'Asia/Shanghai' } };
    h.fields.push({ value: '2026-09-18T21:30', getAttribute: () => 'planned_at' });
    h.fields.push({ value: override, getAttribute: () => 'planned_at_timezone' });
    h.wireQuestionForm();
    await h.nodes.get('clarifyButton').click();
    assert.equal(h.requests[0].path, '/api/agent/tasks/T/clarify');
    assert.equal(h.requests[0].payload.planned_at, override ? '2026-09-18T21:30:00.000Z' : '2026-09-18T13:30:00.000Z');
    assert.equal(h.requests[0].payload.planned_at_timezone, override || 'Asia/Shanghai');
  });
}

test('clarify fallback, timezone-only merge contract and error rejection', async () => {
  const h = setup();
  h.state.task = { task_id: 'T', slots: {} };
  h.fields.push({ value: '2026-09-18T21:30', getAttribute: () => 'planned_at' });
  h.wireQuestionForm();
  await h.nodes.get('clarifyButton').click();
  assert.equal(h.requests[0].payload.planned_at_timezone, h.browserTimezone());
  h.fields.splice(0, 1, { value: 'UTC', getAttribute: () => 'planned_at_timezone' });
  await h.nodes.get('clarifyButton').click();
  assert.deepEqual(h.requests[1].payload, { planned_at_timezone: 'UTC' });
  h.fields.push({ value: '2026-11-01T01:30', getAttribute: () => 'planned_at' });
  h.fields[0].value = 'America/New_York';
  await h.nodes.get('clarifyButton').click();
  assert.equal(h.requests.length, 2);
  assert.match(h.nodes.get('errorBanner').innerHTML, /出现两次/);
});

test('time question supplies a timezone control even when backend does not ask for timezone', () => {
  const h = setup();
  const html = h.renderQuestions({ questions: [{ field: 'planned_at' }], slots: { planned_at_timezone: 'UTC' } });
  assert.match(html, /data-field="planned_at_timezone"/);
  assert.match(html, /任务时区 UTC/);
});
