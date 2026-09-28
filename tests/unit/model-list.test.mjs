import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('../../internal/httpapi/web/app.js', import.meta.url), 'utf8');
// Execute the actual binding block, including the preset loop which previously
// accidentally bound the model-list handler once per preset (or never).
const binding = source.slice(source.indexOf('  document.querySelectorAll("[data-llm-preset]")'), source.indexOf('  document.querySelectorAll("[data-toggle-key]")'));
function setup(count, provider = 'openai_compatible') {
  const handlers = [];
  const button = { disabled: false, addEventListener: (_, handler) => handlers.push(handler) };
  const hint = { textContent: '' }, list = { innerHTML: '' };
  let calls = 0, complete;
  const context = { document: {
    querySelectorAll: () => Array.from({ length: count }, () => ({ addEventListener() {} })),
    querySelector: selector => ({ '#llmModelsBtn': button, '#llmModelsHint': hint, '#llmModelList': list })[selector],
  }, readLlmForm: () => ({ base_url: 'https://model.example', api_key: 'test', provider }),
    llmStatus: {}, escapeHTML: s => s, toast() {},
    api: () => { calls++; return new Promise(resolve => { complete = resolve; }); },
  };
  vm.runInNewContext(binding, context);
  return { handlers, button, hint, list, calls: () => calls, complete: value => complete(value) };
}
for (const count of [0, 1, 3]) {
  test(`model list binds once with ${count} presets and prevents concurrent clicks`, async () => {
    const h = setup(count);
    assert.equal(h.handlers.length, 1);
    const first = h.handlers[0]({ currentTarget: h.button });
    await h.handlers[0]({ currentTarget: h.button });
    assert.equal(h.calls(), 1);
    assert.equal(h.button.disabled, true);
    h.complete({ models: [{ id: 'test-model' }] });
    await first;
    assert.equal(h.button.disabled, false);
    assert.match(h.list.innerHTML, /test-model/);
  });
}
test('Anthropic list explains unsupported operation without a request', async () => {
  const h = setup(3, 'anthropic');
  await h.handlers[0]({ currentTarget: h.button });
  assert.equal(h.calls(), 0);
  assert.match(h.hint.textContent, /手动填写/);
});

test('save notification uses real API fields instead of nonexistent configured', () => {
  const start = source.indexOf('        const configured = !!(saved.enabled');
  const end = source.indexOf('        await renderSettings(main);', start);
  assert(start >= 0 && end > start);
  for (const [provider, enabled, has_api_key, expected] of [
    ['openai_compatible', true, true, true],
    ['openai_compatible', false, true, false],
    ['anthropic', true, true, false],
    ['openai_compatible', true, false, false],
  ]) {
    const h = { saved: { provider, enabled, has_api_key }, state: { config: {} }, toast() {} };
    vm.runInNewContext(source.slice(start, end), h);
    assert.equal(h.state.config.llm_configured, expected);
  }
});
