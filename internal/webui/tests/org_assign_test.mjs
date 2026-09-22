// Node-only regression test for the organization picker (勾选树). It loads pages/org_assign.js as an
// ES module with the UI, API, tree and pinyin dependencies mocked, so what it pins is the wiring an
// operator depends on:
//
//   * the picker reads the node list itself (one source, not the page's possibly stale copy);
//   * the account's current nodes are pre-ticked, and ticking a node does NOT tick its children
//     (membership is a direct relationship per node — the subtree inheritance belongs to the node's
//     TAGS, not to its members);
//   * saving hands back the whole list, sorted, never a delta (the API replaces `org_node_ids`);
//   * an empty selection is a decision with consequences, and it says so on screen;
//   * a failed write keeps the dialog open (the operator can fix it) instead of closing on an error.
//
// The rendering itself is exercised in a real browser by scripts/ui-harness (view `org-person`).
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/org_assign.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');

const calls = [];
let nodes = [];
let readError = null;
let writeError = null;

// The DOM stand-in mirrors ui.js's `el(tag, attrs, children)` contract for the attributes this
// module uses (class/text/type/onclick), plus what a checkbox needs to behave like one: click()
// toggles `checked` and fires click + change, exactly as a browser does.
function node(tag, attrs = {}, children = []) {
  const el = {
    tag, children: [], listeners: {}, attributes: {},
    className: attrs.class || '',
    textContent: attrs.text || '',
    checked: !!attrs.checked,
    disabled: false,
    classList: {
      add() {}, remove() {},
      contains(name) { return el.className.split(/\s+/).includes(name); },
    },
    append(...items) { el.children.push(...items.flat()); },
    replaceChildren(...items) { el.children = items.flat(); },
    setAttribute(name, value) { el.attributes[name] = value; },
    addEventListener(name, listener) { (el.listeners[name] = el.listeners[name] || []).push(listener); },
    fire(name, ev) { for (const listener of el.listeners[name] || []) listener(ev || { currentTarget: el, target: el, stopPropagation() {} }); },
    click() {
      if (el.tag === 'input' && el.type === 'checkbox') el.checked = !el.checked;
      el.fire('click');
      if (el.tag === 'input' && el.type === 'checkbox') el.fire('change');
      if (typeof attrs.onclick === 'function') attrs.onclick({ currentTarget: el, target: el, stopPropagation() {} });
    },
    remove() { el.removed = true; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
  for (const [key, value] of Object.entries(attrs)) {
    if (key === 'class') el.className = value;
    else if (key === 'text') el.textContent = value;
    else if (key === 'type') el.type = value;
    else if (key !== 'onclick') el[key] = value;
  }
  el.append(children || []);
  return el;
}

const api = {
  async get(path, params) {
    calls.push({ method: 'GET', path, params });
    if (path === '/org/nodes') {
      if (readError) throw new Error(readError);
      return { data: nodes };
    }
    throw new Error('unexpected GET ' + path);
  },
  errorMessage: (err) => (err && err.message) || String(err),
};

let treeInstance = null;
const ui = {
  el: node,
  modalHead: (title) => node('div', { text: title }),
  modalBody: (children) => node('div', {}, children),
  modalActions: (children) => node('div', {}, children),
};

const modalRoot = node('div');
const context = vm.createContext({
  console, Error, Promise, Node: class Node {}, setInterval, clearInterval, Date,
  document: {
    createElement: (tag) => node(tag),
    createTextNode: (text) => node('#text', { text }),
    getElementById: (id) => (id === 'modal-root' ? modalRoot : null),
  },
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
const linker = async (specifier) => {
  if (specifier === '../api.js') return new vm.SyntheticModule(['api'], function () { this.setExport('api', api); }, { context });
  if (specifier === '../ui.js') return new vm.SyntheticModule(
    ['el', 'modalHead', 'modalBody', 'modalActions'],
    function () { for (const [name, value] of Object.entries(ui)) this.setExport(name, value); }, { context });
  if (specifier === '../tree.js') return new vm.SyntheticModule(['tree'], function () {
    this.setExport('tree', (options) => {
      treeInstance = { options, nodes: [] };
      const handle = {
        node: node('div', { class: 'tree' }),
        refresh(next) { treeInstance.nodes = next || []; },
        setSelected() {}, expandAll() {}, collapseAll() {},
      };
      treeInstance.handle = handle;
      return handle;
    });
  }, { context });
  if (specifier === '../pinyin.js') return new vm.SyntheticModule(['matchesQuery'], function () {
    this.setExport('matchesQuery', (text, query) => String(text || '').toLowerCase().includes(String(query || '').trim().toLowerCase()));
  }, { context });
  throw new Error('unexpected import ' + specifier);
};
await module.link(linker);
await module.evaluate();

const { openOrgPicker } = module.namespace;
const settle = async () => { for (let i = 0; i < 60; i++) await Promise.resolve(); };

// The picker builds its rows through renderLabel; asking it for a node's row is how the test reaches
// the checkbox the operator would click (the same object the dialog keeps in its registry).
function boxOf(id) {
  const row = treeInstance.options.renderLabel(nodes.find((entry) => entry.id === id));
  const box = row.children.find((child) => child.tag === 'input');
  assert.ok(box, 'every row must carry a checkbox');
  return box;
}

function lastDialog() {
  const dialog = modalRoot.children[modalRoot.children.length - 1];
  assert.ok(dialog, 'the picker must append a dialog');
  return dialog;
}

function findByClass(entry, className, out = []) {
  if (!entry) return out;
  if (typeof entry.className === 'string' && entry.className.split(/\s+/).includes(className)) out.push(entry);
  for (const child of entry.children || []) findByClass(child, className, out);
  return out;
}

function button(entry, label) {
  if (!entry) return null;
  if (entry.tag === 'button' && entry.textContent === label) return entry;
  for (const child of entry.children || []) {
    const found = button(child, label);
    if (found) return found;
  }
  return null;
}

function textOf(entry, out = []) {
  if (!entry) return out;
  if (typeof entry.textContent === 'string' && entry.textContent) out.push(entry.textContent);
  for (const child of entry.children || []) textOf(child, out);
  return out.join(' ');
}

// --- the picker reads the node list and pre-tickets the current memberships --------------------

nodes = [
  { id: 1, parent_id: null, name: '总部', path: '总部', depth: 0 },
  { id: 2, parent_id: 1, name: '研发部', path: '总部/研发部', depth: 1 },
  { id: 3, parent_id: 2, name: '平台组', path: '总部/研发部/平台组', depth: 2 },
  { id: 4, parent_id: 1, name: '市场部', path: '总部/市场部', depth: 1 },
];

calls.length = 0;
const picked = openOrgPicker({ title: '分配组织 — acme', nodeIds: [4] });
await settle();

// 逐个字段比较而不是 deepEqual：这些对象是在 VM 里造出来的，原型与测试所在的 realm 不同，
// 严格深比较会因此报错（把"跨 realm"误报成"值不对"）。
assert.equal(calls[0].method, 'GET', 'the picker reads the node list itself');
assert.equal(calls[0].path, '/org/nodes', 'and it reads the node endpoint itself, not a cached copy');
assert.equal(calls[0].params.limit, 1000, 'with the same limit the org page uses');
assert.equal(treeInstance.nodes.length, 4, 'every node reaches the control');
assert.equal(boxOf(4).checked, true, 'the account\'s current node is ticked');
assert.equal(boxOf(2).checked, false, 'and the others are not');
assert.match(textOf(lastDialog()), /分配组织 — acme/);

// --- ticking a node does not tick its children (membership is per node) ------------------------

boxOf(2).click();
await settle();
assert.equal(boxOf(2).checked, true, 'the clicked node is ticked');
assert.equal(boxOf(3).checked, false,
  'ticking a parent must not tick its subtree: the subtree inherits the node\'s TAGS, not its members');

// --- saving hands back the whole list, sorted --------------------------------------------------

const saved = button(lastDialog(), '保存');
saved.click();
await settle();
const result = await picked;
assert.equal(result.ids.join(','), '2,4', 'saving replaces the list: both nodes, in a stable order');
assert.equal(result.refs.length, 2, 'the caller gets the node references, so a form can show paths');
assert.equal(result.refs.map((ref) => ref.path).join(' | '), '总部/研发部 | 总部/市场部',
  'the references carry the label path, so a form row can show where the account went');

// --- an empty selection says what it means -----------------------------------------------------

calls.length = 0;
const empty = openOrgPicker({ title: '分配组织 — acme', nodeIds: [4] });
await settle();
button(lastDialog(), '清空').click();
await settle();
const clearedDialog = textOf(lastDialog());
assert.match(clearedDialog, /未选择任何节点/, 'clearing every box is a decision, and it must be stated');
assert.match(clearedDialog, /标签授权随即失效/, 'including what it costs (the node tags stop applying)');
button(lastDialog(), '保存').click();
await settle();
assert.equal((await empty).ids.length, 0, 'an empty selection is an empty list, never "leave it alone"');

// --- cancel writes nothing ---------------------------------------------------------------------

const cancelled = openOrgPicker({ title: '分配组织 — acme', nodeIds: [4] });
await settle();
button(lastDialog(), '取消').click();
assert.equal(await cancelled, null, 'cancelling reports "nothing chosen" to the caller');

// --- the write is the caller's, and a failure keeps the dialog open ----------------------------

calls.length = 0;
let submitted = null;
writeError = null;
const writing = openOrgPicker({
  title: '分配组织 — acme',
  nodeIds: [4],
  onSubmit: async (ids) => {
    submitted = ids;
    if (writeError) throw new Error(writeError);
  },
});
await settle();
boxOf(2).click();
await settle();
button(lastDialog(), '保存').click();
await settle();
assert.equal(submitted.join(','), '2,4', 'the caller is handed the same whole list to persist');
assert.equal((await writing).ids.join(','), '2,4', 'and the picker resolves with what was written');

let failureSettled = false;
writeError = '组织节点 2 不存在';
const failing = openOrgPicker({
  title: '分配组织 — acme', nodeIds: [4], onSubmit: async () => { throw new Error(writeError); },
});
failing.then(() => { failureSettled = true; });
await settle();
button(lastDialog(), '保存').click();
await settle();
assert.equal(failureSettled, false, 'a failed write must NOT close the dialog (the operator has to fix it)');
assert.match(textOf(lastDialog()), /组织节点 2 不存在/, 'the server\'s reason is shown inside the dialog');
const retry = button(lastDialog(), '保存');
assert.equal(retry.disabled, false, 'and the dialog is usable again');
button(lastDialog(), '取消').click();
assert.equal(await failing, null);

// --- a failed read disables saving (never a silent empty tree) ---------------------------------

nodes = [];
readError = '组织节点读不到';
const broken = openOrgPicker({ title: '分配组织 — acme', nodeIds: [4] });
await settle();
const brokenDialog = lastDialog();
assert.match(textOf(brokenDialog), /组织节点读不到/, 'a failed read says so instead of showing an empty tree');
assert.equal(button(brokenDialog, '保存').disabled, true, 'and saving is refused while the list is unknown');
button(brokenDialog, '取消').click();
assert.equal(await broken, null);
readError = null;

console.log('org_assign_test: 勾选树读节点、预勾选、每节点独立、整表替换、空选后果、失败留在弹窗内 全部通过');
