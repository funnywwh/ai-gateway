// Node-only regression test for the API Keys page's Feishu binding (M60). It loads
// keys.js as an ES module with UI/API/base dependencies mocked, so it verifies the actual
// request the page sends and the message it shows for every result the callback can return.
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
assert.equal(keysColumn.label, '飞书');
const bound = keysColumn.render({ feishu: { bound: true, open_id: 'ou_alice', name: '张三' } });
assert.equal(bound.text, '张三', 'a bound key shows the Feishu name');
assert.match(bound.title, /ou_alice/, 'the full open id belongs in the tooltip');
const unbound = keysColumn.render({ feishu: { bound: false } });
assert.equal(unbound.text, '未绑定');
// A row without the field at all (an older server) must render, not throw.
assert.equal(keysColumn.render({}).text, '未绑定');

const boundActions = table.rowActions({ id: 7, name: 'alice-key', status: 'active', feishu: { bound: true, name: '张三' } });
const unboundActions = table.rowActions({ id: 8, name: 'bob-key', status: 'active', feishu: { bound: false } });
assert.ok(boundActions.some((action) => action.text === '绑定飞书'), 'every key offers binding');
assert.ok(boundActions.some((action) => action.text === '解绑飞书'), 'a bound key offers unbinding');
assert.ok(!unboundActions.some((action) => action.text === '解绑飞书'), 'an unbound key must not offer unbinding');
// Read-only operators get no write actions at all, Feishu included.
await renderPage();
const viewerActions = table.rowActions({ id: 7, name: 'alice-key', status: 'active', feishu: { bound: true } });
assert.equal(viewerActions.length, 4, 'the admin row keeps its actions');
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

// --- binding navigates to the management endpoint, which redirects to Feishu ---------------

await renderPage();
const bindAction = table.rowActions({ id: 7, name: 'alice-key', status: 'active', feishu: { bound: true } })
  .find((action) => action.text === '绑定飞书');
navigations.length = 0;
bindAction.onclick();
assert.deepEqual(navigations, ['/admin/api/v1/keys/7/feishu/bind'],
  'binding must be a top-level navigation to the endpoint that redirects to Feishu');

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

// --- the callback's result code becomes a message, once, and the parameter is dropped ------

const cases = {
  bound: { level: 'ok', match: /已绑定/ },
  replaced: { level: 'ok', match: /改绑/ },
  cancelled: { level: 'error', match: /取消/ },
  conflict: { level: 'error', match: /另一把 Key/ },
  rejected: { level: 'error', match: /管理员/ },
  expired: { level: 'error', match: /过期/ },
  invalid: { level: 'error', match: /校验/ },
  rate_limited: { level: 'error', match: /频繁/ },
  no_app_permission: { level: 'error', match: /使用权限/ },
  app_error: { level: 'error', match: /飞书应用/ },
  error: { level: 'error', match: /失败/ },
};
for (const [code, expected] of Object.entries(cases)) {
  toasts.length = 0;
  navigations.length = 0;
  await renderPage(new URLSearchParams('feishu=' + code));
  assert.equal(toasts.length, 1, code + ': exactly one message is shown');
  assert.equal(toasts[0].level, expected.level, code + ': wrong level');
  assert.match(toasts[0].text, expected.match, code + ': wrong message');
  assert.ok(navigations.includes('#/keys'), code + ': the parameter must be dropped after reporting');
}
// An unknown code is reported as-is: silently ignoring it would hide a real protocol change.
toasts.length = 0;
await renderPage(new URLSearchParams('feishu=surprise'));
assert.equal(toasts.length, 1);
assert.match(toasts[0].text, /surprise/, 'an unknown result code must be shown, not swallowed');
assert.equal(toasts[0].level, 'error');

// Binding also opts the account in to DSH. That outcome travels as its own parameter, so the
// administrator learns whether the person can sign in yet — including when it deliberately
// did not happen.
const dshCases = {
  enabled: { level: 'ok', match: /已自动启用该账号的 DSH/ },
  already: { level: 'ok', match: /此前已启用 DSH/ },
  declined: { level: 'error', match: /曾被显式停用 DSH/ },
  failed: { level: 'error', match: /自动启用 DSH 失败/ },
};
for (const [state, expected] of Object.entries(dshCases)) {
  toasts.length = 0;
  await renderPage(new URLSearchParams('feishu=bound&key=7&dsh=' + state + '&tenant=dsh-alice&dsh_reason=boom'));
  assert.equal(toasts.length, 2, state + ': the identity result and the DSH outcome are both reported');
  assert.equal(toasts[1].level, expected.level, state + ': wrong level');
  assert.match(toasts[1].text, expected.match, state + ': wrong message');
}
// "off" means the deployment does not do it: silence is correct, not an error.
toasts.length = 0;
await renderPage(new URLSearchParams('feishu=bound&key=7&dsh=off'));
assert.equal(toasts.length, 1, 'dsh=off must not add a second message');
// An unknown state is surfaced rather than swallowed.
toasts.length = 0;
await renderPage(new URLSearchParams('feishu=bound&key=7&dsh=surprise'));
assert.match(toasts.map((t) => t.text).join(' '), /surprise/, 'an unknown DSH state must be shown');

// Without the parameter nothing is shown at all.
toasts.length = 0;
await renderPage();
assert.equal(toasts.length, 0, 'a plain visit must not show a Feishu message');

// --- the page never builds a Feishu request of its own ------------------------------------

assert.doesNotMatch(source, /keys\/' \+ row\.id \+ '\/feishu' \+ '\?/, 'the page must not guess the bind URL shape');
assert.doesNotMatch(source, /app_secret|access_token|code_verifier/, 'the console must never see a Feishu credential');

console.log('keys_test: 飞书绑定列、绑定/解绑动作、11 个结果码与幂等/失败路径全部通过');
