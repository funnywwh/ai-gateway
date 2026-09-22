// Node-only regression test for the account-level Feishu person picker (M72). It loads
// account_feishu.js as an ES module with the UI/API dependencies mocked, so what it pins is the
// two facts an operator depends on: the picker reads the Feishu DIRECTORY (not a person-by-id
// lookup), and the write goes to the account-level route with the person the operator chose —
// including the refusals that must never reach the API at all.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/account_feishu.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');

const calls = [];
const toasts = [];
const confirmations = [];
const dialogs = [];
let confirmAnswer = true;
let directory = null;
let directoryError = null;

// The DOM stand-in mirrors ui.js's `el(tag, attrs, children)` contract: attributes are copied
// (with `class`→className and `text`→textContent, exactly as the real helper does) and children
// are appended in the constructor. Getting this wrong would make the dialog look empty to the
// test while the page renders fine.
function node(tag, attrs = {}, children = []) {
  const el = {
    tag, children: [], listeners: {}, dataset: {}, attributes: {},
    className: attrs.class || '',
    textContent: attrs.text || '',
    classList: {
      add(name) { el.className = (el.className + ' ' + name).trim(); },
      remove() {}, toggle() { return false; },
      contains(name) { return el.className.split(/\s+/).includes(name); },
    },
    append(...items) { el.children.push(...items.flat()); },
    addEventListener(name, listener) { el.listeners[name] = listener; },
    replaceChildren(...items) { el.children = items.flat(); },
    setAttribute(name, value) { el.attributes[name] = value; },
    remove() { el.removed = true; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
  for (const [key, value] of Object.entries(attrs)) {
    if (key === 'class') el.className = value;
    else if (key === 'text') el.textContent = value;
    else el[key] = value;
  }
  el.append(children || []);
  return el;
}

// holdDirectory 让这次读挂住不返回：只有把请求按在手里，才能观察到"弹窗已经打开、还在读"那一瞬
// （读得快时这个状态一帧都不存在，而用户报障的正是这个瞬间"点了没反应"）。
let holdDirectory = false;
let releaseDirectory = null;

const api = {
  async get(path, params) {
    calls.push({ method: 'GET', path, params });
    if (path === '/org/feishu/directory') {
      if (directoryError) throw new Error(directoryError);
      if (holdDirectory) {
        return await new Promise((resolve) => {
          releaseDirectory = () => { holdDirectory = false; resolve(directory); };
        });
      }
      return directory;
    }
    throw new Error('unexpected GET ' + path);
  },
  async put(path, body) { calls.push({ method: 'PUT', path, body }); return { ok: true, result: 'bound' }; },
  async del(path) { calls.push({ method: 'DELETE', path }); return { unbound: true, account_id: 7 }; },
  errorMessage: (err) => (err && err.message) || String(err),
};

const ui = {
  el: node,
  modal: async () => null,
  toast: (text, level) => toasts.push({ text, level }),
  badge: (text, kind) => node('span', { class: 'badge ' + (kind || ''), text }),
  confirmDialog: async (title) => { confirmations.push(title); return confirmAnswer; },
  // 与 ui.js 的真实形状一致：modalBody/modalActions 收的是一个子元素数组。
  modalHead: (title) => node('div', { text: title }),
  modalBody: (children) => node('div', { children: children || [] }),
  modalActions: (children) => node('div', { children: children || [] }),
  withBusy: async (button, _label, fn) => fn(),
  // 与 ui.js 的真实形状一致：progressLine 返回 { nodes, stop }，withBusy 也是用它实现的。
  progressLine: (label) => {
    progressLabels.push(label);
    return { nodes: [node('span', { class: 'spinner' }), node('span', { text: label + '…' })], stop() { stops.push(label); } };
  },
};
const progressLabels = [];
const stops = [];

const modalRoot = node('div');
const context = vm.createContext({
  console,
  Error,
  Promise,
  Node: class Node {},
  document: {
    createElement: (tag) => node(tag),
    createTextNode: (text) => node('#text', { text }),
    getElementById: (id) => (id === 'modal-root' ? modalRoot : null),
  },
  navigator: {},
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
const linker = async (specifier) => {
  if (specifier === '../api.js') return new vm.SyntheticModule(['api'], function () { this.setExport('api', api); }, { context });
  if (specifier === '../ui.js') return new vm.SyntheticModule(
    ['el', 'modal', 'toast', 'badge', 'confirmDialog', 'modalHead', 'modalBody', 'modalActions', 'withBusy', 'progressLine'],
    function () {
      for (const [name, value] of Object.entries(ui)) this.setExport(name, value);
    }, { context });
  if (specifier === '../pinyin.js') return new vm.SyntheticModule(['matchesQuery'], function () {
    // A deliberately simple matcher: the pinyin table itself is exercised by its own test.
    this.setExport('matchesQuery', (value, query) => String(value || '').toLowerCase().includes(String(query || '').toLowerCase()));
  }, { context });
  throw new Error('unexpected import ' + specifier);
};
await module.link(linker);
await module.evaluate();

const { openFeishuPersonPicker, unbindAccountFeishu } = module.namespace;

// The picker opens a dialog on the modal root and returns a promise that settles when it is
// closed; the test drives it by reaching into the last dialog it appended.
function lastDialog() {
  const dialog = modalRoot.children[modalRoot.children.length - 1];
  assert.ok(dialog, 'the picker must append a dialog');
  return dialog;
}

function collectText(entry, out = []) {
  if (!entry) return out;
  if (typeof entry.textContent === 'string' && entry.textContent) out.push(entry.textContent);
  for (const child of entry.children || []) collectText(child, out);
  return out;
}

// --- 弹窗先出现、通讯录后到（用户报障：加载人员要先弹出框、显示进度） -------------------------
//
// 这一段的证据只能来自"请求还按在手里"的那一刻：读得快的时候，"弹窗已开、正在读"这个状态一帧都
// 不存在，任何在读完成之后做的断言都会全绿——而那正是旧实现的形态（先 await 读、读完才建弹窗，
// 慢的时候屏幕上什么都没有）。
const heldCalls = calls.length;
holdDirectory = true;
progressLabels.length = 0;
directory = { names_available: true, users: [{ open_id: 'ou_a', name: '李智超', account: null }] };
const slowPicker = openFeishuPersonPicker({ account: { id: 7, name: 'acme' } });
await new Promise((resolve) => setTimeout(resolve, 0));
{
  const picker = lastDialog();
  assert.ok(picker, 'the dialog must exist BEFORE the directory read finishes');
  assert.ok(progressLabels.includes('正在读取飞书通讯录'),
    'and it must say it is reading (a spinner + a sentence + seconds, not a silent wait)');
  assert.equal(findByClass(picker, 'feishu-picker-row').length, 0, 'no rows can exist yet: the read is still out');
  assert.equal(findButton(picker, '绑定').disabled, true, 'there is nothing to bind yet');
  assert.equal(findButton(picker, '刷新').disabled, true, 'and the read cannot be started twice');
  assert.equal(calls[heldCalls].params, undefined, 'the first read uses the server cache');
}
releaseDirectory();
await new Promise((resolve) => setTimeout(resolve, 0));
assert.equal(findByClass(lastDialog(), 'feishu-picker-row').length, 1, 'the rows arrive when the read does');
assert.ok(stops.includes('正在读取飞书通讯录'), 'the progress line is stopped when the read settles');
assert.equal(findButton(lastDialog(), '刷新').disabled, false, 'and the read can be repeated');
press(findButton(lastDialog(), '刷新'));
await new Promise((resolve) => setTimeout(resolve, 0));
assert.equal(calls.at(-1).params.refresh, 'true',
  '刷新 bypasses the 60 s cache: somebody added in Feishu five seconds ago has to show up now');
press(findButton(lastDialog(), '取消'));
assert.equal(await slowPicker, false);
directory = null;

// --- the picker reads the directory and offers every person ---------------------------------

directory = {
  names_available: true,
  users: [
    { open_id: 'ou_a', name: '李智超', account: null },
    { open_id: 'ou_b', name: '王五', account: { id: 9, name: '老板' } },
    { open_id: 'ou_c', name: '张三', account: { id: 7, name: 'acme' } },
  ],
};
calls.length = 0;
const pickerPromise = openFeishuPersonPicker({ account: { id: 7, name: 'acme' } });
await new Promise((resolve) => setTimeout(resolve, 0));
assert.equal(calls[0].method, 'GET', 'the picker must read the directory');
assert.equal(calls[0].path, '/org/feishu/directory',
  'the person list and the current bindings come from the directory, not a per-person lookup');
assert.equal(calls[0].params, undefined,
  'the first read takes the server cache: refresh=true is for the 刷新 button (and for retrying)');

// 人员列表是弹窗里那个带 class 的容器：测试从它取文本与单选框，而不是猜层级。
function findByClass(entry, className, out = []) {
  if (!entry) return out;
  if (typeof entry.className === 'string' && entry.className.split(/\s+/).includes(className)) out.push(entry);
  for (const child of entry.children || []) findByClass(child, className, out);
  return out;
}

const dialog = lastDialog();
const listRows = findByClass(dialog, 'feishu-picker-row');
const text = listRows.map((row) => collectText(row).join(' ')).join(' | ');
for (const want of ['李智超', '王五', '张三', 'ou_a', 'ou_b', 'ou_c']) {
  assert.ok(text.includes(want), 'the person list must contain ' + want + ': ' + text);
}
assert.ok(text.includes('已绑定「老板」'), 'a person taken by another account must say which account: ' + text);
// The person already bound to THIS account is selectable (re-binding is a no-op), the one on
// another account is not: the disabled state has to be visible in the row itself.
const radios = [];
(function collectRadios(entry) {
  if (!entry) return;
  if (entry.tag === 'input' && entry.type === 'radio') radios.push(entry);
  for (const child of entry.children || []) collectRadios(child);
})(lastDialog());
assert.equal(radios.length, 3, 'one radio button per person');
assert.equal(radios.filter((box) => box.disabled).length, 1, 'only the person held by another account is disabled');
// 关闭弹窗（取消）不写任何东西，并把 false 交给调用方。取消按钮是弹窗里的那个「取消」。
// press clicks a button whichever way it was wired: the dialog uses onclick attributes for the plain
// buttons and addEventListener for the one that shows progress (绑定), and a test that only knows the
// first shape would silently do nothing on the second.
function press(entry) {
  if (!entry) return;
  if (typeof entry.onclick === 'function') { entry.onclick({ currentTarget: entry, target: entry }); return; }
  const handler = entry.listeners && entry.listeners.click;
  if (handler) handler({ currentTarget: entry, target: entry, stopPropagation() {} });
}

function findButton(entry, label) {
  if (!entry) return null;
  if (entry.tag === 'button' && entry.textContent === label) return entry;
  for (const child of entry.children || []) {
    const found = findButton(child, label);
    if (found) return found;
  }
  return null;
}
press(findButton(lastDialog(), '取消'));
assert.equal(await pickerPromise, false, 'closing the picker without choosing reports no binding');

// --- choosing a person writes the account-level route ---------------------------------------

calls.length = 0;
toasts.length = 0;
let bound = null;
const bindPromise = openFeishuPersonPicker({ account: { id: 7, name: 'acme' }, onBound: (person) => { bound = person; } });
await new Promise((resolve) => setTimeout(resolve, 0));
{
  const picker = lastDialog();
  const boxes = [];
  (function collect(entry) {
    if (!entry) return;
    if (entry.tag === 'input' && entry.type === 'radio') boxes.push(entry);
    for (const child of entry.children || []) collect(child);
  })(picker);
  const first = boxes.find((box) => !box.disabled);
  first.checked = true;
  first.listeners.change();
  // The dialog's primary button is the last button in the action row.
  const buttons = [];
  (function collectButtons(entry) {
    if (!entry) return;
    if (entry.tag === 'button') buttons.push(entry);
    for (const child of entry.children || []) collectButtons(child);
  })(picker);
  const confirm = buttons.find((button) => button.textContent === '绑定');
  assert.ok(confirm, 'the picker must offer a 绑定 button');
  press(confirm);
  await new Promise((resolve) => setTimeout(resolve, 0));
}
const write = calls.find((call) => call.method === 'PUT');
assert.ok(write, 'choosing a person must write the binding');
assert.equal(write.path, '/accounts/7/feishu', 'the write goes to the account-level route (M72)');
assert.equal(write.body.open_id, 'ou_a');
assert.equal(write.body.name, '李智超');
assert.equal(await bindPromise, true);
assert.ok(bound && bound.open_id === 'ou_a', 'the caller is told who was bound');
assert.equal(toasts.at(-1).level, 'ok');

// --- a directory failure is reported INSIDE the dialog, and nothing is written ---------------

calls.length = 0;
toasts.length = 0;
directoryError = '飞书不可达';
const failing = openFeishuPersonPicker({ account: { id: 7, name: 'acme' } });
await new Promise((resolve) => setTimeout(resolve, 0));
{
  const picker = lastDialog();
  assert.ok(picker, 'a failed read must still leave the dialog on screen');
  assert.match(collectText(picker).join(' '), /飞书不可达/, 'the server reason is shown in the dialog');
  assert.match(collectText(picker).join(' '), /刷新/, 'and the dialog offers the retry');
  assert.equal(findButton(picker, '绑定').disabled, true, 'with nothing to bind, the button stays disabled');
}
assert.ok(!calls.some((call) => call.method === 'PUT'), 'a failed read must not write anything');
press(findButton(lastDialog(), '取消'));
assert.equal(await failing, false, 'closing a failed picker reports no binding');
directoryError = null;

// --- an empty directory explains what to fix (also in the dialog) ----------------------------

toasts.length = 0;
directory = { names_available: true, users: [] };
const emptyPicker = openFeishuPersonPicker({ account: { id: 7, name: 'acme' } });
await new Promise((resolve) => setTimeout(resolve, 0));
assert.match(collectText(lastDialog()).join(' '), /权限/, 'an empty directory must point at the data permission');
press(findButton(lastDialog(), '取消'));
assert.equal(await emptyPicker, false);

// --- unbinding asks first, is idempotent, and reports honestly ------------------------------

toasts.length = 0;
confirmations.length = 0;
calls.length = 0;
confirmAnswer = false;
assert.equal(await unbindAccountFeishu({ id: 7, name: 'acme', feishu: { bound: true, name: '张三' } }), false);
assert.equal(confirmations.length, 1, 'unbinding must ask first');
assert.equal(calls.length, 0, 'declining must not call the API');

confirmAnswer = true;
calls.length = 0;
assert.equal(await unbindAccountFeishu({ id: 7, name: 'acme', feishu: { bound: true, name: '张三' } }), true);
assert.deepEqual(calls, [{ method: 'DELETE', path: '/accounts/7/feishu' }], 'unbinding goes to the account route');
assert.equal(toasts.at(-1).text, '已解绑');

// An idempotent answer is reported as such rather than as a change.
api.del = async (path) => { calls.push({ method: 'DELETE', path }); return { unbound: false, account_id: 7 }; };
calls.length = 0;
toasts.length = 0;
await unbindAccountFeishu({ id: 7, name: 'acme', feishu: { bound: true } });
assert.equal(toasts.at(-1).text, '该账号本来就没有绑定');

console.log('account_feishu_test: 人员弹窗读通讯录、账号级绑定写入、已占用者不可选、解绑幂等 全部通过');
