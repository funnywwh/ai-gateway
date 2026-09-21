// Static regression checks for the organization page as the person/account surface (M72).
//
// The organization page is where an operator now does per-account work: a person row carries the
// membership checkbox, that account's DSH state, its Feishu identity and its key count, and
// expanding it reveals the account's keys and the operations on them. Those facts are wiring —
// which endpoint is called, which write is reachable, what a read-only role must not get — and
// wiring is exactly what a text assertion can protect without a browser. The rendering itself is
// exercised by scripts/ui-harness (`org` and `org-person` views).
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const org = await readFile(new URL('../static/js/pages/org.js', import.meta.url), 'utf8');
const picker = await readFile(new URL('../static/js/pages/account_feishu.js', import.meta.url), 'utf8');
const keyActions = await readFile(new URL('../static/js/pages/key_actions.js', import.meta.url), 'utf8');
const css = await readFile(new URL('../static/app.css', import.meta.url), 'utf8');

// --- the person row: account facts next to the membership checkbox ---------------------------

// 人员列表是一张多列表格：列定义只写一处，表头 / colspan / 两种模式都从它推出。
assert.match(org, /const MEMBER_COLUMNS = \[/, 'the person table columns must be declared once');
assert.match(org, /function personTable\(\{ pickable, checked = new Set\(\), filtering = \(\) => false, onToggle = \(\) => \{\} \}\) \{/,
  'the person table must be its own function: it renders the rows, their expansion and their state');
for (const label of ["'账号'", "'DSH'", "'飞书'", "'Key'", "'所属组织'", "'操作'"]) {
  assert.ok(org.includes('label: ' + label), 'the table must have a ' + label + ' column');
}
assert.match(org, /entry\.row = el\('tr', \{\s*class: 'org-person org-member',\s*dataset: \{ accountId: String\(account\.id\) \},/,
  'a person row is a table row that still carries .org-member (the row layout rules key off it)');
assert.match(org, /el\('tr', \{\s*class: 'org-person-detail',\s*dataset: \{ accountId: String\(account\.id\) \},/,
  'each account gets its own detail row, addressable by account id');
assert.match(org, /el\('span', \{ class: 'org-member-name', text: account\.name \}\)/,
  'the row keeps the account name in its own class (the layout rules key off it)');
assert.match(org, /el\('span', \{ class: 'org-member-name', text: account\.name \}\)/,
  'the row keeps the account name in its own class (the layout rules key off it)');
for (const badge of ['dshBadge', 'feishuBadge', 'keyBadge']) {
  assert.match(org, new RegExp(badge + '\\(account\\)'), 'the row must show ' + badge);
}
// The three DSH states are the whole reason the badge exists: enabled, explicitly disabled by an
// administrator, and never enabled (which is what auto_enable turns into "usable at login").
assert.match(org, /account\.dsh_enabled\) return badge\('DSH 已启用'/, 'an enabled account says so');
assert.match(org, /account\.dsh_disabled_at\) return badge\('DSH 已停用（管理员）'/, 'an explicit disable is distinguishable');
assert.match(org, /account\.dsh_effective\) \{[\s\S]*?badge\('DSH 登录即可用', 'ok'\)/, 'the auto-enabled case is stated, not implied');
// 自动启用的前提（账号要有可用模型）必须写在行里：首登失败时人看到的是这条说明，不是文档。
assert.match(org, /需该账号有可用模型/, 'the model-grant precondition must be visible in the row');

// --- expanding a row loads that account's keys, not the whole key list -----------------------

// 展开/收起：类挂在摘要行与详情行两处，详情行默认隐藏由 CSS 门控（见文末的 CSS 断言）。
assert.match(org, /entry\.row\.classList\.toggle\('open', open\)/, 'the expander toggles the summary row');
assert.match(org, /entry\.detailRow\.classList\.toggle\('open', open\)/, 'and the detail row with it');
assert.match(org, /state\.open\.add\(entry\.account\.id\)/, 'the expansion state lives on the page, not in the DOM alone');
assert.match(org, /async function fillDetail\(entry\) \{/, 'the detail is filled into the row that is already there');
assert.match(org, /await api\.get\('\/keys', \{ account_id: account\.id, limit: 100 \}\)/,
  'the expanded row must fetch only this account\'s keys');
assert.match(org, /const isWorker = String\(key\.name \|\| ''\)\.startsWith\('dshgw-'\)/,
  'the worker key must be recognised, because it is not a key a person signs in with');
assert.match(org, /网关自动创建（worker 凭据，不参与选择）/, 'and it must say so in the row');

// --- the per-account operations are the ones M72 moved here ---------------------------------

for (const [label, pattern] of [
  ['新建 Key', /onclick: \(\) => addKey\(account, refresh\)/],
  ['启用/停用 DSH', /onclick: \(\) => toggleDSH\(account\)/],
  ['绑定/解绑飞书', /onclick: \(\) => account\.feishu && account\.feishu\.bound \? unbindFeishu\(account, refresh\) : bindFeishu\(account, refresh\)/],
  ['分配组织', /onclick: \(\) => assignOrgs\(account\)/],
]) {
  assert.match(org, pattern, 'the expanded row must offer ' + label);
}
assert.match(org, /api\.post\('\/accounts\/' \+ account\.id \+ '\/dsh', \{ enabled: false \}\)/,
  'disabling DSH from the row uses the same endpoint as the accounts page');
assert.match(org, /nodeIds: account\.org_node_ids \|\| \[\]/,
  'the picker opens on the account\'s current memberships');
assert.match(org, /onSubmit: \(ids\) => api\.patch\('\/accounts\/' \+ account\.id, \{ org_node_ids: ids \}\)/,
  'assigning organizations replaces the membership list (整表替换)');
assert.match(org, /createKeyForAccount/, 'creating a key goes through the shared, one-time-secret path');
assert.match(org, /editKey\(key, refresh\)/, 'a key row can be edited from the person row');
assert.match(org, /toggleKey\(key, refresh\)/, 'a key row can be suspended from the person row');
assert.match(org, /openFeishuPersonPicker\(\{ account \}\)/, 'binding opens the person picker');
assert.match(org, /unbindAccountFeishu\(account\)/, 'unbinding goes through the shared confirmation');

// --- read-only operators get no write actions ------------------------------------------------

const actionButtons = org.slice(org.indexOf("const actions = el('div', { class: 'toolbar org-person-actions' }"));
assert.match(actionButtons, /text: '新建 Key', disabled: readonly/);
assert.match(actionButtons, /text: account\.dsh_enabled \? '停用 DSH' : '启用 DSH', disabled: readonly/);
assert.match(actionButtons, /disabled: readonly,/, 'the Feishu and organization buttons are disabled too');

// --- filtering is a view, never the membership -----------------------------------------------

assert.match(org, /function matchesPerson\(account, search\)/,
  'the person filter must be its own function: it matches two fields');
assert.match(org, /box\.disabled = readonly \|\| \(isFiltering && !box\.checked\);[\s\S]*?过滤时不能再加入成员/,
  'a filtered list must not ADD members (it is not the whole membership), while un-ticking stays possible');
assert.match(org, /const filtering = search\.trim\(\) !== '';/,
  'the filtering state is derived once per paint');

// --- 行内「编辑」与工具条「新建成员」 -----------------------------------------------------------

assert.match(org, /onclick: \(\) => editPerson\(entry\.account\)/,
  'the person row must offer 编辑 (in front of 展开, so account fields are one click away)');
assert.match(org, /const updated = await editAccount\(account, \{ title: '编辑账户 ' \+ account\.name \}\)/,
  'editing goes through the shared account editor');
assert.match(org, /const created = await createAccount\(\{[\s\S]*?title: '新建成员 — ' \+ node\.name/,
  '新建成员 opens the same creator, titled for the node');
assert.match(org, /checked\.add\(created\.id\);/,
  'the new member joins the node in the same request, so it must be ticked locally too');
assert.doesNotMatch(org, /reloadAccount\(/,
  'the old per-account re-read is gone: rows are refreshed through the row they belong to');

// --- the unassigned accounts have a home -----------------------------------------------------

assert.match(org, /const unassigned = state\.accounts\.filter\(\(account\) => !\(account\.org_node_ids \|\| \[\]\)\.length\)/,
  'accounts in no node must be found from the account list (the only endpoint that can answer it)');
assert.match(org, /function unassignedRow\(unassigned\)/, 'and rendered as a row of their own');
assert.match(org, /未归属账户 ' \+ unassigned\.length \+ ' 个'/, 'the row states how many there are');
assert.match(org, /org-members-plain/, 'the unassigned list is read-only: there is no node to join');
// 未归属那份列表**不渲染**勾选列（personTable 的 pickable:false），而不是画出来再用 CSS 藏起来：
// 没有节点可写时，一个勾选框只会骗人。
assert.match(org, /personTable\(\{ pickable: false \}\)/,
  'the unassigned list must drop the membership column, not hide it');

// --- the page loads whole accounts, not name-only rows ---------------------------------------

assert.match(org, /state\.accounts = payload\.data \|\| \[\];/,
  'the person rows need the whole account (DSH state, Feishu identity, key counts)');

// --- the shared key module owns the one-time secret dialog -----------------------------------

assert.match(keyActions, /export async function createKeyForAccount\(account, \{ name = '' \} = \{\}\) \{/,
  'the shared creator takes the account, so no page rebuilds the flow');
assert.match(keyActions, /await api\.patch\('\/keys\/' \+ created\.id, \{[\s\S]*?record_input_mode/,
  'the recording switches are applied right after creation, in one place');
assert.match(keyActions, /明文只显示这一次/, 'the secret dialog must say the plaintext is shown once');
assert.match(picker, /api\.get\('\/org\/feishu\/directory'\)/,
  'the person picker reads the directory (names and current bindings in one call)');
assert.match(picker, /api\.put\('\/accounts\/' \+ account\.id \+ '\/feishu'/,
  'and writes the account-level binding route');
assert.doesNotMatch(picker, /keys\/' \+ .*feishu\/bind/, 'the retired key-level bind route must not be called');

// --- the new markup has styles (a row that renders unstyled is a broken row) ------------------

for (const rule of ['.org-member-table {', '.org-person.open td {', '.org-person-detail {',
  '.org-person-detail-body {', '.org-member-toggle {', '.org-person-key {', '.org-unassigned {']) {
  assert.ok(css.includes(rule), 'app.css must style ' + rule);
}

// 「收起」的门控：详情行默认 display:none，只有 .open 才成为一行。用户报障的根因就是这条规则
// 从来不存在——收起只是把 open 类摘掉，内容照旧可见。源码一侧把它钉死。
assert.match(css, /\.org-person-detail \{ display:none; \}/,
  'a detail row must be hidden until it is opened (this rule IS the fix for 「点击收起不会收起」)');
assert.match(css, /\.org-person-detail\.open \{ display:table-row; \}/,
  'and an open detail row must become a table row again');

console.log('org_person_test: 人员行/展开详情/逐账号操作/未归属账户/只读与过滤保护 全部通过');
