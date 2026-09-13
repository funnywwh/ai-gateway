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
