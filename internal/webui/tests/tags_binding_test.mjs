// Static regression checks for account/API Key tag binding and editing.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const accounts = await readFile(new URL('../static/js/pages/accounts.js', import.meta.url), 'utf8');
const keys = await readFile(new URL('../static/js/pages/keys.js', import.meta.url), 'utf8');
const tags = await readFile(new URL('../static/js/pages/tags.js', import.meta.url), 'utf8');

assert.match(accounts, /key: 'tags', label: '标签'/);
assert.match(accounts, /api\.patch\('\/accounts\/' \+ row\.id, \{ \.\.\.values, tags: splitTags\(values\.tags\) \}\)/);
assert.match(accounts, /所有 API Key 自动继承/);
assert.match(keys, /key: 'effective_tags', label: '生效标签'/);
assert.match(keys, /name: 'tags', label: 'Key 标签（逗号分隔）'/);
assert.match(keys, /api\.patch\('\/keys\/' \+ row\.id, \{[\s\S]*tags: splitTags\(values\.tags\)/);
assert.match(tags, /标签（账号与 API Key）/);

console.log('Account/API Key tag binding UI checks passed.');
