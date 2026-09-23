// Node-only regression checks for the request log reasoning-effort column.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/js/pages/requests.js', import.meta.url), 'utf8');
assert.match(source, /key: 'reasoning_effort', label: '推理强度', render: \(row\) => reasoningEffortCell\(row\)/);
assert.ok(source.includes("['推理强度', row.reasoning_effort || '—']"));
const start = source.indexOf('function reasoningEffortCell(row) {');
const end = source.indexOf('\nfunction modelCell', start);
assert.ok(start >= 0 && end > start);
const render = vm.runInNewContext(`${source.slice(start, end)}\n; reasoningEffortCell`, {
  el: (tag, props) => ({ tag, ...props }),
});
for (const effort of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'future-value']) {
  const cell = render({ reasoning_effort: effort });
  assert.equal(cell.text, effort);
  assert.match(cell.title, /应用模型策略后/);
}
for (const row of [{}, { reasoning_effort: '' }, { reasoning_effort: null }]) {
  const cell = render(row);
  assert.equal(cell.text, '—');
  assert.equal(cell.class, 'muted');
}
console.log('Request log reasoning-effort column checks passed.');

// The detail dialog's input panel must say when the record is only the head of a message
// (M81): a question cut at recording.input_max_chars looks exactly like a short question,
// and the operator would read a truncated one as the whole thing.
assert.match(source, /panel\(inputPanelTitle\(row\), row\.input\)/);
const titleStart = source.indexOf('function inputPanelTitle(row) {');
const titleEnd = source.indexOf('\nasync function detail', titleStart);
assert.ok(titleStart >= 0 && titleEnd > titleStart);
const inputPanelTitle = vm.runInNewContext(`${source.slice(titleStart, titleEnd)}\n; inputPanelTitle`);

assert.equal(inputPanelTitle({ input_recorded: false }), '输入（未录制）');
assert.equal(inputPanelTitle({ input_recorded: true }), '输入');
assert.equal(
  inputPanelTitle({ input_recorded: true, input: { input: [], omitted: { tools: 1 } } }),
  '输入',
);
assert.equal(
  inputPanelTitle({ input_recorded: true, input: { input_truncated: true, input_max_chars: 100 } }),
  '输入（已截断：每条用户消息只留前 100 字符）',
);
// A row written by a build that capped the text but did not record the number still has to
// admit the truncation rather than claim to be the whole question.
assert.equal(
  inputPanelTitle({ input_recorded: true, input: { input_truncated: true } }),
  '输入（已截断）',
);
console.log('Request log input-panel title checks passed.');
