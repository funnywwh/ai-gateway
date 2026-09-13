// Node-only regression test for the model reasoning form. It loads models.js as an
// ES module with UI/API dependencies mocked, so it verifies the actual submit
// callbacks without requiring a browser engine.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/models.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');
const calls = [];
const modals = [];
const tables = [];

function node(tag, props = {}) {
  return {
    tag, ...props, children: [], listeners: {},
    append(...children) { this.children.push(...children); },
    addEventListener(name, listener) { this.listeners[name] = listener; },
  };
}
const api = {
  async get(path) {
    if (path === '/providers') return { data: [{ id: 9, name: 'shared' }] };
    if (path === '/models') return { data: [{ id: 1, public_name: 'existing' }] };
    if (path === '/routes') return { data: [] };
    throw new Error(`unexpected GET ${path}`);
  },
  async post(path, body) { calls.push({ method: 'post', path, body }); return {}; },
  async patch(path, body) { calls.push({ method: 'patch', path, body }); return {}; },
  errorMessage(err) { return String(err); },
};
const ui = {
  el: node,
  card: (...args) => ({ tag: 'card', args }),
  pagedTable(config) {
    const table = { node: node('table'), config, refreshes: 0, async refresh() { this.refreshes++; await config.load({ limit: 20, offset: 0 }); } };
    tables.push(table);
    return table;
  },
  async modal(spec) { modals.push(spec); return spec; },
  toast() {}, badge: (text) => text, async confirmDialog() { return false; },
};

const context = vm.createContext({ console });
const apiModule = new vm.SourceTextModule('export const api = globalThis.__api;', { context });
const uiModule = new vm.SourceTextModule(`
  export const el = globalThis.__ui.el;
  export const card = globalThis.__ui.card;
  export const pagedTable = globalThis.__ui.pagedTable;
  export const modal = globalThis.__ui.modal;
  export const toast = globalThis.__ui.toast;
  export const badge = globalThis.__ui.badge;
  export const confirmDialog = globalThis.__ui.confirmDialog;
`, { context });
context.__api = api;
context.__ui = ui;
const module = new vm.SourceTextModule(source, { context, identifier: sourceURL.href });
await module.link((specifier) => {
  if (specifier === '../api.js') return apiModule;
  if (specifier === '../ui.js') return uiModule;
  throw new Error(`unexpected import ${specifier}`);
});
await module.evaluate();

const page = node('page');
const actions = node('actions');
await module.namespace.render({ page, actions, session: { role: 'admin' } });
assert.equal(tables.length, 2, 'render creates model and route tables');
const create = actions.children.find((child) => child.text === '新建模型');
await create.listeners.click();
const createForm = modals.at(-1);
assert.equal(createForm.fields.find((field) => field.name === 'reasoning_mode').value, undefined);
assert.equal(createForm.fields.find((field) => field.name === 'reasoning_effort').label, '推理强度（默认/强制模式生效）');
assert.match(createForm.fields.find((field) => field.name === 'reasoning_mode').hint, /包括 none/);
assert.match(createForm.fields.find((field) => field.name === 'reasoning_mode').hint, /保留请求的 summary/);
assert.match(createForm.fields.find((field) => field.name === 'reasoning_mode').hint, /所有路由/);
assert.match(createForm.fields.find((field) => field.name === 'reasoning_effort').hint, /上游拒绝请求/);
await createForm.onSubmit({
  public_name: 'new-model', display_name: '', aliases: [],
  reasoning_mode: 'force', reasoning_effort: 'high',
});
assert.equal(JSON.stringify(calls.at(-1)), JSON.stringify({
  method: 'post', path: '/models',
  body: { public_name: 'new-model', display_name: '', aliases: [], reasoning: { mode: 'force', effort: 'high' } },
}));
assert.ok(tables[0].refreshes >= 2, 'create submission rerenders the model table');

const row = { public_name: 'existing', display_name: 'Existing', enabled: true, aliases: [], reasoning: { mode: 'force', effort: 'high' } };
const edit = tables[0].config.rowActions(row)[0];
await edit.onclick();
const editForm = modals.at(-1);
assert.equal(editForm.fields.find((field) => field.name === 'reasoning_mode').value, 'force');
assert.equal(editForm.fields.find((field) => field.name === 'reasoning_effort').value, 'high');
await editForm.onSubmit({
  display_name: 'Existing', enabled: true, aliases: [], sale_pricing: undefined,
  reasoning_mode: 'inherit', reasoning_effort: 'medium',
});
assert.equal(JSON.stringify(calls.at(-1)), JSON.stringify({
  method: 'patch', path: '/models/existing',
  body: { display_name: 'Existing', enabled: true, aliases: [], reasoning: null },
}));
assert.ok(tables[0].refreshes >= 3, 'inherit clear submission rerenders the model table');
console.log('models UI reasoning regression passed');
