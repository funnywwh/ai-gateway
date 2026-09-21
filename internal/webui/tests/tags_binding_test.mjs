// Static regression checks for account/API Key tag binding and editing.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const accounts = await readFile(new URL('../static/js/pages/accounts.js', import.meta.url), 'utf8');
const keys = await readFile(new URL('../static/js/pages/keys.js', import.meta.url), 'utf8');
// Key 的编辑表单在 M72 抽到了共用模块（组织页也要用），标签写入路径跟着它一起搬到那里。
const keyActions = await readFile(new URL('../static/js/pages/key_actions.js', import.meta.url), 'utf8');
const tags = await readFile(new URL('../static/js/pages/tags.js', import.meta.url), 'utf8');

assert.match(accounts, /key: 'tags', label: '标签'/);
// The account editor gained a second derived field (org membership, M49), so matching the
// whole object literal would only pin the exact spelling of unrelated code. What this test
// is here for is that tag editing goes through splitTags on a PATCH by id.
assert.match(accounts, /api\.patch\('\/accounts\/' \+ row\.id, \{[\s\S]*?\.\.\.values[\s\S]*?tags: splitTags\(values\.tags\)/);
assert.match(accounts, /所有 API Key 自动继承/);
assert.match(keys, /key: 'effective_tags', label: '生效标签'/);
assert.match(keyActions, /name: 'tags', label: 'Key 标签（逗号分隔）'/);
// 编辑走 id（PATCH），标签值经 splitTags 规整：两处都钉住，避免有人把共用模块的内联版本复制回页面。
assert.match(keyActions, /api\.patch\('\/keys\/' \+ row\.id, \{[\s\S]*tags: \(values\.tags \|\| ''\)\.split\(','\)/);
assert.match(keys, /import \{ createKeyForAccount, editKey, toggleKey, showSecret \} from '\.\/key_actions\.js'/,
  'the keys page must use the shared key actions instead of its own copy');
assert.match(tags, /标签（账号与 API Key）/);
// Editing goes by id (PATCH), never by name: the upsert is keyed by name, so an edit used
// to fork a second tag — and it refused any name the writer did not accept, which left the
// rows the importer created (蓝精灵1/2/3) permanently uneditable.
assert.match(tags, /if \(row\) await api\.patch\('\/tags\/' \+ row\.id, values\)/);
assert.match(tags, /else await api\.post\('\/tags', values\)/);
// The name field is the identity while editing, so it is shown read-only.
assert.match(tags, /readonly: !!row/);

console.log('Account/API Key tag binding UI checks passed.');
