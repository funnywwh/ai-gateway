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

assert.match(org, /function personRow\(account, node, checked, repaint, filtering\) \{/,
  'the person row must be its own function: it carries three different concerns');
assert.match(org, /el\('span', \{ class: 'org-member-name', text: account\.name \}\)/,
  'the row keeps the account name in its own class (the layout rules key off it)');
for (const badge of ['dshBadge', 'feishuBadge', 'keyBadge']) {
  assert.match(org, new RegExp(badge + '\\(account\\)'), 'the row must show ' + badge);
}
// The three DSH states are the whole reason the badge exists: enabled, explicitly disabled by an
// administrator, and never enabled (which is what auto_enable turns into "usable at login").
assert.match(org, /account\.dsh_enabled\) return badge\('DSH 已启用'/, 'an enabled account says so');
assert.match(org, /account\.dsh_disabled_at\) return badge\('DSH 已停用（管理员）'/, 'an explicit disable is distinguishable');
assert.match(org, /account\.dsh_effective\) return badge\('DSH 登录即可用'/, 'the auto-enabled case is stated, not implied');

// --- expanding a row loads that account's keys, not the whole key list -----------------------

assert.match(org, /container\.classList\.toggle\('open'\)/, 'the expander toggles the container, not the page');
assert.match(org, /await api\.get\('\/keys', \{ account_id: account\.id, limit: 100 \}\)/,
  'the expanded row must fetch only this account\'s keys');
assert.match(org, /const isWorker = String\(key\.name \|\| ''\)\.startsWith\('dshgw-'\)/,
  'the worker key must be recognised, because it is not a key a person signs in with');
assert.match(org, /网关自动创建（worker 凭据，不参与选择）/, 'and it must say so in the row');

// --- the per-account operations are the ones M72 moved here ---------------------------------

for (const [label, pattern] of [
  ['新建 Key', /onclick: \(\) => addKey\(account, refreshDetail\)/],
  ['启用/停用 DSH', /onclick: \(\) => toggleDSH\(account, refreshDetail\)/],
  ['绑定/解绑飞书', /onclick: \(\) => account\.feishu && account\.feishu\.bound \? unbindFeishu\(account, refreshDetail\) : bindFeishu\(account, refreshDetail\)/],
  ['分配组织', /onclick: \(\) => assignOrgs\(account, refreshDetail\)/],
]) {
  assert.match(org, pattern, 'the expanded row must offer ' + label);
}
assert.match(org, /api\.post\('\/accounts\/' \+ account\.id \+ '\/dsh', \{ enabled: false \}\)/,
  'disabling DSH from the row uses the same endpoint as the accounts page');
assert.match(org, /api\.patch\('\/accounts\/' \+ account\.id, \{ org_node_ids: splitList\(values\.org_node_ids\)\.map\(Number\) \}\)/,
  'assigning organizations replaces the membership list');
assert.match(org, /createKeyForAccount/, 'creating a key goes through the shared, one-time-secret path');
assert.match(org, /editKey\(key, refreshDetail\)/, 'a key row can be edited from the person row');
assert.match(org, /toggleKey\(key, refreshDetail\)/, 'a key row can be suspended from the person row');
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
assert.match(org, /if \(filtering && !box\.checked\) \{[\s\S]*?box\.disabled = true;[\s\S]*?过滤时不能再加入成员/,
  'a filtered list must not ADD members (it is not the whole membership), while un-ticking stays possible');
assert.match(org, /const filtering = search\.trim\(\) !== '';/,
  'the filtering state is derived once per paint');

// --- the unassigned accounts have a home -----------------------------------------------------

assert.match(org, /const unassigned = state\.accounts\.filter\(\(account\) => !\(account\.org_node_ids \|\| \[\]\)\.length\)/,
  'accounts in no node must be found from the account list (the only endpoint that can answer it)');
assert.match(org, /function unassignedRow\(unassigned\)/, 'and rendered as a row of their own');
assert.match(org, /未归属账户 ' \+ unassigned\.length \+ ' 个'/, 'the row states how many there are');
assert.match(org, /org-members-plain/, 'the unassigned list is read-only: there is no node to join');

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

for (const rule of ['.org-person {', '.org-person-detail {', '.org-member-toggle {', '.org-person-key {', '.org-unassigned {']) {
  assert.ok(css.includes(rule), 'app.css must style ' + rule);
}

console.log('org_person_test: 人员行/展开详情/逐账号操作/未归属账户/只读与过滤保护 全部通过');
