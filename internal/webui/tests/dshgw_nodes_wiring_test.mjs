// Static wiring checks for the 「DSH 节点」 page (M77). These are text assertions on purpose: the
// behaviour of the page is pinned by dshgw_nodes_test.mjs (a real module run) and its rendering by
// scripts/ui-harness (views `nodes` / `nodes-readonly`), so what is left here is the *plumbing* —
// the route exists, the accounts page shows the placement, and the harness page is registered.
//
// Each of these has already been wrong once in this repository's history in some other page:
// a page nobody can reach (no route), a column whose data never arrives (no join), and a harness
// page nobody runs (not in the view list) all look finished in a diff.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const read = (relative) => readFile(new URL(relative, import.meta.url), 'utf8');

const router = await read('../static/js/router.js');
assert.match(router, /path: '\/dsh-nodes', title: 'DSH 节点', module: '\.\/pages\/dshgw_nodes\.js', group: '运维'/,
  'the node page must be reachable from the sidebar (运维 group)');
assert.ok(router.includes("'/dsh-nodes'"), 'and its path must be the one the docs and the harness use');

const page = await read('../static/js/pages/dshgw_nodes.js');
// 清单默认不探测：这是"页面刷新不该变成机群负载"的实现点。
assert.match(page, /api\.get\('\/dshgw\/nodes', probe \? \{ probe: 'true' \} : undefined\)/,
  'the list call must only ask for a probe when one was requested');
assert.match(page, /\/dshgw\/nodes\/' \+ encodeURIComponent\(name\) \+ '\/deploy'/,
  'the deploy must go through the node-scoped endpoint');
assert.match(page, /accept_host_key: fingerprint/,
  'the fingerprint confirmation must be sent back as accept_host_key');
assert.match(page, /purge=true&confirm=/, 'a purge must carry the confirmation the API requires');
assert.match(page, /不会搬运数据/, 'the migration dialog must say that it does not move data');
assert.match(page, /setTimeout\(poll, 1500\)/, 'the deploy drawer polls while the job runs');
assert.ok(page.includes('clearTimeout(timer)'), 'and stops polling when it is closed');

const accounts = await read('../static/js/pages/accounts.js');
assert.ok(accounts.includes("label: '节点'"), 'the accounts page must show the placement column');
assert.match(accounts, /export function dshNodeCell/, 'and render it from the row\'s node');
assert.match(accounts, /api\.get\('\/dshgw\/nodes'\)/, 'the enable dialog must read the node list');
assert.match(accounts, /values\.node && values\.node !== 'local' \? \{ node: values\.node \}/,
  'and only send a node when one was really chosen');

const harness = await read('../../../scripts/ui-harness/run.sh');
assert.ok(harness.includes('nodes nodes-readonly'), 'the harness view list must include the node page');
assert.ok(harness.includes('nodes|nodes-readonly) echo "dshgw-nodes.html"'),
  'and it must map those views to the harness page');

const harnessPage = await read('../../../scripts/ui-harness/dshgw_nodes.page.html');
assert.ok(harnessPage.includes("import { render as renderNodes } from '/js/pages/dshgw_nodes.js';"),
  'the harness page must render the real page module');

console.log('dshgw_nodes_wiring_test.mjs: all checks passed');
