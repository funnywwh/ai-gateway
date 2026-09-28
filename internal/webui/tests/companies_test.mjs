// Node-only regression test for the 「公司」page (M93) — the console-side half of the
// console-managed Feishu company registry.
//
// What is asserted is *what the page sends*: a create carries the secret exactly once, an edit
// without a secret does not send the field at all (that is how "keep the stored one" travels), the
// probe can run before saving with the typed values, deleting asks for confirmation and says the data
// stays, and a viewer sees no write actions.
//
// The page is loaded as an ES module in a VM with the UI, API and router dependencies mocked, the
// same shape org_feishu_test.mjs uses.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/companies.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');
const routerSource = await readFile(new URL('../static/js/router.js', import.meta.url), 'utf8');

function node(tag, props = {}, childList) {
  const children = [];
  const el = {
    tag, ...props, children, listeners: {}, checked: false, disabled: false,
    value: props.value === undefined ? '' : props.value,
    append(...items) {
      for (const item of items.flat()) {
        if (item === null || item === undefined || item === false) continue;
        children.push(item);
      }
    },
    addEventListener(name, listener) { this.listeners[name] = listener; },
    // The console's el() hands `onclick` through as a property (that is what a real browser fires),
    // so the mock honours both shapes.
    click() {
      const handler = this.onclick || this.listeners.click;
      if (handler) handler({ currentTarget: this, target: this });
    },
    replaceChildren(...items) { children.length = 0; this.append(...items); },
    querySelector(selector) {
      const match = (candidate) => {
        if (!candidate || !candidate.tag) return false;
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
  };
  if (childList !== undefined) el.append(...childList.flat());
  return el;
}

function textOf(item) {
  if (item === null || item === undefined) return '';
  if (typeof item === 'string') return item;
  const own = item.textContent !== undefined ? item.textContent : item.text;
  let out = own === undefined ? '' : String(own);
  for (const child of item.children || []) out += ' ' + textOf(child);
  return out;
}

const calls = [];
const toasts = [];
const confirmations = [];
let confirmAnswer = true;
let modalArgs = null;
let modalResult = null;
const navigations = [];

const companies = [
  { id: null, app_id: 'cli_aaa', name: '本公司', identity: true, source: 'identity', enabled: true,
    note: '', secret_configured: true, client_ready: true, client_error: '', shadowed_by_config: false,
    warnings: [], root_node_id: 1, root_node_name: '本公司', root_will_create: false, root_blocked: false,
    company_nodes: 3, linked_accounts: 5 },
  { id: 2, app_id: 'cli_bbb', name: '某某科技', identity: false, source: 'console', enabled: true,
    note: '客户 A', secret_configured: true, client_ready: true, client_error: '', shadowed_by_config: false,
    warnings: [], root_node_id: null, root_node_name: '某某科技', root_will_create: true, root_blocked: false,
    company_nodes: 3, linked_accounts: 5 },
  { id: 3, app_id: 'cli_ccc', name: '停用公司', identity: false, source: 'console', enabled: false,
    note: '', secret_configured: true, client_ready: true, client_error: '', shadowed_by_config: false,
    warnings: [], root_node_id: null, root_node_name: '停用公司', root_will_create: true, root_blocked: false,
    company_nodes: 0, linked_accounts: 0 },
];

const api = {
  async get(path, params) {
    calls.push({ method: 'GET', path, params });
    if (path === '/org/feishu/companies') {
      return { data: JSON.parse(JSON.stringify(companies)), count: companies.length, identity_app_id: 'cli_aaa', secrets_ready: true };
    }
    throw new Error('unexpected GET ' + path);
  },
  async post(path, body) {
    calls.push({ method: 'POST', path, body });
    if (path === '/org/feishu/companies') return { ok: true, id: 4, company: { id: 4, name: body.name } };
    if (path === '/org/feishu/companies/probe') {
      return body.id
        ? { ok: true, stage: 'ok', message: '可读：密钥有效', departments_seen: 4, names_available: true, samples: [{ id: 'od_1', name: '研发部' }] }
        : { ok: false, stage: 'credentials', message: 'App ID / App Secret 被飞书拒绝' };
    }
    throw new Error('unexpected POST ' + path);
  },
  async patch(path, body) { calls.push({ method: 'PATCH', path, body }); return { ok: true, id: 2 }; },
  async del(path) { calls.push({ method: 'DELETE', path }); return { ok: true, deleted: true, company_nodes: 3, linked_accounts: 5 }; },
  errorMessage: (err) => (err && err.message) || String(err),
};

let tables = [];
const ui = {
  el: (tag, attrs, children) => node(tag, attrs, children),
  card: (title, body, extra) => node('div', { class: 'card' }, [node('h3', { text: title }), body, ...(extra || [])]),
  badge: (text, kind) => node('span', { class: 'badge ' + (kind || ''), text }),
  toast: (text, level) => { toasts.push({ text, level }); return node('div', { text }); },
  confirmDialog: async (title, message) => { confirmations.push({ title, message }); return confirmAnswer; },
  withBusy: async (button, label, action) => action(),
  pagedTable: (options) => {
    const table = { options, refreshes: 0, node: node('div', { class: 'paged-table' }) };
    table.refresh = async () => { table.refreshes++; tables = [table]; await options.load({ limit: 50, offset: 0 }); };
    return table;
  },
  modal: async (options) => { modalArgs = options; return modalResult; },
  formatTime: (value) => String(value),
};

const base = {
  apiRoot: () => '/admin/api/v1',
  serverRoot: () => '',
  consolePath: (path) => path,
};

const context = vm.createContext({
  console,
  URL,
  URLSearchParams,
  encodeURIComponent,
  window: { location: { hash: '' }, addEventListener() {}, dispatchEvent() {} },
  document: {
    createElement: (tag) => node(tag),
    getElementById: () => null,
    querySelector: () => null,
    querySelectorAll: () => [],
  },
  navigator: {},
  setTimeout,
  clearTimeout,
  fetch: async () => { throw new Error('the page must not fetch directly'); },
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
await module.link(async (specifier) => {
  if (specifier === '../api.js') return new vm.SyntheticModule(['api'], function () { this.setExport('api', api); }, { context });
  if (specifier === '../ui.js') return new vm.SyntheticModule(
    ['el', 'card', 'pagedTable', 'modal', 'toast', 'badge', 'confirmDialog'],
    function () {
      for (const name of ['el', 'card', 'pagedTable', 'modal', 'toast', 'badge', 'confirmDialog']) {
        this.setExport(name, ui[name]);
      }
    }, { context });
  if (specifier === '../base.js') return new vm.SyntheticModule(['apiRoot', 'serverRoot', 'consolePath'], function () {
    this.setExport('apiRoot', base.apiRoot);
    this.setExport('serverRoot', base.serverRoot);
    this.setExport('consolePath', base.consolePath);
  }, { context });
  throw new Error('unexpected import ' + specifier);
});
await module.evaluate();

const page = node('div');
const actions = node('div');
await module.namespace.render({
  page, actions,
  session: { role: 'admin' },
  navigate: (path) => navigations.push(path),
});

const table = tables[0];
assert.ok(table, 'the page renders a table');
const rows = table.options.load ? companies : [];
assert.equal(rows.length, 3);

// --- 行渲染 ------------------------------------------------------------------------------

const wrappedRows = rows.map((row) => ({ row, cells: table.options.columns.map((col) => textOf(col.render ? col.render(row) : row[col.key])) }));
const identityRow = wrappedRows[0];
const consoleRow = wrappedRows[1];
const disabledRow = wrappedRows[2];
assert.match(identityRow.cells.join(' '), /身份应用/, 'the identity application is labelled as such');
assert.match(consoleRow.cells.join(' '), /控制台/, 'a console-registered company is labelled as such');
assert.match(disabledRow.cells.join(' '), /已停用/, 'a paused company says so');
assert.match(consoleRow.cells.join(' '), /未建（同步时新建「某某科技」）/, 'a company node that does not exist yet is described as to-be-created');

// 只读来源没有写按钮：身份应用与配置公司在页面里只能看。
const identityActions = table.options.rowActions(rows[0]).map(textOf);
assert.ok(identityActions.some((label) => label.includes('feishu.app_id')), 'the identity row points at the configuration setting');
assert.ok(!identityActions.includes('编辑'), 'the identity row has no edit action');
const consoleActions = table.options.rowActions(rows[1]).map(textOf);
// Arrays built inside the module's VM context carry that context's prototypes, so compare their
// contents as text rather than with deepStrictEqual.
assert.equal(consoleActions.slice(0, 4).join('|'), '同步|测试连接|编辑|停用', 'a console company offers sync, probe, edit and pause');
assert.ok(consoleActions.includes('删除登记'), 'a console company offers deleting its registration');

// 「同步」跳到组织架构页并带上公司（深链接）。
const syncButton = table.options.rowActions(rows[1]).find((button) => textOf(button) === '同步');
syncButton.click();
assert.equal(navigations.join(','), '/org?company=cli_bbb', 'the sync action deep-links into the org page with the company');

// --- 新建：密钥只随创建请求发送 -----------------------------------------------------------

const createButton = actions.children.find((child) => child.text === '新建公司');
assert.ok(createButton, 'the page offers 新建公司');
modalResult = { ok: true, id: 4 };
calls.length = 0;
createButton.click();
await new Promise((resolve) => setTimeout(resolve, 0));
assert.ok(modalArgs, '新建公司 opens a dialog');
assert.equal(modalArgs.submitLabel, '创建');
const fields = modalArgs.fields;
assert.equal(fields.map((field) => field.name).join(','), 'name,app_id,app_secret,root_node,note,enabled');
assert.equal(fields.find((field) => field.name === 'app_secret').type, 'password', 'the secret is a password input');
assert.equal(fields.find((field) => field.name === 'app_secret').value, undefined, 'the secret field is never prefilled');
// 先测一次（未保存也能测）：探测请求带着刚填的 app_id + app_secret。
modalArgs.extraActions[0].onClick({ name: '新客户', app_id: 'cli_ddd', app_secret: 'typed-secret' });
await new Promise((resolve) => setTimeout(resolve, 0));
const probeBeforeSave = calls.find((call) => call.path === '/org/feishu/companies/probe');
assert.ok(probeBeforeSave, 'the dialog can probe before saving');
assert.equal(JSON.stringify(probeBeforeSave.body), JSON.stringify({ app_id: 'cli_ddd', app_secret: 'typed-secret' }),
  'the probe carries the typed values and does not store anything');
assert.ok(toasts.some((toast) => /被飞书拒绝/.test(toast.text)), 'the probe verdict is reported');
// 提交创建。
await modalArgs.onSubmit({ name: '新客户', app_id: 'cli_ddd', app_secret: 'typed-secret', root_node: '', note: '', enabled: true });
const createCall = calls.find((call) => call.method === 'POST' && call.path === '/org/feishu/companies');
assert.ok(createCall, 'creating posts to the registry');
assert.equal(createCall.body.app_secret, 'typed-secret', 'the create carries the secret');
assert.equal(createCall.body.app_id, 'cli_ddd');

// --- 编辑：留空密钥 = 不发这个字段 ---------------------------------------------------------

modalResult = null;
calls.length = 0;
const editButton = table.options.rowActions(rows[1]).find((button) => textOf(button) === '编辑');
editButton.click();
await new Promise((resolve) => setTimeout(resolve, 0));
const editFields = modalArgs.fields;
assert.equal(editFields.find((field) => field.name === 'app_id').readonly, true, 'app_id is read-only while editing');
assert.equal(editFields.find((field) => field.name === 'app_id').value, 'cli_bbb');
assert.match(editFields.find((field) => field.name === 'app_secret').hint, /留空 = 保持不变/);
await modalArgs.onSubmit({ name: '某某科技', app_id: 'cli_bbb', app_secret: '', root_node: '', note: '客户 A2', enabled: true });
const patchCall = calls.find((call) => call.method === 'PATCH' && call.path === '/org/feishu/companies/2');
assert.ok(patchCall, 'editing PATCHes the registration');
assert.ok(!('app_secret' in patchCall.body), 'an empty secret field is omitted entirely (keep the stored one)');
assert.equal(patchCall.body.note, '客户 A2');

// --- 停用 / 删除 / 测试连接 ---------------------------------------------------------------

confirmations.length = 0;
const pauseButton = table.options.rowActions(rows[1]).find((button) => textOf(button) === '停用');
pauseButton.click();
await new Promise((resolve) => setTimeout(resolve, 0));
assert.equal(confirmations.length, 1, 'pausing asks for confirmation first');
assert.match(confirmations[0].message, /已导入的节点、成员关系与人员映射都保留/, 'the pause dialog promises the data stays');

confirmations.length = 0;
calls.length = 0;
const deleteButton = table.options.rowActions(rows[1]).find((button) => textOf(button) === '删除登记');
confirmAnswer = true;
deleteButton.click();
await new Promise((resolve) => setTimeout(resolve, 0));
assert.match(confirmations[0].title, /删除公司/);
assert.match(confirmations[0].message, /只删\*\*登记\*\*/, 'the delete dialog says the registration is what goes');
assert.match(confirmations[0].message, /3 个/, 'and reports how many nodes remain');
assert.ok(calls.some((call) => call.method === 'DELETE' && call.path === '/org/feishu/companies/2'), 'a confirmed delete calls the registry');

calls.length = 0;
const probeButton = table.options.rowActions(rows[1]).find((button) => textOf(button) === '测试连接');
probeButton.click();
await new Promise((resolve) => setTimeout(resolve, 0));
const probeCall = calls.find((call) => call.path === '/org/feishu/companies/probe');
assert.equal(JSON.stringify(probeCall.body), JSON.stringify({ id: 2 }),
  'probing a saved company sends only its id: the secret stays on the server');
assert.ok(toasts.some((toast) => /研发部/.test(toast.text)), 'a successful probe names a sample department');

// --- 只读角色 ----------------------------------------------------------------------------

const viewerPage = node('div');
const viewerActions = node('div');
await module.namespace.render({ page: viewerPage, actions: viewerActions, session: { role: 'viewer' }, navigate: () => {} });
assert.equal(viewerActions.children.find((child) => child.text === '新建公司').disabled, true, 'a viewer cannot create');
const viewerTable = tables[0];
const viewerActionsForRow = viewerTable.options.rowActions(rows[1]).map(textOf);
assert.ok(!viewerActionsForRow.includes('编辑') && !viewerActionsForRow.includes('删除登记'),
  'a viewer sees no write actions on a row');

// --- ui.js: the dialog gained extra footer actions ---------------------------------------

const uiSource = await readFile(new URL('../static/js/ui.js', import.meta.url), 'utf8');
assert.match(uiSource, /extraActions/, 'ui.modal supports the extraActions the company dialog uses');
assert.match(routerSource, /path: '\/companies'.*pages\/companies\.js/, 'the router serves the company page');

console.log('companies_test.mjs: ok');

// --- M94：公司名可改（含身份应用与配置来源的公司）-----------------------------------------

// 身份应用行现在也有「改名」（配置来源的行都只有改名，凭据/根节点/备注/启停仍归配置）。
const identityRenameActions = table.options.rowActions(rows[0]).map(textOf);
assert.ok(identityRenameActions.includes('改名'), 'the identity row offers 改名');
assert.ok(!identityRenameActions.includes('编辑'), 'the identity row still has no full edit (its credentials belong to the config)');
assert.match(identityRenameActions.join(' '), /feishu\.app_id/, 'and it says which setting owns the rest');

// 改名对话框：只有公司名可编辑，其余字段只读；提交只发 name，路径用 app_id。
calls.length = 0;
modalResult = { ok: true };
const renameButton = table.options.rowActions(rows[0]).find((button) => textOf(button) === '改名');
renameButton.click();
await new Promise((resolve) => setTimeout(resolve, 0));
const renameFields = modalArgs.fields;
assert.equal(modalArgs.submitLabel, '保存');
assert.equal(renameFields.find((field) => field.name === 'name').readonly, undefined, 'the name stays editable');
for (const name of ['app_id', 'app_secret', 'root_node', 'note', 'enabled']) {
  assert.equal(renameFields.find((field) => field.name === name).readonly, true, name + ' is read-only for a config-owned company');
}
assert.equal(modalArgs.extraActions.length, 0, 'no 先测试连接 for a company whose secret lives in the config');
await modalArgs.onSubmit({ name: '智天成', app_id: 'cli_aaa', app_secret: '', root_node: '', note: '', enabled: true });
const renameCall = calls.find((call) => call.method === 'PATCH');
assert.ok(renameCall, '改名 PATCHes the company');
assert.equal(renameCall.path, '/org/feishu/companies/cli_aaa', 'a config-owned company is addressed by its app id');
assert.equal(JSON.stringify(renameCall.body), JSON.stringify({ name: '智天成' }),
  'only the name travels: the other fields belong to the configuration');

// 根层已有同名节点时，对话框在名字字段上直接给出两条出路。
companies[0].root_name_taken = { node_id: 11, name: '智天成' };
await module.namespace.render({ page: node('div'), actions: node('div'), session: { role: 'admin' }, navigate: () => {} });
const warnedTable = tables[0];
warnedTable.options.rowActions(rows[0]).find((button) => textOf(button) === '改名').click();
await new Promise((resolve) => setTimeout(resolve, 0));
const warnedHint = modalArgs.fields.find((field) => field.name === 'name').hint;
assert.match(warnedHint, /先同步一次/, 'the conflict hint explains the sync-first way out');
assert.match(warnedHint, /改名\/移走/, 'and the rename-the-node way out');
companies[0].root_name_taken = null;

console.log('companies_test.mjs: M94 checks passed');
