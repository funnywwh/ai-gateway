// 视图 `csp`：控制台**真实策略**之下的行内样式。
//
// 这个视图存在的唯一理由：harness 以前不发 CSP。server.py 只发 Cache-Control，于是
// tree.page.html 的 `workspaceIndentShiftsLabels` 这类几何断言跑在"没有策略"的世界里，
// 而线上发的是 `style-src 'self'`——行内 style **属性**归 style-src-attr 管（回落到
// style-src），没有 'unsafe-inline' 就被浏览器整条丢弃：DOM 里 row 上写着
// `style="padding-left:52px"`，`getComputedStyle(row).paddingLeft` 却是 `0px`。
// 于是"走查全绿 + 线上零缩进"可以同时成立，而且看起来完全不像一处 bug（现场反馈就是
// 「为什么这个组织树没有缩进？」）。
//
// 这里钉两件事，缺一个就会退化成"看着在查、其实查不到"：
//
//   ① **自检**：这一页真的跑在那条策略下。判据是"属性写法必须被丢弃"——如果哪天有人把
//      'unsafe-inline' 加进控制台策略，这一条会立刻变红。那是个要重新评估的决定
//      （注入面一起变宽），不该由这个视图默默变绿来掩盖。
//   ② **几何**：修好之后 el() 的行内样式真的落到**计算值**上：三层缩进 8/30/52px、每层
//      22px、标签的 x 按层右移，另有一条独立的弹窗宽度（`.modal` 的 CSS 是 680px，行内把它
//      放到 900px）——它证明修的是 el() 这条通路，而不是"恰好把这棵树的缩进补回来了"。
//
// 反面判据同时留着：`noZeroIndent` 正是事故现场的样子（属性有、样式没生效）。
import { tree } from '/js/tree.js';
import { el } from '/js/ui.js';

// 必须与 tree.js 的 INDENT 相等。org_tree_test.mjs 会把这里的 "=== 22" 与 tree.js 的常量钉在一起
// （改一个不改另一个，走查会报红——这正是那次 14→22 改动漏掉的东西之一）。
const INDENT = 22;

const view = (location.hash || '#csp').slice(1);
const errors = [];
window.onerror = (msg) => errors.push('onerror: ' + msg);
window.onunhandledrejection = (ev) => errors.push('rejection: ' + ev.reason);
const settle = async () => { for (let i = 0; i < 60; i++) await Promise.resolve(); };

// 与其它 harness 页同一套回报方式：结果发给 runner（`/report?data=`），而不是留在页面里。
function report(data) {
  data.toasts = [...document.querySelectorAll('.toast')].map((n) => n.textContent);
  const url = '/report?data=' + encodeURIComponent(JSON.stringify(data));
  try { if (navigator.sendBeacon && navigator.sendBeacon(url)) return; } catch (err) { /* fall through */ }
  try {
    const xhr = new XMLHttpRequest();
    xhr.open('GET', url, false);
    xhr.send();
  } catch (err) { /* reporting must never break the harness */ }
}

const checks = {};
try {
  const page = document.getElementById('page');

  // ① 自检：策略生效的证据是"属性写法被丢弃"。这条不成立，后面所有几何断言都没有意义。
  const probe = document.createElement('div');
  probe.setAttribute('style', 'padding-left:99px');
  page.append(probe);
  await settle();
  checks.styleAttributeIsBlocked = getComputedStyle(probe).paddingLeft === '0px';

  // ② 三层节点：缩进必须落在计算值上（属性里那段声明还在，但生效与否只能看计算值）。
  const nodes = [
    { id: 1, parent_id: null, name: '总部', account_count: 5 },
    { id: 2, parent_id: 1, name: '研发部', account_count: 3 },
    { id: 3, parent_id: 2, name: '平台组', account_count: 1 },
  ];
  const t = tree({
    mode: 'workspace',
    filter: false,
    renderLabel: (node) => node.name,
    renderMeta: (node) => node.account_count + ' 个账号',
  });
  page.append(t.node);
  t.refresh(nodes);
  await settle();

  const rows = [...t.node.querySelectorAll('.tree-row')];
  checks.rowsRendered = rows.length === 3;
  const computed = rows.map((row) => getComputedStyle(row).paddingLeft);
  const labelLeft = rows.map((row) => Math.round(row.querySelector('.tree-label').getBoundingClientRect().left));
  checks.attributeCarriesIndent = /padding-left:\s*8px/.test(rows[0].getAttribute('style') || '')
    && /padding-left:\s*30px/.test(rows[1].getAttribute('style') || '');
  checks.computedIndentBase = computed[0] === '8px';
  checks.computedIndentPerLevel = computed[1] === (8 + INDENT) + 'px' && computed[2] === (8 + 2 * INDENT) + 'px';
  checks.indentPerLevelIs22 = INDENT === 22;
  // 几何必须与计算值一致：只断样式挡不住"另一条 CSS 把行内样式顶掉"，那种情况下屏幕上是平的。
  checks.labelsShiftPerLevel = labelLeft[1] - labelLeft[0] === INDENT && labelLeft[2] - labelLeft[1] === INDENT;
  // 事故现场的样子：属性在、计算值全是 0px（每层贴着左边缘）。
  checks.noZeroIndent = computed.every((value) => value !== '0px');

  // ③ el() 的 style 键不止这棵树在用：弹窗宽度写在同一个地方。
  const modal = el('div', { class: 'modal', style: 'width:min(900px,100%)' });
  page.append(modal);
  await settle();
  // 期望值是"CSS 的 680px 被行内抬到 900px"：容器比 900px 窄时按容器算（那种部署下这一条
  // 依然有意义——它区分的是"行内生效"与"回落到 CSS 的 680px"）。
  const want = Math.min(900, page.clientWidth);
  checks.inlineWidthApplies = Math.abs(modal.getBoundingClientRect().width - want) <= 1;

  // ok 是本视图自己的判定；runner 另外把任何 false 项也当作失败，两者不会朝"假绿"的方向分歧。
  const bad = Object.keys(checks).filter((key) => checks[key] === false);
  report({ view, stage: 'done', ok: bad.length === 0, failed: bad, checks, errors });
} catch (err) {
  // 抛出即失败：报出去就停，第二个 "done" 会把诊断覆盖成半份的。
  report({ view, stage: 'threw', checks, thrown: String(err && err.stack ? err.stack : err), errors });
}
