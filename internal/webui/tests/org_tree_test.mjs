// Static regression checks for the organization structure UI: the reusable tree control, the
// sidebar slot it is mounted into, the org page's write path, and the account page's org column
// and filter.
//
// These are text assertions rather than behaviour tests because this environment has no node
// and no JS test runner (see docs/TODO.md); the behaviour itself is exercised in a real browser
// by scripts/ui-harness (views `tree` and `org`). What this file protects is the wiring: a
// control that stops being reusable, a page that stops calling the endpoint it must call, or a
// read-only operator who gets a write button.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const treeSrc = await readFile(new URL('../static/js/tree.js', import.meta.url), 'utf8');
const org = await readFile(new URL('../static/js/pages/org.js', import.meta.url), 'utf8');
const accounts = await readFile(new URL('../static/js/pages/accounts.js', import.meta.url), 'utf8');
const app = await readFile(new URL('../static/js/app.js', import.meta.url), 'utf8');
const router = await readFile(new URL('../static/js/router.js', import.meta.url), 'utf8');
const css = await readFile(new URL('../static/app.css', import.meta.url), 'utf8');

// --- the control is reusable and knows nothing about the organization structure ------------

assert.match(treeSrc, /export function tree\(\{/, 'tree.js must export the tree() factory');
assert.match(treeSrc, /^import \{ el, clear \} from '\.\/ui\.js';$/m,
  'the tree may only depend on the generic DOM helpers, so any page can reuse it');
assert.doesNotMatch(treeSrc, /from '\.\/api\.js'/,
  'the tree must not fetch: it receives nodes and reports interactions through callbacks');
assert.doesNotMatch(treeSrc, /org/i,
  'the tree must not know about the organization structure at all');
for (const option of ['nodes', 'selectedId', 'mode', 'expandDepth', 'collapsible', 'filter',
  'renderLabel', 'renderMeta', 'actions', 'onSelect', 'onToggle', 'onAction']) {
  assert.match(treeSrc, new RegExp('\\b' + option + '\\b'), 'the tree must accept the ' + option + ' option');
}
// Both placements are one control: the mode decides density, not behaviour.
assert.match(treeSrc, /mode = 'workspace'/, 'workspace must be the default placement');
assert.match(treeSrc, /tree-' \+ \(compact \? 'sidebar' : 'workspace'\)/,
  'the two placements must differ only by a mode class');
assert.match(treeSrc, /const compact = mode === 'sidebar'/, 'sidebar mode is the compact placement');
// Accessibility and keyboard support are part of the contract, not an extra.
assert.match(treeSrc, /role: 'tree'/, 'the container must be an ARIA tree');
assert.match(treeSrc, /role: 'treeitem'/, 'rows must be ARIA treeitems');
assert.match(treeSrc, /'aria-expanded'/, 'a parent row must report its expansion state');
assert.match(treeSrc, /'aria-level'/, 'a row must report its level');
assert.match(treeSrc, /tabindex: String\(String\(focusId\) === String\(id\) \? 0 : -1\)/,
  'the tree must use a roving tabindex (exactly one focusable row)');
for (const key of ['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight', 'Enter', 'Home', 'End']) {
  assert.match(treeSrc, new RegExp("'" + key + "'"), 'keyboard navigation must handle ' + key);
}
// One delegated listener on the root instead of one per row.
assert.match(treeSrc, /root\.addEventListener\('click'/, 'clicks must be delegated from the root');
assert.doesNotMatch(treeSrc, /row\.addEventListener\('click'/,
  'per-row listeners would cost one closure per row');

// --- the shell offers a sidebar slot to every page ----------------------------------------

assert.match(app, /const sidebar = el\('div', \{ class: 'sidebar-slot' \}\)/,
  'the shell must provide a sidebar slot');
assert.match(app, /renderBrand\(version\), nav, sidebar,/, 'the slot must sit between the nav and the footer');
assert.match(app, /clear\(sidebar\)/, 'the slot must be emptied on every route change');
assert.match(app, /render\(\{ page, actions, session, route, navigate, sidebar \}\)/,
  'the slot must reach the page through the render context');
assert.match(css, /\.sidebar-slot:empty \{ display:none; \}/,
  'an unused slot must not change the layout of pages that do not use it');

// --- the org page mounts the same control twice and keeps them in step --------------------

assert.match(org, /mode: 'sidebar'/, 'the org page must mount a compact tree');
assert.match(org, /mode: 'workspace'/, 'the org page must mount a full tree');
assert.match(org, /sidebar\.append\(/, 'the compact tree goes into the sidebar slot');
assert.match(org, /sidebarTree\.setSelected\(id\)/, 'selecting in the page must update the sidebar tree');
assert.match(org, /mainTree\.setSelected\(id\)/, 'selecting in the sidebar must update the page tree');
assert.match(org, /api\.put\('\/org\/nodes\/' \+ node\.id \+ '\/accounts', \{ account_ids: \[\.\.\.checked\] \}\)/,
  'saving members must replace the node\'s member list');
assert.match(org, /api\.get\('\/org\/nodes'/, 'the tree data comes from the org endpoint');
assert.match(org, /api\.del\('\/org\/nodes\/' \+ node\.id \+ '\?cascade=true'\)/,
  'deleting a node from the page confirms the subtree cascade');
assert.match(org, /confirmDialog\('删除组织节点'/, 'a delete must be confirmed first');
// A read-only operator gets no write buttons at all.
assert.match(org, /actions: \(node\) => readonly \? \[\] : \[/, 'row actions must be hidden for read-only roles');
assert.match(org, /const saveMembers = el\('button', \{\s*class: 'btn btn-primary', text: '保存成员',\s*disabled: readonly/,
  'the member save button must be disabled for read-only roles');
// The save path must not run before the current members are known, or it would clear the node.
assert.match(org, /state\.membersLoaded = false;/, 'the save button must be re-armed only after the members load');

// --- the accounts page shows and filters by organization ----------------------------------

assert.match(accounts, /key: 'org_nodes', label: '所属组织'/, 'accounts must show the organization column');
assert.match(accounts, /api\.get\('\/org\/nodes', \{ limit: 1000 \}\)/, 'the filter options come from the org endpoint');
assert.match(accounts, /query\.org_node_id = orgFilter\.value/, 'the selected node must reach the query');
assert.match(accounts, /include_descendants/, 'the filter must expose the descendants switch');
assert.match(accounts, /api\.get\('\/accounts', \{ limit, offset, \.\.\.query \}\)/,
  'the account list must send the organization filter');
assert.match(accounts, /name: 'org_node_ids'/, 'the account editor must offer the organization nodes');
assert.match(accounts, /org_node_ids: splitIDs\(values\.org_node_ids\)/,
  'the editor must send the replacement membership list');

// --- routing ------------------------------------------------------------------------------

assert.match(router, /path: '\/org', title: '组织架构', module: '\.\/pages\/org\.js'/,
  'the org page needs a route');

console.log('Organization structure UI checks passed.');
