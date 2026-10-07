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

// ---------------------------------------------------------------------------
// 整行可点开详情（M91）：点击面从「最右边那个按钮」扩到整行。
//
// 行本身由 ui.js 的 table() 造（页面拿不到 <tr>），所以这一页只需证明三件事：整行与「详情」
// 按钮走**同一个入口**、入口把失败收在自己身上、这一页确实开了那个入口；两条守卫（行内控件、
// 拖选文本）在 ui.js 里，一并在这里钉住——它们没有别的 Node 侧落点。
// ---------------------------------------------------------------------------
assert.match(source, /onRowClick: \(row\) => openDetail\(row\)/,
  '列表必须把整行接进 openDetail：M91 之后行本身就是入口');
assert.match(source, /class: 'btn', text: '详情',\s*\n\s*onclick: \(\) => openDetail\(row\)/,
  '「详情」按钮与整行必须同一个入口：键盘路径不能走另一条分支');
const openStart = source.indexOf('function openDetail(row) {');
const openEnd = source.indexOf('\nasync function detail', openStart);
assert.ok(openStart >= 0 && openEnd > openStart, 'openDetail 必须存在');
const openDetailSource = source.slice(openStart, openEnd);
assert.match(openDetailSource, /if \(detailPending\) return;/,
  'pending 期间第二次点击必须被丢掉：弹窗要等接口回来才挂进 DOM，双击会开两个');
assert.match(openDetailSource, /\.catch\(\(err\) => toast\(api\.errorMessage\(err\), 'error'\)\)/,
  '失败必须在入口处变成屏幕上的 toast——未捕获的 promise 只会进控制台');
assert.match(openDetailSource, /detail\(row\.request_id\)/, 'openDetail 才是真正去取详情的那一处');
// 全文件只允许有一次取详情：第二次出现就意味着有一条绕过 openDetail 的路径，失败处理会分叉。
assert.equal((source.match(/detail\(row\.request_id\)/g) || []).length, 1,
  '不允许存在绕过 openDetail 的第二条路径（失败处理会因此分叉）');

// The client filter must offer every value the column can hold: an operator who can see
// `uya-agent` in the table but cannot filter by it has no way to isolate its consumption.
// Read off the real option list rather than grepping, so a renamed label cannot pass.
const clientSelectStart = source.indexOf("const client = el('select'");
const clientSelectEnd = source.indexOf(']);', clientSelectStart);
assert.ok(clientSelectStart >= 0 && clientSelectEnd > clientSelectStart,
  '找不到客户端筛选下拉');
const clientSelectSource = source.slice(clientSelectStart, clientSelectEnd);
const clientValues = [...clientSelectSource.matchAll(/value: '([^']*)'/g)].map((m) => m[1]);
for (const value of ['', 'dsh', 'uya-agent', 'codex', 'console', 'unknown']) {
  assert.ok(clientValues.includes(value),
    `客户端下拉缺少取值 ${JSON.stringify(value)}（现有：${clientValues.join(', ')}）`);
}
// …and the column must not be reduced to a known-value lookup that drops new ones: it
// renders whatever the row carries, which is what makes adding a value a one-line change.
assert.match(source, /render: \(row\) => \(row\.client \? badge\(row\.client/,
  '客户端列必须原样渲染行里的值，不许按已知取值表映射（否则新增取值会显示不出来）');

const uiSource = await readFile(new URL('../static/js/ui.js', import.meta.url), 'utf8');
assert.match(uiSource, /const ROW_CLICK_OWNERS = 'button, a, input, select, textarea, label, summary, \[role="button"\], \[role="link"\]'/,
  '行内控件自己吃掉点击：否则「详情」按钮的这一次会再冒泡成一次行点击');
assert.match(uiSource, /target\.closest\(ROW_CLICK_OWNERS\)/, '行点击必须放过落在控件上的那一次');
assert.match(uiSource, /String\(window\.getSelection\(\)\)\.trim\(\)/,
  '拖选文本（复制请求 id）之后的那次点击不能打开弹窗');
assert.match(uiSource, /export function table\(\{[^}]*onRowClick[^}]*\}\)/,
  'table() 的 onRowClick 必须是可选参数：不传的列表 DOM 不变');
assert.match(uiSource, /export function pagedTable\(\{[^}]*onRowClick[^}]*\}\)/,
  'pagedTable() 要把 onRowClick 透传给 table()');
console.log('Request log row-click detail checks passed.');

// ---------------------------------------------------------------------------
// 时间窗口（M97）：当天 / 本周 / 本月 / 时间段，边界按**本机时区**算。
//
// 这一层钉的是「哪个时刻」——最难的也正是它：一个把本地 00:00 当成 UTC 00:00 的实现，在 UTC
// 主机上跑起来与正确实现**完全一样**，只有在偏移非零的时区里才露馅，而线上操作员基本都不在
// UTC。所以 Makefile 给这一行显式设了 TZ（见 ui-base），下面的断言也同时钉住"偏移被算进去了"：
// 本地 00:00 的 UTC 时刻必须等于当天 0 点减去本机偏移，而不是 'T00:00:00.000Z'。
//
// 另一半是**右端**：日历窗口只发 from，让服务端定「现在」；时间段两端都发，结束日是 23:59:59。
// ---------------------------------------------------------------------------
const windowStart = source.indexOf('const pad2 =');
const windowEnd = source.indexOf('\n// A request with no usage row', windowStart);
assert.ok(windowStart >= 0, '窗口的纯函数必须存在（M97）');
const windowSource = source.slice(windowStart, windowEnd > windowStart ? windowEnd : source.length);
const localZone = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
if ((new Date(2026, 0, 1).getTimezoneOffset()) === 0) {
  throw new Error('this node test needs a non-UTC TZ to mean anything; the Makefile sets one');
}
const windowContext = vm.runInNewContext(
  `${windowSource}\n; ({ windowParamsOf, windowLabelOf, shortRange })`, { Intl, Date, String, Number });
// 参数对象是另一个 vm 上下文里造的，原型不同，deepStrictEqual 会因此失败——比字段而不是比原型。
const params = (kind, now, range) => JSON.parse(JSON.stringify(windowContext.windowParamsOf(kind, now, range)));
const { windowLabelOf } = windowContext;
const { shortRange } = windowContext;

const offsetHours = -new Date(2026, 9, 7).getTimezoneOffset() / 60;
const offsetStamp = (y, mo, d, h, mi, s) => {
  const local = new Date(y, mo, d, h, mi, s);
  return new Date(local.getTime()).toISOString();
};
// A Wednesday at 09:36 local; this week's Monday is the 5th, the month started on the 1st.
const at = new Date(2026, 9, 7, 9, 36, 49);

// 当天 / 本周 / 本月：**只带 from**，值是本地 00:00（不是 UTC 00:00），右端不出现。
const today = params('today', at);
assert.deepEqual(Object.keys(today), ['from'], '当天只发 from：右端是服务端的「现在」');
assert.equal(today.from, offsetStamp(2026, 9, 7, 0, 0, 0), '当天必须是本机时区的 00:00');
if (offsetHours !== 0) {
  assert.notEqual(today.from, '2026-10-07T00:00:00.000Z', '本地 00:00 不是 UTC 00:00：偏移必须算进去');
}
assert.equal(params('week', at).from, offsetStamp(2026, 9, 5, 0, 0, 0), '本周从周一开始');
assert.equal(params('month', at).from, offsetStamp(2026, 9, 1, 0, 0, 0), '本月从 1 日开始');
assert.deepEqual(Object.keys(params('month', at)), ['from']);
// 周日属于**上一周**：若按 getDay() 直接减，周日上午的「本周」会缩成当天。
const sunday = new Date(2026, 9, 11, 9, 0, 0);
assert.equal(params('week', sunday).from, offsetStamp(2026, 9, 5, 0, 0, 0),
  '周日看「本周」仍然是本周一（不是当天）');
// 跨月/跨年：1 月 1 日的「本周」落在去年 12 月。
assert.equal(params('week', new Date(2027, 0, 1, 9, 0, 0)).from, offsetStamp(2026, 11, 28, 0, 0, 0),
  '本周可以跨年：边界按日历算，不按"最近 N 天"算');

// 滚动窗口：days 原样透传，且**不带** from（两条互斥的表达方式，服务端按 from 优先）。
assert.deepEqual(params('d7', at), { days: '7' });
assert.deepEqual(params('d1', at), { days: '1' });
assert.deepEqual(params('d30', at), { days: '30' });

// 时间段：两端都发，起始 00:00:00、结束 23:59:59（闭区间，含结束日最后一秒）。
const custom = params('custom', at, { from: '2026-10-01', to: '2026-10-07' });
assert.deepEqual(custom, {
  from: offsetStamp(2026, 9, 1, 0, 0, 0),
  to: offsetStamp(2026, 9, 7, 23, 59, 59),
}, '时间段必须是本机时区的整天闭区间');
assert.equal(shortRange({ from: '2026-10-01', to: '2026-10-07' }), '10-01 ~ 10-07');
// 区间只给了一半（或解析不动）时不能瞎猜：退回默认的滚动窗口，而不是发一个假的边界。
assert.deepEqual(params('custom', at, null), { days: '7' });
assert.deepEqual(params('custom', at, { from: '2026-10-01' }), { days: '7' });
assert.deepEqual(params('custom', at, { from: 'x', to: '2026-10-07' }), { days: '7' });
// 未知取值不产生任何 from/to（服务端会把 days=undefined 忽略，但不能发出半个窗口）。
assert.deepEqual(params('nonsense', at), { days: '7' });

// 提示语：边界必须**写在屏幕上**（只有"当天"三个字时，00:00 到底按谁的时区无人能核对）。
assert.match(windowLabelOf('today', null, at), /窗口：2026-10-07 00:00:00 ~ 现在/);
assert.match(windowLabelOf('week', null, at), /窗口：2026-10-05 00:00:00（本周一） ~ 现在/);
assert.match(windowLabelOf('month', null, at), /窗口：2026-10-01 00:00:00（本月 1 日） ~ 现在/);
assert.match(windowLabelOf('custom', { from: '2026-10-01', to: '2026-10-07' }, at),
  /窗口：2026-10-01 00:00:00 ~ 2026-10-07 23:59:59/);
assert.match(windowLabelOf('d7', null, at), /最近 7 天/);
// 提示语的时区限定只在**确实**按本地时间算的窗口上出现：滚动窗口是绝对的 N×24 小时，
// 给它标一个时区名是把噪声当信息，也会让「本地时间」这四个字在别处变得不可信。
const windowUsesLocalTime = vm.runInNewContext(
  `${windowSource}\n; windowUsesLocalTime`, { Intl, Date, String, Number });
for (const kind of ['today', 'week', 'month', 'custom']) {
  assert.equal(windowUsesLocalTime(kind), true, kind + ' 的边界按本地时间算');
}
for (const kind of ['d1', 'd3', 'd7', 'd30']) {
  assert.equal(windowUsesLocalTime(kind), false, kind + ' 是滚动窗口：没有时区可标');
}

// 源码接线：下拉必须有这四个新选项；窗口参数必须经 filterParams（唯一出口）到达三处调用，
// 而 loadModelOptions 不能再直读下拉的 value——那样会把 days=today 发给只认整数的服务端，
// 模型下拉会静默退回默认 7 天。
assert.match(source, /\['today', '当天'\]/, '下拉必须有「当天」');
assert.match(source, /\['week', '本周（周一起）'\]/, '下拉必须有「本周」');
assert.match(source, /\['month', '本月'\]/, '下拉必须有「本月」');
assert.match(source, /\['custom', '时间段…'\]/, '下拉必须有「时间段…」');
for (const n of [1, 3, 7, 30]) {
  assert.match(source, new RegExp(`\\['d${n}', '最近 ${n} 天'\\]`), `滚动窗口最近 ${n} 天必须保留`);
}
assert.match(source, /windowParamsOf\(windowSelect\.value, new Date\(\), windowRange\)/,
  'filterParams 必须有窗口的唯一出口');
assert.match(source, /api\.get\('\/requests\/dimensions', \{ \.\.\.windowParams\(\), group_by: 'model', limit: 50 \}\)/,
  '模型下拉的窗口必须走 windowParams：直读下拉 value 会发出 days=today');
assert.doesNotMatch(source, /days\.value/,
  '窗口下拉的值不许被直接当 days 用：M97 起它的取值是语义名');
assert.match(source, /filterParams\(\), group_by: groupBy\.value, sort: sortBy\.value/,
  '统计卡与列表必须共用同一个窗口');
console.log(`Request log time-window checks passed (TZ=${localZone}, offset=${offsetHours}h).`);
