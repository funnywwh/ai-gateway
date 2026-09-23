// Node-only regression test for the 「DSH 节点」 page (M77). It loads pages/dshgw_nodes.js as an ES
// module with the UI and API dependencies mocked, so what it pins is the wiring an operator depends
// on — the parts a page render alone would not prove:
//
//   * the list is cheap by default and the probe is an explicit action (`probe=true` is only asked
//     for when somebody clicks 探测) — a page that probed on every render would turn the console's
//     own refresh into the fleet's load;
//   * a deploy is asynchronous and the first attempt is *refused* by design: the page must turn that
//     refusal into a fingerprint confirmation, then retry with the fingerprint;
//   * the deploy drawer reads the progress endpoint, shows the phases and the log tail, and stops
//     polling when it is closed (no timer left running behind a closed modal);
//   * a purge cannot happen by accident: it needs the node's name typed again;
//   * a placement change says, on screen, that it does not move data;
//   * a read-only session gets no action buttons at all — not buttons that fail with 403.
//
// The rendering itself is exercised in a real browser by scripts/ui-harness (views `nodes` /
// `nodes-readonly`), which is the only place the CSS and the real DOM are in play.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/dshgw_nodes.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');

const calls = [];
const responses = new Map();
const toasts = [];
const modals = [];

// The DOM stand-in mirrors ui.js's `el(tag, attrs, children)` contract for what this page uses.
function node(tag, attrs = {}, children = []) {
  const el = {
    tag, children: [], listeners: {}, attributes: {},
    className: attrs.class || '',
    textContent: attrs.text || '',
    value: attrs.value === undefined ? '' : attrs.value,
    disabled: !!attrs.disabled,
    placeholder: attrs.placeholder || '',
    append(...items) { el.children.push(...items.flat().filter(Boolean)); },
    replaceChildren(...items) { el.children = items.flat().filter(Boolean); },
    setAttribute(name, value) { el.attributes[name] = value; },
    addEventListener(name, listener) { (el.listeners[name] = el.listeners[name] || []).push(listener); },
    fire(name, ev) { for (const listener of el.listeners[name] || []) listener(ev || { currentTarget: el, target: el }); },
    click() { el.fire('click', { currentTarget: el, target: el }); },
    remove() { el.removed = true; },
    querySelector() { return null; },
  };
  // ui.js turns attributes named `onclick…` into listeners; the stub must too, or a click in this
  // test would silently do nothing and "the button exists" would be mistaken for "the button works".
  for (const [key, value] of Object.entries(attrs || {})) {
    if (typeof value === 'function' && key.startsWith('on')) {
      (el.listeners[key.slice(2)] = el.listeners[key.slice(2)] || []).push(value);
    }
  }
  el.append(children || []);
  return el;
}

function walk(entry, out = []) {
  if (!entry) return out;
  out.push(entry);
  for (const child of entry.children || []) walk(child, out);
  return out;
}

function textsOf(entry) {
  return walk(entry).map((child) => child.textContent).filter(Boolean).join(' ');
}

function buttonOf(entry, label) {
  return walk(entry).find((child) => child.tag === 'button' && child.textContent === label) || null;
}

// rowButtonOf reaches a button inside the row whose text names a node: the tables hold several rows
// with the same action labels, and clicking "the first 删除" is how a test ends up asserting on the
// wrong machine.
function rowButtonOf(page, rowText, label) {
  // Every row of every table on the page: the node table and the tenant table both hold actions,
  // and the helper must not silently look at only the first of them.
  const row = walk(page).filter((child) => child.tag === 'tr').find((tr) => textsOf(tr).includes(rowText));
  return row ? buttonOf(row, label) : null;
}

function findByText(entry, needle) {
  return walk(entry).find((child) => typeof child.textContent === 'string' && child.textContent.includes(needle)) || null;
}

const api = {
  async get(path, params) {
    calls.push({ method: 'GET', path, params });
    const handler = responses.get('GET ' + path);
    if (!handler) throw new Error('unexpected GET ' + path);
    return handler(params);
  },
  async post(path, body) {
    calls.push({ method: 'POST', path, body });
    const handler = responses.get('POST ' + path);
    if (!handler) throw new Error('unexpected POST ' + path);
    return handler(body);
  },
  async patch(path, body) {
    calls.push({ method: 'PATCH', path, body });
    const handler = responses.get('PATCH ' + path);
    if (!handler) throw new Error('unexpected PATCH ' + path);
    return handler(body);
  },
  async del(path) {
    calls.push({ method: 'DELETE', path });
    const handler = responses.get('DELETE ' + path);
    if (!handler) throw new Error('unexpected DELETE ' + path);
    return handler();
  },
  errorMessage: (err) => (err && err.message) || String(err),
};

const timers = [];
const ui = {
  el: node,
  card: (title, children, actions) => node('section', { class: 'card' },
    [node('h2', { text: title })].concat(children || []).concat(actions || [])),
  badge: (text, kind) => node('span', { class: 'badge ' + (kind || ''), text }),
  toast: (message, kind) => { toasts.push({ message, kind }); },
  formatTime: (value) => 'T(' + value + ')',
  jsonBlock: (value) => node('pre', { text: JSON.stringify(value) }),
  table: ({ columns, rows, rowActions, empty }) => {
    const body = node('tbody');
    if ((rows || []).length === 0) body.append(node('tr', { text: empty || '' }));
    for (const row of rows || []) {
      const tr = node('tr');
      for (const column of columns) tr.append(node('td', {}, [column.render ? column.render(row) : node('span', { text: String(row[column.key] || '') })]));
      if (rowActions) tr.append(node('td', {}, rowActions(row)));
      body.append(tr);
    }
    const table = node('table', {}, [body]);
    return { node: table, body };
  },
  confirmDialog: async (title, message) => {
    calls.push({ method: 'CONFIRM', path: title, body: message });
    modals.push({ title, message });
    return true;
  },
  modalHead: (title) => node('div', { text: title }),
  modalBody: (children) => node('div', {}, children),
  modalActions: (children) => node('div', { class: 'modal-actions' }, children),
  // modal() is the framework's form dialog; the page uses it for registration, editing and the
  // purge confirmation, so the stub records the fields and can submit them.
  modal: ({ title, fields, submitLabel, onSubmit }) => {
    const box = node('div', { class: 'modal' }, fields.map((field) => node('label', { text: field.label })));
    modals.push({ title, box, fields, submitLabel, onSubmit });
    return Promise.resolve(null);
  },
};

const modalRoot = node('div');
const document = {
  createElement: (tag) => node(tag),
  getElementById: (id) => (id === 'modal-root' ? modalRoot : null),
};
// customDialog appends straight to #modal-root; the test reads it back through this list.
modalRoot.append = (...items) => { modalRoot.children.push(...items.flat().filter(Boolean)); };

const context = vm.createContext({
  console, Error, Promise, Date, JSON, Number, String, Object, Array, Boolean, RegExp, setTimeout, clearTimeout,
  document,
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
const linker = async (specifier) => {
  if (specifier === '../api.js') {
    return new vm.SyntheticModule(['api'], function () { this.setExport('api', api); }, { context });
  }
  if (specifier === '../ui.js') {
    return new vm.SyntheticModule(
      ['el', 'card', 'badge', 'toast', 'modal', 'confirmDialog', 'formatTime', 'table', 'modalHead', 'modalBody', 'modalActions', 'jsonBlock'],
      function () { for (const [name, value] of Object.entries(ui)) this.setExport(name, value); },
      { context });
  }
  throw new Error('unexpected import ' + specifier);
};
await module.link(linker);
await module.evaluate();

const { render, stateInfo, featureSummary, deployPhaseRows, supervisionLabel } = module.namespace;
const settle = async () => { for (let i = 0; i < 200; i++) await Promise.resolve(); };

// --- fixtures ---------------------------------------------------------------------------------

const NODES = [
  {
    name: 'node-a', source: 'console', url: 'http://10.0.0.1:18400', ssh_host: '10.0.0.1', ssh_user: 'winger',
    default: true, state: 'ready', revision: 'abcdef1234567', features: { tenant_plugins: ['web-tty'], host_shares: 1 },
    tenants: 2, running: 1, reachable: true, probed_at: '2026-09-23T02:00:00Z', token_state: 'set',
  },
  {
    name: 'node-b', source: 'config', url: 'http://10.0.0.2:18400', ssh_host: '10.0.0.2', ssh_user: 'dshgw',
    state: 'unreachable', reachable: false, token_state: 'set',
  },
];
const TENANTS = [
  { name: 'alice', account: 'Alice', node: 'node-a', running: true, public_port: 32601, worker_port: 32900, handshake: 'ok', tenant_url: 'https://dshgw.local:32601/' },
  { name: 'bob', account: 'Bob', node: 'node-b', running: false, suspended: true, public_port: 32602, worker_port: 32901 },
];

const FINGERPRINT = 'SHA256:TG5FPCWIAAQEtNNBi7nTG7iNFrQUGgyQQDxRnuuIOgE';
let listProbes = [];
let deployBodies = [];
let deployPolls = 0;
let deployRunning = true;

function reset() {
  calls.length = 0;
  toasts.length = 0;
  modals.length = 0;
  modalRoot.children = [];
  listProbes = [];
  deployBodies = [];
  deployPolls = 0;
  // 默认"任务已结束"：只有专门验证"进行中"外观的那一节才把它打开。否则任何一节留下的
  // 轮询链都会在测试结束后继续跑（这正是本节末尾那条断言要抓的泄漏）。
  deployRunning = false;
  responses.clear();
  responses.set('GET /dshgw/nodes', (params) => {
    listProbes.push(Boolean(params && params.probe === 'true'));
    return { nodes: NODES, probing: params && params.probe === 'true' };
  });
  responses.set('GET /dshgw/tenants', () => ({ tenants: TENANTS }));
  responses.set('POST /dshgw/nodes/node-a/deploy', (body) => {
    deployBodies.push(body);
    if (!body.accept_host_key) {
      // What the daemon really answers on a first deploy: a refusal carrying the fingerprint.
      throw new Error('preflight: ' + FINGERPRINT + ' has host key; confirm it with --accept-host-key');
    }
    return { deploy: { node: 'node-a', running: true, state: 'deploying', phase: 'preflight' } };
  });
  responses.set('GET /dshgw/nodes/node-a/deploy', () => {
    deployPolls += 1;
    const phases = [
      { name: 'preflight', ok: true, millis: 10 },
      { name: 'upload', ok: true, millis: 20 },
    ];
    if (deployRunning) {
      return { deploy: { node: 'node-a', running: true, state: 'deploying', phase: 'upload', phases, log_tail: '[upload] out: upload-ok\n' } };
    }
    return {
      deploy: {
        node: 'node-a', running: false, state: 'ready', systemd: true, phases,
        log_tail: '[upload] out: upload-ok\n[verify] out: the node answers\n',
      },
    };
  });
  responses.set('POST /dshgw/nodes/node-a/probe', () => ({ node: { name: 'node-a', reachable: true }, reachable: true }));
  responses.set('POST /dshgw/nodes/node-a/reconcile', () => ({ result: { started: ['alice'], stopped: [], pruned: ['ghost'] } }));
  responses.set('POST /dshgw/nodes/node-a/rotate-token', (body) => {
    deployBodies.push(Object.assign({ rotate_token: true }, body));
    return { deploy: { node: 'node-a', running: true, state: 'deploying', rotated_token: true } };
  });
  responses.set('GET /dshgw/nodes/node-a/audit', () => ({ lines: ['{"kind":"ssh-mount-refused"}', 'not json'], count: 2 }));
  responses.set('GET /dshgw/nodes/node-b/audit', () => ({ lines: [], count: 0 }));
  responses.set('DELETE /dshgw/nodes/node-b', () => ({ removed: 'node-b' }));
  responses.set('DELETE /dshgw/nodes/node-b?purge=true&confirm=node-b', () => ({ removed: 'node-b', purged: true }));
  responses.set('PATCH /dshgw/nodes/node-a', () => ({ node: { name: 'node-a' } }));
  responses.set('POST /dshgw/nodes', (body) => { deployBodies.push(body); return { node: { name: body.name } }; });
  responses.set('POST /dshgw/tenants/alice/restart', () => ({ restarted: 'alice' }));
  responses.set('POST /dshgw/tenants/bob/node', () => ({ tenant: 'bob', node: 'node-a' }));
}

// renderPage is one page render; it returns the toolbar and page elements the assertions reach into.
async function renderPage(role) {
  reset();
  const page = node('div');
  const actions = node('div');
  await render({ page, actions, session: { role: role || 'admin' } });
  await settle();
  return { page, actions };
}

// --- 1. the list is cheap, and probing is an explicit action ----------------------------------

{
  const { page, actions } = await renderPage('admin');
  assert.deepEqual(listProbes, [false], 'the first load must not probe: a page render is not a health check');
  assert.ok(textsOf(page).includes('node-a') && textsOf(page).includes('node-b'), 'every node must be listed');
  assert.ok(textsOf(page).includes('就绪'), 'a ready node shows its state');
  assert.ok(textsOf(page).includes('不可达'), 'an unreachable node shows its state');
  assert.ok(textsOf(page).includes('插件 1'), 'the capability report reaches the table');
  assert.ok(textsOf(page).includes('alice') && textsOf(page).includes('bob'), 'the tenant table lists the placement rows');

  const probeAll = buttonOf(actions, '探测全部');
  assert.ok(probeAll, 'probing the whole fleet is a button, not a side effect');
  probeAll.click();
  await settle();
  assert.deepEqual(listProbes, [false, true], 'the button is what asks for a probe');
}

// --- 2. a single probe knows its node, and its refusal is shown -------------------------------

{
  const { page } = await renderPage('admin');
  const row = buttonOf(page, '探测');
  assert.ok(row, 'every node row offers a probe');
  row.click();
  await settle();
  const probe = calls.find((entry) => entry.method === 'POST' && entry.path.endsWith('/probe'));
  assert.equal(probe.path, '/dshgw/nodes/node-a/probe', 'the probe names the node in the row it was clicked on');
  assert.ok(toasts.some((entry) => entry.message.includes('node-a')), 'the outcome is reported to the operator');
}

// --- 3. a deploy is asynchronous: refusal → fingerprint confirmation → retry -------------------

{
  const { page } = await renderPage('admin');
  deployRunning = true; // 这一节要验证"部署进行中"的界面，所以任务先活着
  const deploy = buttonOf(page, '部署/升级');
  assert.ok(deploy, 'an admin can deploy');
  deploy.click();
  await settle();
  // The stub confirms instantly, so both sends are observed in one settle: the first without a
  // fingerprint, the retry with it. That order is the whole point of the gate.
  assert.ok(deployBodies.length >= 1, 'the first attempt is sent');
  assert.ok(!deployBodies[0].accept_host_key, 'and it does not invent a fingerprint');
  assert.ok(modals.some((entry) => String(entry.title || '').includes('主机密钥')
    && String(entry.message || '').includes(FINGERPRINT)),
    'the refusal becomes a confirmation dialog naming the fingerprint');
  assert.equal(deployBodies.length, 2, 'confirming retries once');
  assert.equal(deployBodies[1].accept_host_key, FINGERPRINT, 'the retry carries the confirmed fingerprint');
  assert.equal(deployPolls, 1, 'the page opens the progress drawer after starting the job');
  assert.ok(textsOf(modalRoot).includes('preflight'), 'the drawer shows the phases');
  assert.ok(textsOf(modalRoot).includes('upload-ok'), 'and the log tail');
  assert.ok(!textsOf(modalRoot).includes('systemd'),
    'while the job runs the page does not yet claim how the node is supervised (that is only known once it is up)');
  // The drawer polls while the job runs; let it finish and take one more poll.
  deployRunning = false;
  await new Promise((resolve) => setTimeout(resolve, 1600));
  await settle();
  assert.ok(textsOf(modalRoot).includes('the node answers'), 'the finished job shows the log that proves it');
  assert.ok(textsOf(modalRoot).includes('systemd'), 'and how the node is supervised from then on');
}

// --- 4. rotate-token says what it costs, and sends the rotation ------------------------------

{
  const { page } = await renderPage('admin');
  const rotate = buttonOf(page, '轮换令牌');
  rotate.click();
  await settle();
  assert.ok(modals.some((entry) => entry.title.includes('轮换节点令牌')), 'a rotation is confirmed first');
  assert.ok(deployBodies.some((entry) => entry.rotate_token === true), 'and then the rotation is sent');
}

// --- 5. reconcile reports what it did, not just that it ran ----------------------------------

{
  const { page } = await renderPage('admin');
  rowButtonOf(page, 'node-a', '对账').click();
  await settle();
  const toast = toasts.find((entry) => entry.message.includes('对账完成'));
  assert.ok(toast, 'the reconciliation reports its outcome');
  assert.ok(toast.message.includes('剪除 1'), 'including the records it pruned: ' + toast.message);
}

// --- 6. a purge needs the node name typed again ---------------------------------------------

{
  const { page } = await renderPage('admin');
  rowButtonOf(page, 'node-b', '删除').click();
  await settle();
  const dialog = modalRoot.children[modalRoot.children.length - 1];
  assert.ok(buttonOf(dialog, '仅移除记录'), 'forgetting a node and purging it are two different buttons');
  assert.ok(buttonOf(dialog, '彻底删除（含目标机数据）'), 'the destructive one says what it deletes');
  buttonOf(dialog, '彻底删除（含目标机数据）').click();
  await settle();
  const purge = modals.find((entry) => entry.title.includes('彻底删除'));
  assert.ok(purge, 'the purge asks for confirmation');
  assert.ok(purge.fields.some((field) => field.name === 'confirm'), 'and it needs the node name typed');
  // A wrong name must not purge.
  const refused = await purge.onSubmit({ confirm: 'node-c' });
  assert.equal(refused, false, 'a mismatched name is refused');
  assert.equal(calls.filter((entry) => entry.method === 'DELETE').length, 0, 'a mismatched name must not purge');
  const result = await purge.onSubmit({ confirm: 'node-b' });
  assert.equal(result, true, 'the matching name proceeds');
  const purgeCall = calls.find((entry) => entry.method === 'DELETE');
  assert.equal(purgeCall.path, '/dshgw/nodes/node-b?purge=true&confirm=node-b', 'the confirmation travels with the request');
}

// --- 7. forgetting a node touches nothing on the machine ------------------------------------

{
  const { page } = await renderPage('admin');
  rowButtonOf(page, 'node-b', '删除').click();
  await settle();
  const dialog = modalRoot.children[modalRoot.children.length - 1];
  buttonOf(dialog, '仅移除记录').click();
  await settle();
  const del = calls.find((entry) => entry.method === 'DELETE');
  assert.equal(del.path, '/dshgw/nodes/node-b', 'the plain removal sends no purge flag');
  assert.ok(toasts.some((entry) => entry.message.includes('目标机未改动')), 'and says so');
}

// --- 8. moving a tenant warns that it does not move data ------------------------------------

{
  const { page } = await renderPage('admin');
  const move = rowButtonOf(page, 'bob', '迁移落点');
  assert.ok(move, 'the tenant table offers the migration');
  move.click();
  await settle();
  const dialog = modals.find((entry) => entry.title.includes('迁移'));
  assert.ok(dialog, 'the migration opens a form');
  const placement = dialog.fields.find((field) => field.name === 'node');
  assert.equal(placement.type, 'select', 'the placement is a choice among known nodes');
  // ui.js accepts plain strings or {value,label}; the page passes strings, and the test reads both.
  const optionValues = placement.options.map((option) => (typeof option === 'string' ? option : option.value));
  assert.ok(optionValues.includes('local'), 'including this machine: ' + optionValues.join(','));
  assert.ok(optionValues.includes('node-a'), 'and the registered nodes: ' + optionValues.join(','));
  await dialog.onSubmit({ node: 'node-a' });
  await settle();
  assert.ok(modals.some((entry) => String(entry.message || '').includes('不会搬运数据')),
    'the confirmation says the data stays where it is');
  const sent = calls.find((entry) => entry.method === 'POST' && entry.path.endsWith('/node'));
  assert.equal(sent.path, '/dshgw/tenants/bob/node', 'the placement is sent for that tenant');
  assert.equal(sent.body.node, 'node-a');
}

// --- 9. registration only writes a record ----------------------------------------------------

{
  const { actions } = await renderPage('admin');
  buttonOf(actions, '添加节点').click();
  await settle();
  const dialog = modals.find((entry) => entry.title.includes('添加工作节点'));
  assert.ok(dialog, 'the registration form opens');
  const result = await dialog.onSubmit({
    name: 'node-c', listen: '10.0.0.3:18400', ssh_host: '10.0.0.3', ssh_user: 'winger',
    ssh_port: '22', worker_port_lo: '', worker_port_hi: '', default: 'false',
  });
  assert.equal(result, true, 'a valid registration closes the form');
  const created = calls.find((entry) => entry.method === 'POST' && entry.path === '/dshgw/nodes');
  assert.equal(created.body.name, 'node-c');
  assert.equal(created.body.listen, '10.0.0.3:18400');
  assert.equal(created.body.ssh_port, 22, 'a numeric field is sent as a number');
  assert.equal(created.body.worker_port_lo, undefined, 'an empty optional field is left out, not sent as 0');
  assert.equal(created.body.default, false, 'the boolean is a boolean');
  assert.ok(toasts.some((entry) => entry.message.includes('部署')), 'and the operator is told the next step is a deploy');
}

// --- 10. a read-only session gets no actions at all ------------------------------------------

{
  const { page, actions } = await renderPage('viewer');
  // The toolbar按钮按仓库惯例是"可见但禁用"（backups/admins 页一致），行内动作则直接不渲染：
  // 一个只读会话不该看到一排点下去必然 403 的按钮。
  assert.equal(buttonOf(actions, '添加节点').disabled, true, 'a viewer cannot register a node');
  assert.equal(buttonOf(page, '部署/升级'), null, 'a viewer cannot deploy');
  assert.equal(buttonOf(page, '删除'), null, 'a viewer cannot remove a node');
  assert.equal(buttonOf(page, '迁移落点'), null, 'a viewer cannot move a tenant');
  assert.equal(buttonOf(page, '轮换令牌'), null, 'a viewer cannot rotate a secret');
  assert.equal(buttonOf(page, '重启'), null, 'a viewer cannot restart a tenant');
  assert.ok(buttonOf(page, '探测'), 'but looking is still allowed');
  assert.ok(buttonOf(page, '审计'), 'including reading a node\'s events');
  assert.ok(buttonOf(page, '部署进度'), 'and watching a deploy somebody else started');
}

// --- 11. the pure helpers say what the page shows -------------------------------------------

{
  // 逐字段比较而不是 deepEqual：这些对象是在 VM 里造出来的，原型与测试所在的 realm 不同
  // （仓库里 org_assign_test.mjs 有同样的说明）。
  const ready = stateInfo('ready');
  assert.equal(ready.label, '就绪');
  assert.equal(ready.kind, 'ok');
  const unreachable = stateInfo('unreachable');
  assert.equal(unreachable.label, '不可达');
  assert.equal(unreachable.kind, 'danger');
  const unknown = stateInfo('weird');
  assert.equal(unknown.label, 'weird', 'an unknown state is shown as it is, not hidden');
  assert.equal(unknown.kind, '');
  assert.equal(featureSummary({ tenant_plugins: ['a', 'b'], host_shares: 2, ssh_workspaces: true }), '插件 2 · 主机目录 2 · SSH 工作区');
  assert.equal(featureSummary(undefined), '—');
  const phases = deployPhaseRows({ phases: [{ name: 'preflight', ok: true, millis: 12 }, { name: 'start' }] });
  assert.equal(phases.length, 2);
  assert.equal(phases[0].millis, 12);
  assert.equal(phases[1].ok, true, 'a phase without a verdict is not reported as failed');
  assert.ok(supervisionLabel({ running: false, systemd: false }).includes('不会自动拉起'),
    'the detached fallback must say it will not survive a reboot');
  assert.ok(supervisionLabel({ running: false, systemd: true }).includes('会自动拉起'));
}

// --- 12. nothing may outlive the page ---------------------------------------------------------
//
// 轮询链一旦在抽屉关闭后继续跑，浏览器标签页会一直打网关（真实缺陷，不是测试洁癖）。Node 把
// 未清的定时器报成活动 handle，所以这里可以真的断言"没有遗留"。
{
  const pending = process._getActiveHandles().filter((handle) => handle && handle.constructor
    && handle.constructor.name === 'Timeout');
  assert.equal(pending.length, 0,
    'no polling timer may outlive the drawers: ' + pending.length + ' timer(s) left running');
}

console.log('dshgw_nodes_test.mjs: all checks passed');
