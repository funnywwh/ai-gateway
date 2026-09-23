// Node-only regression checks for the request log's reasoning-effort and routing columns.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/js/pages/requests.js', import.meta.url), 'utf8');

// ---------------------------------------------------------------------------
// 推理强度（M20）
// ---------------------------------------------------------------------------
assert.match(source, /key: 'reasoning_effort', label: '推理强度', render: \(row\) => reasoningEffortCell\(row\)/);
assert.ok(source.includes("['推理强度', row.reasoning_effort || '—']"));
const effortStart = source.indexOf('function reasoningEffortCell(row) {');
const effortEnd = source.indexOf('\nfunction modelCell', effortStart);
assert.ok(effortStart >= 0 && effortEnd > effortStart);
const renderEffort = vm.runInNewContext(`${source.slice(effortStart, effortEnd)}\n; reasoningEffortCell`, {
  el: (tag, props) => ({ tag, ...props }),
});
for (const effort of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'future-value']) {
  const cell = renderEffort({ reasoning_effort: effort });
  assert.equal(cell.text, effort);
  assert.match(cell.title, /应用模型策略后/);
}
for (const row of [{}, { reasoning_effort: '' }, { reasoning_effort: null }]) {
  const cell = renderEffort(row);
  assert.equal(cell.text, '—');
  assert.equal(cell.class, 'muted');
}
console.log('Request log reasoning-effort column checks passed.');

// The detail dialog's input panel has to tell three states apart (M82): a body that was kept
// (plain text now), a row whose policy wanted an input but had nothing to keep, and a row
// whose recording was off. The text alone cannot: an empty panel looks the same in the last
// two cases, so the row's recording mode is what decides the heading.
assert.match(source, /panel\(inputPanelTitle\(row\), row\.input\)/);
const titleStart = source.indexOf('function inputPanelTitle(row) {');
const titleEnd = source.indexOf('\nasync function detail', titleStart);
assert.ok(titleStart >= 0 && titleEnd > titleStart);
const inputPanelTitle = vm.runInNewContext(`${source.slice(titleStart, titleEnd)}\n; inputPanelTitle`);

// Kept: plain text under the default policy, a JSON document under "full".
assert.equal(inputPanelTitle({ input_recorded: true, input: 'ping from m23' }), '输入');
assert.equal(
  inputPanelTitle({ input_recorded: true, input: { input: [], model: 'replay' } }),
  '输入',
);
// Nothing qualified, and the row says the policy was the input policy: that is not "not recorded".
assert.equal(
  inputPanelTitle({ input_recorded: false, record_input_mode: 'user' }),
  '输入（未保留：只有样板或超长用户消息）',
);
// Recording off (or metadata-only): the panel really is "not recorded".
assert.equal(inputPanelTitle({ input_recorded: false, record_input_mode: 'off' }), '输入（未录制）');
assert.equal(inputPanelTitle({ input_recorded: false, record_input_mode: 'metadata' }), '输入（未录制）');
assert.equal(inputPanelTitle({ input_recorded: false }), '输入（未录制）');
// A row written between M81 and M82 holds a JSON document whose text was truncated; the old
// wording is still the honest description of what is in it.
assert.equal(
  inputPanelTitle({ input_recorded: true, input: { input_truncated: true, input_max_chars: 100 } }),
  '输入（已截断：每条用户消息只留前 100 字符）',
);
assert.equal(
  inputPanelTitle({ input_recorded: true, input: { input_truncated: true } }),
  '输入（已截断）',
);
console.log('Request log input-panel title checks passed.');

// ---------------------------------------------------------------------------
// 上游模型 / 路由路线（M78）：两列必须排在「供应商」之后，且单元格把「每次尝试一行」的
// 事实讲清楚——失败转移有多跳、没有计量行的请求没有路线、迁移前的行说不知道而不是 0/空白。
// ---------------------------------------------------------------------------
const providerCol = source.indexOf("{ key: 'provider', label: '供应商'");
const upstreamCol = source.indexOf("{ key: 'upstream_model', label: '上游模型'");
const routeCol = source.indexOf("{ key: 'route', label: '路由路线'");
assert.ok(providerCol >= 0 && upstreamCol > providerCol && routeCol > upstreamCol,
  'the two M78 columns must follow the provider column');
// The detail carries the same facts: the identity block names them and the route block lists
// the hops. These are read from the source because the modal is built at click time.
assert.ok(source.includes("['上游模型',") && source.includes("['映射规则',"));
assert.ok(source.includes("el('h4', { text: '路由路线' })"));

const routesStart = source.indexOf('function attemptsOf(row) {');
const routesEnd = source.indexOf('\n// A request with no usage row', routesStart);
assert.ok(routesStart >= 0 && routesEnd > routesStart);
const routesContext = {
  el: (tag, props) => ({ tag, ...props }),
  money: (micros) => (Number(micros) / 1_000_000).toFixed(6) + ' USD',
};
const { upstreamModelsCell, routePathCell } = vm.runInNewContext(
  `${source.slice(routesStart, routesEnd)}\n; ({ upstreamModelsCell, routePathCell })`, routesContext);

// A failed-over request: two hops, the first one rejected upstream. Both columns read the same
// attempts, so the upstream model column lists both names in the order they were tried.
const failover = {
  usage: { metered: true },
  providers: [{ id: 1, name: 'replay-local' }, { id: 3, name: 'deepseek' }],
  attempts: [
    { attempt_no: 1, route_id: 12, provider_id: 1, provider_name: 'replay-local', upstream_model: 'deepseek-chat',
      status: 'failed', error_code: 'upstream_400', terminated_reason: 'upstream_error',
      latency_ms: 300, ttft_ms: 0, cost_micros: 12, charge_micros: 0 },
    { attempt_no: 2, route_id: 13, provider_id: 3, provider_name: 'deepseek', upstream_model: 'deepseek-v3',
      status: 'completed', latency_ms: 900, ttft_ms: 120, cost_micros: 1_234, charge_micros: 2_468 },
  ],
};
const upstream = upstreamModelsCell(failover);
assert.equal(upstream.text, 'deepseek-chat → deepseek-v3');
assert.match(upstream.title, /1\. 路由 #12/);
assert.match(upstream.title, /2\. 路由 #13/);
const route = routePathCell(failover);
assert.equal(route.text, '#12 replay-local ✗ upstream_400 → #13 deepseek ✓');
assert.match(route.title, /1\. 路由 #12 · 供应商 replay-local #1 · 上游模型 deepseek-chat · failed/);
assert.match(route.title, /错误码 upstream_400/);
assert.match(route.title, /成本 0\.000012 USD \/ 对客 0\.000000 USD/);
assert.match(route.title, /路由 id 可在「模型与路由」页对照/);

// A locally rejected request has no metering row: it has no route path at all, and saying
// "未计量" / "无上游尝试" is a different statement from a hop that shows nothing.
const unmetered = { usage: { metered: false }, providers: [], attempts: [] };
assert.equal(upstreamModelsCell(unmetered).text, '未计量');
assert.equal(routePathCell(unmetered).text, '无上游尝试');
assert.match(routePathCell(unmetered).title, /没有到达上游/);
// A row metered before migration 0027 knows no route and no upstream model: unknown, not #0.
const legacy = { usage: { metered: true }, providers: [{ id: 4, name: 'old' }], attempts: [
  { attempt_no: 1, route_id: 0, provider_id: 4, provider_name: 'old', upstream_model: '',
    status: 'completed', latency_ms: 10, ttft_ms: 0, cost_micros: 1, charge_micros: 2 },
] };
assert.equal(upstreamModelsCell(legacy).text, '—');
assert.equal(upstreamModelsCell(legacy).class, 'muted');
assert.match(upstreamModelsCell(legacy).title, /迁移 0027/);
assert.equal(routePathCell(legacy).text, '（未知路由） old ✓');

console.log('Request log reasoning-effort and routing column checks passed.');
