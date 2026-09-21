// Node-only regression test for the API Keys page's Feishu column after M72. The page no
// longer binds anything — a Feishu identity belongs to an ACCOUNT, and binding happens on the
// organization page — so what this test pins is the shape that remains: the column is read-only,
// the only action left is clearing a leftover key-level binding, and the page never talks to
// Feishu itself.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/keys.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');

const calls = [];
const toasts = [];
const confirmations = [];
const navigations = [];
let confirmAnswer = true;
let table = null;

function node(tag, props = {}) {
  const children = props.children || [];
  const el = {
    tag, ...props, children, listeners: {},
    append(...items) { children.push(...items); },
    addEventListener(name, listener) { this.listeners[name] = listener; },
    replaceChildren(...items) { children.length = 0; children.push(...items); },
  };
  delete el.children;
  el.children = children;
  return el;
}

const api = {
  async get(path) {
    if (path === '/accounts') return { data: [{ id: 1, name: 'acme' }] };
    if (path === '/keys') {
      return {
        count: 2,
        data: [
          { id: 7, name: 'alice-key', account_id: 1, key_prefix: 'sk-gw-alice', status: 'active',
            tags: [], account_tags: [], effective_tags: [], record_input_mode: 'user',
            feishu: { bound: true, open_id: 'ou_alice', name: '张三', bound_by: 'admin', bound_at: '2026-09-18T10:00:00Z' } },
          { id: 8, name: 'bob-key', account_id: 1, key_prefix: 'sk-gw-bob', status: 'active',
            tags: [], account_tags: [], effective_tags: [], record_input_mode: 'user',
            feishu: { bound: false } },
        ],
      };
    }
    throw new Error('unexpected GET ' + path);
  },
  post: async (path, body) => { calls.push({ method: 'POST', path, body }); return {}; },
  patch: async (path, body) => { calls.push({ method: 'PATCH', path, body }); return {}; },
  del: async (path) => { calls.push({ method: 'DELETE', path }); return { unbound: true, key_id: 7 }; },
  errorMessage: (err) => (err && err.message) || String(err),
};

const ui = {
  el: node,
  card: (title, body, extra) => node('section', { title, body, extra, children: [] }),
  pagedTable(options) {
    table = options;
    return { node: node('div'), refresh: async () => { await options.load({ limit: 20, offset: 0 }); } };
  },
  modal: async () => null,
  toast: (text, level) => toasts.push({ text, level }),
  statusBadge: (status) => node('span', { text: status }),
  formatTime: (value) => String(value || ''),
  confirmDialog: async (title) => { confirmations.push(title); return confirmAnswer; },
  modalHead: (...args) => node('div', { args }),
  modalBody: (...args) => node('div', { args }),
  modalActions: (...args) => node('div', { args }),
};

const base = { apiRoot: () => '/admin/api/v1', serverRoot: () => '', consolePath: (path) => path };

const context = vm.createContext({
  console,
  URL,
  URLSearchParams,
  window: {
    location: {
      hash: '',
      assign(url) { navigations.push(url); },
    },
    addEventListener() {},
    dispatchEvent() {},
  },
  document: { createElement: (tag) => node(tag), querySelector: () => null },
  navigator: {},
  setTimeout,
  clearTimeout,
  fetch: async () => { throw new Error('the page must not fetch directly'); },
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
const linker = async (specifier) => {
  if (specifier === '../api.js') return new vm.SyntheticModule(['api'], function () { this.setExport('api', api); }, { context });
  if (specifier === '../base.js') return new vm.SyntheticModule(['apiRoot', 'serverRoot', 'consolePath'], function () {
    this.setExport('apiRoot', base.apiRoot);
    this.setExport('serverRoot', base.serverRoot);
    this.setExport('consolePath', base.consolePath);
  }, { context });
  if (specifier === '../ui.js') return new vm.SyntheticModule(
    ['el', 'card', 'pagedTable', 'modal', 'toast', 'statusBadge', 'formatTime', 'confirmDialog', 'modalHead', 'modalBody', 'modalActions'],
    function () {
      for (const [name, value] of Object.entries(ui)) this.setExport(name, value);
    }, { context });
  if (specifier === '../pinyin.js') return new vm.SyntheticModule(['matchesQuery'], function () {
    this.setExport('matchesQuery', () => true);
  }, { context });
  // The Key actions live in their own module since M72 (both this page and the organization page
  // use them); this test is about the Feishu column, so a stub with the right shape is enough.
  if (specifier === './key_actions.js') return new vm.SyntheticModule(
    ['createKeyForAccount', 'editKey', 'toggleKey', 'showSecret'],
    function () {
      for (const name of ['createKeyForAccount', 'editKey', 'toggleKey', 'showSecret']) {
        this.setExport(name, () => {});
      }
    }, { context });
  throw new Error('unexpected import ' + specifier);
};
await module.link(linker);
await module.evaluate();

async function renderPage(params = new URLSearchParams()) {
  const page = node('section');
  await module.namespace.render({
    page,
    actions: node('div'),
    session: { role: 'admin' },
    route: { params },
    navigate: (path) => navigations.push('#' + path),
  });
}

// --- the binding is visible, and the two row actions match its state ----------------------

await renderPage();
assert.ok(table, 'the page must render a table');
const keysColumn = table.columns.find((column) => column.key === 'feishu');
assert.ok(keysColumn, 'the keys table must carry a Feishu column');
assert.equal(keysColumn.label, '飞书（旧）', 'the column says it is the legacy key-level view');
const bound = keysColumn.render({ feishu: { bound: true, open_id: 'ou_alice', name: '张三' } });
assert.equal(bound.text, '张三', 'a bound key shows the Feishu name');
assert.match(bound.title, /ou_alice/, 'the full open id belongs in the tooltip');
const unbound = keysColumn.render({ feishu: { bound: false } });
assert.equal(unbound.text, '—');
// A row without the field at all (an older server) must render, not throw.
assert.equal(keysColumn.render({}).text, '—');

const boundActions = table.rowActions({ id: 7, name: 'alice-key', status: 'active', feishu: { bound: true, name: '张三' } });
const unboundActions = table.rowActions({ id: 8, name: 'bob-key', status: 'active', feishu: { bound: false } });
assert.ok(boundActions.some((action) => action.text === '解绑飞书'), 'a leftover binding can be cleared');
assert.ok(!unboundActions.some((action) => action.text === '解绑飞书'), 'an unbound key must not offer unbinding');
// Read-only operators get no write actions at all, Feishu included.
await renderPage();
const viewerActions = table.rowActions({ id: 7, name: 'alice-key', status: 'active', feishu: { bound: true } });
assert.equal(viewerActions.length, 3, 'the admin row keeps its actions');
const readonlyPage = node('section');
await module.namespace.render({
  page: readonlyPage,
  actions: node('div'),
  session: { role: 'viewer' },
  route: { params: new URLSearchParams() },
  navigate: () => {},
});
// Compared by length rather than deep equality: the actions are built inside the module's
// realm, so their Array prototype is not this test's.
assert.equal(table.rowActions({ id: 7, feishu: { bound: true } }).length, 0,
  'a read-only operator must not see binding actions');

// --- binding is gone: the page must not offer it, and must not navigate anywhere --------------

await renderPage();
const boundRow = { id: 7, name: 'alice-key', status: 'active', feishu: { bound: true, name: '张三' } };
assert.ok(!table.rowActions(boundRow).some((action) => action.text === '绑定飞书'),
  'the keys page must not offer binding any more (M72 moved it to the account)');

// --- unbinding confirms first, then deletes, and reports honestly --------------------------

const unbindAction = table.rowActions({ id: 7, name: 'alice-key', status: 'active', feishu: { bound: true, name: '张三' } })
  .find((action) => action.text === '解绑飞书');
confirmations.length = 0;
calls.length = 0;
toasts.length = 0;
confirmAnswer = false;
await unbindAction.onclick();
assert.equal(confirmations.length, 1, 'unbinding must ask first');
assert.equal(calls.length, 0, 'declining the confirmation must not call the API');

confirmAnswer = true;
await unbindAction.onclick();
assert.deepEqual(calls, [{ method: 'DELETE', path: '/keys/7/feishu' }]);
assert.equal(toasts.at(-1).text, '已解绑');

// An idempotent answer is reported as such rather than as a change.
api.del = async (path) => { calls.push({ method: 'DELETE', path }); return { unbound: false, key_id: 7 }; };
toasts.length = 0;
await unbindAction.onclick();
assert.equal(toasts.at(-1).text, '该 Key 本来就没有绑定');

// A failure is surfaced instead of looking like success.
api.del = async () => { throw new api_error('服务器错误'); };
function api_error(message) { const err = new Error(message); err.name = 'ApiError'; return err; }
toasts.length = 0;
await unbindAction.onclick();
assert.equal(toasts.at(-1).level, 'error');
assert.equal(toasts.at(-1).text, '服务器错误');

// --- a leftover result code in the hash is ignored, not reported -----------------------------
//
// The server used to redirect here with #/keys?feishu=… after a binding. Nothing produces that
// any more; an old bookmark must simply render the page.
toasts.length = 0;
await renderPage(new URLSearchParams('feishu=bound&key=7&dsh=enabled&tenant=dsh-alice'));
assert.equal(toasts.length, 0, 'a stale result code must not be reported as if it just happened');

// --- the page never builds a Feishu request of its own ------------------------------------

assert.doesNotMatch(source, /keys\/' \+ row\.id \+ '\/feishu' \+ '\?/, 'the page must not guess the bind URL shape');
assert.doesNotMatch(source, /app_secret|access_token|code_verifier/, 'the console must never see a Feishu credential');

console.log('keys_test: 只读飞书列、解绑遗留绑定的幂等/失败路径、绑定入口已移除 全部通过');
