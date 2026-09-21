// Node-only regression test for the Feishu directory-sync dialog (M70). It loads
// pages/org_feishu.js as an ES module with the UI, API, tree and pinyin dependencies mocked,
// and asserts the requests the dialog builds — which is the part a browser harness can only
// observe through a stubbed fetch.
//
// The interesting claims are the ones about *what is sent*: the sync posts to the sync
// endpoint, "创建用户" posts to the person's open_id (percent-encoded, because an open id is
// opaque), "绑定账号" PUTs the chosen account id, and "解绑" DELETEs the same path. The merge
// rules themselves belong to the server and are tested in Go.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/org_feishu.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');

const calls = [];
const toasts = [];
const confirmations = [];
let confirmAnswer = true;
let modalResult = null;
let modalArgs = null;

function node(tag, props = {}, childList) {
  const children = [];
  const el = {
    tag, ...props, children, listeners: {}, checked: false, disabled: false, value: props.value || '',
    // The real el() skips null/undefined/false children (that is how the console renders an
    // optional row), so the mock must too — otherwise walking the tree stumbles over nulls.
    append(...items) {
      for (const item of items.flat()) {
        if (item === null || item === undefined || item === false) continue;
        children.push(item);
      }
    },
    addEventListener(name, listener) { this.listeners[name] = listener; },
    click() { const handler = this.listeners.click; if (handler) handler({ currentTarget: this, target: this }); },
    replaceChildren(...items) { children.length = 0; children.push(...items.flat()); },
    querySelector(selector) {
      // Enough of the selector language for the assertions below: a tag name, a
      // tag[type=x] or a class.
      const match = (candidate) => {
        if (!candidate || !candidate.tag) return false;
        const attr = selector.match(/^(\w+)\[type=["']?([\w-]+)["']?\]$/);
        if (attr) return candidate.tag === attr[1] && candidate.type === attr[2];
        if (selector.startsWith('.')) return String(candidate.class || '').split(' ').includes(selector.slice(1));
        return candidate.tag === selector;
      };
      const walk = (item) => {
        for (const child of item.children || []) {
          if (match(child)) return child;
          const found = walk(child);
          if (found) return found;
        }
        return null;
      };
      return walk(this);
    },
    querySelectorAll(selector) {
      const out = [];
      const match = (candidate) => {
        if (!candidate || !candidate.tag) return false;
        if (selector.startsWith('.')) return String(candidate.class || '').split(' ').includes(selector.slice(1));
        const attr = selector.match(/^(\w+)\[type=["']?([\w-]+)["']?\]$/);
        if (attr) return candidate.tag === attr[1] && candidate.type === attr[2];
        return candidate.tag === selector;
      };
      const walk = (item) => {
        for (const child of item.children || []) {
          if (match(child)) out.push(child);
          walk(child);
        }
      };
      walk(this);
      return out;
    },
    remove() {},
  };
  // The literal defaults above would otherwise win over the props (they are written after the
  // spread), so they are re-applied from the props when the caller stated them.
  for (const key of ['disabled', 'checked', 'value']) {
    if (props && props[key] !== undefined) el[key] = props[key];
  }
  // Handlers arrive as `onclick`-style attributes (that is what ui.js's el() accepts and
  // translates into addEventListener), so the mock has to do that translation too — without
  // it every button in the dialog would look inert.
  for (const [key, value] of Object.entries(props || {})) {
    if (key.startsWith('on') && typeof value === 'function') el.listeners[key.slice(2)] = value;
  }
  // el(tag, attrs, children) is how the console builds every node: the third argument is the
  // child list, so the mock has to accept it or the whole tree comes out empty.
  if (childList !== undefined && childList !== null) el.append(...[].concat(childList));
  return el;
}

// textOf walks the mocked tree: the dialog's assertions in the harness read textContent, and
// the module builds text nodes through el(..., { text }).
function textOf(item) {
  if (!item) return '';
  if (typeof item === 'string') return item;
  // The module writes some labels through textContent (the header line, the people count),
  // which a plain object mock stores as an ordinary property.
  const own = item.textContent !== undefined ? item.textContent : item.text;
  let out = own === undefined ? '' : String(own);
  for (const child of item.children || []) out += ' ' + textOf(child);
  return out;
}

const directory = {
  fetched_at: '2026-09-22T08:30:00Z', cached: false, names_available: true, truncated: false,
  departments: [
    { id: 'od_a', parent_id: null, name: '研发部', depth: 0, direct_user_count: 1, selected: true, included: true,
      local: { node_id: null, matched: '', will_create: true } },
    { id: 'od_b', parent_id: null, name: '市场部', depth: 0, direct_user_count: 2, selected: true, included: true,
      local: { node_id: 2, matched: 'name', will_pin: true } },
  ],
  users: [
    { open_id: 'ou_wang', union_id: 'on_wang', name: '王五', department_ids: ['od_a'], in_scope: true,
      account: { id: 7, name: '王五', matched_by: 'name', needs_bind: true },
      join_nodes: [{ department_id: 'od_a', name: '研发部', node_id: null, will_create: true }] },
    { open_id: 'ou_new', union_id: 'on_new', name: '李四', department_ids: ['od_b'], in_scope: true,
      account: null, join_nodes: [{ department_id: 'od_b', name: '市场部', node_id: 2, will_create: false }] },
    { open_id: 'ou_zhao', union_id: 'on_zhao', name: '赵六', department_ids: ['od_b'], in_scope: true,
      account: { id: 1, name: 'acme', matched_by: 'api_key', needs_bind: true }, join_nodes: [] },
  ],
  users_truncated: false,
  selection: [],
  unknown_department_ids: [],
  stats: { departments: 2, departments_selected: 2, departments_ancestors: 0,
    departments_to_create: 1, departments_to_pin: 1, departments_skipped: 0,
    users: 3, users_in_scope: 3, users_out_of_scope: 0,
    users_matched: 2, users_unmatched: 1, users_already_synced: 0, memberships_to_add: 1 },
  warnings: [],
};

const api = {
  async get(path, params) {
    calls.push({ method: 'GET', path, params });
    if (path === '/org/feishu/directory') return JSON.parse(JSON.stringify(directory));
    if (path === '/accounts') {
      return { data: [
        { id: 1, name: 'acme' }, { id: 7, name: '王五' }, { id: 4, name: '李四' }, { id: 3, name: '张三' },
      ] };
    }
    throw new Error('unexpected GET ' + path);
  },
  post: async (path, body) => {
    calls.push({ method: 'POST', path, body });
    // The create-user route answers with the new account, which is what the dialog names in
    // its confirmation toast.
    if (/\/account$/.test(path)) return { ok: true, account: { id: 8, name: (body && body.name) || '新账户' } };
    return { ok: true, created_nodes: [], linked_users: [] };
  },
  put: async (path, body) => { calls.push({ method: 'PUT', path, body }); return { ok: true, account: { id: body.account_id } }; },
  del: async (path) => { calls.push({ method: 'DELETE', path }); return { ok: true, unbound: true }; },
  errorMessage: (err) => (err && err.message) || String(err),
};

let treeInstance = null;
const ui = {
  el: (tag, attrs, children) => node(tag, attrs, children),
  modal: async (options) => { modalArgs = options; return modalResult; },
  toast: (text, level) => { toasts.push({ text, level }); return node('div', { text }); },
  badge: (text) => node('span', { text }),
  withBusy: async (button, label, action) => action(),
  confirmDialog: async (title) => { confirmations.push(title); return confirmAnswer; },
  modalHead: (title) => node('div', { text: title }),
  modalBody: (children) => node('div', {}, children),
  modalActions: (children) => node('div', {}, children),
};

const base = { apiRoot: () => '/admin/api/v1', serverRoot: () => '', consolePath: (path) => path };

const modalRoot = node('div');
const context = vm.createContext({
  console,
  URL,
  URLSearchParams,
  encodeURIComponent,
  window: { location: { hash: '' }, addEventListener() {}, dispatchEvent() {} },
  document: {
    createElement: (tag) => node(tag),
    getElementById: (id) => (id === 'modal-root' ? modalRoot : null),
    querySelector: () => null,
    querySelectorAll: () => [],
  },
  navigator: {},
  setTimeout,
  clearTimeout,
  fetch: async () => { throw new Error('the dialog must not fetch directly'); },
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
const linker = async (specifier) => {
  if (specifier === '../api.js') return new vm.SyntheticModule(['api'], function () { this.setExport('api', api); }, { context });
  if (specifier === '../ui.js') return new vm.SyntheticModule(
    ['el', 'modal', 'toast', 'badge', 'withBusy', 'confirmDialog', 'modalHead', 'modalBody', 'modalActions'],
    function () { for (const [name, value] of Object.entries(ui)) this.setExport(name, value); }, { context });
  if (specifier === '../tree.js') return new vm.SyntheticModule(['tree'], function () {
    this.setExport('tree', (options) => {
      treeInstance = { options, nodes: [], refresh(next) { this.nodes = next; }, setSelected() {}, expandAll() {}, collapseAll() {} };
      return { node: node('div', { class: 'tree' }), refresh: treeInstance.refresh.bind(treeInstance), setSelected() {}, expandAll() {}, collapseAll() {} };
    });
  }, { context });
  if (specifier === '../pinyin.js') return new vm.SyntheticModule(['matchesQuery'], function () {
    // A stand-in that behaves like the real one for the cases the dialog filters on: pinyin
    // for the two names under test, plain substring otherwise.
    this.setExport('matchesQuery', (text, query) => {
      const needle = String(query || '').trim().toLowerCase();
      if (!needle) return true;
      const source = String(text || '').toLowerCase();
      if (source.includes(needle)) return true;
      const pinyin = { 王五: 'wangwu ww', 李四: 'lisi ls', 张三: 'zhangsan zs', 'dev-张三': 'dev-zhangsan devzs', acme: 'acme', 赵六: 'zhaoliu zl' };
      return (pinyin[text] || '').split(' ').some((reading) => reading.startsWith(needle));
    });
  }, { context });
  if (specifier === '../base.js') return new vm.SyntheticModule(['apiRoot', 'serverRoot', 'consolePath'], function () {
    this.setExport('apiRoot', base.apiRoot);
    this.setExport('serverRoot', base.serverRoot);
    this.setExport('consolePath', base.consolePath);
  }, { context });
  throw new Error('unexpected import ' + specifier);
};
await module.link(linker);
await module.evaluate();

// --- the dialog opens with a preview request -------------------------------------------

// The dialog reads the directory on open and asks for the cache (no refresh=true).
const settle = async () => { for (let i = 0; i < 50; i++) await Promise.resolve(); };
module.namespace.openFeishuSync({ onDone: () => {} });
await settle();

const preview = calls.find((call) => call.path === '/org/feishu/directory');
assert.ok(preview, 'opening the dialog must read the directory');
assert.equal(preview.method, 'GET');
assert.equal(preview.params, undefined, 'the first read takes the server cache');
// …and then it re-reads once with the whole directory selected, which is what makes the
// numbers in the confirm dialog the server's numbers for that exact scope.
const seeded = calls.filter((call) => call.path === '/org/feishu/directory').pop();
assert.match(seeded.params.departments, /^0,od_a,od_b$/, 'every node is in scope by default');

assert.ok(modalRoot.children.length >= 1, 'the dialog is appended to #modal-root');
const dialog = modalRoot.children[0];
const dialogText = textOf(dialog);
assert.match(dialogText, /同步飞书组织架构/);
assert.match(dialogText, /将创建 1/, 'the header states how many departments will be created');
assert.match(dialogText, /待决定 1/, 'the header states how many people need a decision');
assert.ok(treeInstance, 'the department tree is built from the preview');
assert.equal(treeInstance.nodes.length, 3, 'a synthetic root plus the two departments');
assert.equal(treeInstance.nodes[1].parent_id, '0', 'top-level departments hang under the root');

const refreshButton = dialog.querySelectorAll('button').find((b) => b.text === '刷新');
const syncButton = dialog.querySelectorAll('button').find((b) => b.text === '同步');
assert.ok(refreshButton && syncButton, 'the dialog offers 刷新 and 同步');

// --- 同步 confirms, then posts, then re-reads with refresh=true -------------------------

confirmations.length = 0;
confirmAnswer = false;
syncButton.click();
await settle();
assert.equal(confirmations.length, 1, '同步 asks for confirmation first');
assert.match(confirmations[0], /执行一次飞书同步/);
assert.ok(!calls.some((call) => call.method === 'POST' && call.path === '/org/feishu/sync'),
  'a cancelled confirmation must not post');

confirmAnswer = true;
syncButton.click();
await settle();
const sync = calls.find((call) => call.method === 'POST' && call.path === '/org/feishu/sync');
assert.ok(sync, 'a confirmed 同步 posts to the sync endpoint');
assert.deepEqual([...sync.body.department_ids].sort(), ['0', 'od_a', 'od_b'],
  'the sync carries the scope the operator ticked');
const refreshed = calls.filter((call) => call.path === '/org/feishu/directory').pop();
// The objects come out of the module's own VM context, so compare fields rather than
// prototypes.
assert.equal(refreshed.params.refresh, 'true', 'the dialog re-reads the directory uncached after a write');
assert.ok(toasts.some((toast) => /同步完成/.test(toast.text)), 'the result is reported');

// --- 创建用户 posts the person's open id and the typed name -----------------------------

const personRow = (name) => dialog.querySelectorAll('.feishu-user').find((row) => textOf(row).includes(name));
// The create form resolves with the account the route returned, so the mock answers the way
// the server does.
modalResult = { account: { id: 8, name: '李四（市场）' } };
const unmatchedRow = personRow('李四');
assert.ok(unmatchedRow, 'the unmatched person is listed');
assert.match(textOf(unmatchedRow), /未匹配/);
const createButton = unmatchedRow.querySelectorAll('button').find((b) => b.text === '创建用户');
assert.ok(createButton, 'an unmatched person offers 创建用户');
createButton.click();
await settle();
assert.ok(modalArgs, '创建用户 opens the create form');
assert.equal(modalArgs.fields[0].value, '李四', 'the form is prefilled with the Feishu name');
// Submitting the form is what the modal would do with the operator's values.
const created = await modalArgs.onSubmit({ name: '李四（市场）', note: '' });
await settle();
assert.equal(created.account.id, 8);
const createCall = calls.find((call) => call.method === 'POST' && /\/org\/feishu\/users\/.+\/account$/.test(call.path));
assert.ok(createCall, '创建用户 posts to the person endpoint');
assert.equal(createCall.path, '/org/feishu/users/ou_new/account', 'the path carries the open_id');
assert.equal(createCall.body.name, '李四（市场）');
assert.equal(createCall.body.note, '');

// --- 绑定账号 opens the picker, preselects the same-name account, and PUTs the choice ---

const pickButton = personRow('李四').querySelectorAll('button').find((b) => b.text === '绑定账号');
pickButton.click();
await settle();
const accountRead = calls.find((call) => call.method === 'GET' && call.path === '/accounts');
assert.ok(accountRead, 'the picker reads the account list once');
assert.equal(accountRead.params.limit, 1000, 'with the same limit the organization page uses');

const picker = modalRoot.children[modalRoot.children.length - 1];
const pickerText = textOf(picker);
assert.match(pickerText, /选择要把这个飞书人员绑定到哪个本地账户/);
assert.match(pickerText, /acme/);
// acme is already matched to 赵六, so it can never be stolen by 李四.
const pickerRows = picker.querySelectorAll('.feishu-picker-row');
const acmeRow = pickerRows.find((row) => textOf(row).includes('acme'));
assert.ok(acmeRow, 'every account is listed');
assert.equal(acmeRow.querySelector('input').disabled, true, 'an account bound to someone else cannot be chosen');
assert.match(textOf(acmeRow), /已绑定其他飞书身份/);
const lisiRow = pickerRows.find((row) => textOf(row).includes('李四'));
assert.ok(lisiRow, 'the same-name account is listed');
assert.equal(lisiRow.querySelector('input').checked, true, 'the same-name candidate is preselected');

// Choosing another account and confirming sends that id.
const zhangRow = pickerRows.find((row) => textOf(row).includes('张三'));
zhangRow.querySelector('input').checked = true;
zhangRow.querySelector('input').listeners.change();
const bindButton = picker.querySelectorAll('button').find((b) => b.text === '绑定');
assert.ok(bindButton, 'the picker has a confirm button');
bindButton.click();
await settle();
const bindCall = calls.find((call) => call.method === 'PUT' && /\/org\/feishu\/users\/.+\/account$/.test(call.path));
assert.ok(bindCall, 'binding PUTs the person endpoint');
assert.equal(bindCall.path, '/org/feishu/users/ou_new/account');
assert.equal(bindCall.body.account_id, 3, 'the chosen account id is what travels');

// --- 解绑 confirms first, then deletes -------------------------------------------------

confirmations.length = 0;
const matchedRow = personRow('王五');
const unbindButton = matchedRow.querySelectorAll('button').find((b) => b.text === '解绑');
assert.ok(unbindButton, 'a matched person offers 解绑');
confirmAnswer = false;
unbindButton.click();
await settle();
assert.equal(confirmations.length, 1, '解绑 asks for confirmation');
assert.ok(!calls.some((call) => call.method === 'DELETE'), 'a cancelled 解绑 must not delete');
confirmAnswer = true;
unbindButton.click();
await settle();
const unbind = calls.find((call) => call.method === 'DELETE');
assert.ok(unbind, 'a confirmed 解绑 deletes the mapping');
assert.equal(unbind.path, '/org/feishu/users/ou_wang/account');

// --- a failed read reports itself instead of rendering an empty tree --------------------

calls.length = 0;
modalRoot.children.length = 0;
const failingApi = { ...api, get: async (path) => { throw new Error('502 飞书拒绝了这次通讯录读取'); } };
const failContext = vm.createContext({
  console, URL, URLSearchParams, encodeURIComponent,
  window: { location: { hash: '' }, addEventListener() {}, dispatchEvent() {} },
  document: { createElement: (tag) => node(tag), getElementById: () => modalRoot, querySelector: () => null, querySelectorAll: () => [] },
  navigator: {}, setTimeout, clearTimeout, fetch: async () => { throw new Error('no direct fetch'); },
});
const failModule = new vm.SourceTextModule(source, { identifier: sourceURL.href, context: failContext });
await failModule.link(async (specifier) => {
  if (specifier === '../api.js') return new vm.SyntheticModule(['api'], function () { this.setExport('api', failingApi); }, { context: failContext });
  if (specifier === '../ui.js') return new vm.SyntheticModule(
    ['el', 'modal', 'toast', 'badge', 'withBusy', 'confirmDialog', 'modalHead', 'modalBody', 'modalActions'],
    function () { for (const [name, value] of Object.entries(ui)) this.setExport(name, value); }, { context: failContext });
  if (specifier === '../tree.js') return new vm.SyntheticModule(['tree'], function () {
    this.setExport('tree', () => ({ node: node('div'), refresh() {}, setSelected() {}, expandAll() {}, collapseAll() {} }));
  }, { context: failContext });
  if (specifier === '../pinyin.js') return new vm.SyntheticModule(['matchesQuery'], function () { this.setExport('matchesQuery', () => true); }, { context: failContext });
  if (specifier === '../base.js') return new vm.SyntheticModule(['apiRoot', 'serverRoot', 'consolePath'], function () {
    this.setExport('apiRoot', base.apiRoot); this.setExport('serverRoot', base.serverRoot); this.setExport('consolePath', base.consolePath);
  }, { context: failContext });
  throw new Error('unexpected import ' + specifier);
});
await failModule.evaluate();
failModule.namespace.openFeishuSync({ onDone: () => {} });
await settle();
const failedText = textOf(modalRoot.children[0]);
assert.match(failedText, /读取飞书通讯录失败/, 'a failed read says so instead of showing an empty tree');
assert.match(failedText, /502/, 'and repeats the reason the server gave');

console.log('org_feishu_test.mjs: ok');
