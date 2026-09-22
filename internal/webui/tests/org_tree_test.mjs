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
// 账号的创建/编辑（含所属组织的勾选树字段）在本次改动里抽成了共用模块：账户页与组织页都用它。
const accountActions = await readFile(new URL('../static/js/pages/account_actions.js', import.meta.url), 'utf8');
const orgAssign = await readFile(new URL('../static/js/pages/org_assign.js', import.meta.url), 'utf8');
const app = await readFile(new URL('../static/js/app.js', import.meta.url), 'utf8');
const router = await readFile(new URL('../static/js/router.js', import.meta.url), 'utf8');
const css = await readFile(new URL('../static/app.css', import.meta.url), 'utf8');

// --- the control is reusable and knows nothing about the organization structure ------------

assert.match(treeSrc, /export function tree\(\{/, 'tree.js must export the tree() factory');
assert.match(treeSrc, /^import \{ el, clear \} from '\.\/ui\.js';$/m,
  'the tree may only depend on the generic DOM helpers, so any page can reuse it');
assert.doesNotMatch(treeSrc, /from '\.\/api\.js'/,
  'the tree must not fetch: it receives nodes and reports interactions through callbacks');
// The intent is "no organization-specific code or data reaches the control". It used to be
// spelled /org/i over the whole file, which broke the moment a comment mentioned the word —
// a comment cannot couple anything. What actually couples it is importing the module, naming
// its endpoints, or reaching the API, so those are what is refused here.
assert.doesNotMatch(treeSrc, /org\.js|org\/nodes|org_node/, 'the tree must not reference the organization module or its endpoints');
assert.doesNotMatch(treeSrc, /\borg[A-Z]/, 'the tree must not carry organization-shaped identifiers');
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

// --- 组织树只渲染在工作区，不再挂到左侧栏 ---------------------------------------------------
//
// 这里断言的是**负向事实**，所以两侧都要钉住：页面不得有侧边栏实例，shell 也不得再提供插槽。
// 只查页面会把"插槽留着但没人用"当成通过，而那段代码没有任何使用者，正是要清掉的东西。
assert.doesNotMatch(org, /mode: 'sidebar'/, 'the org page must not mount a second tree in the sidebar');
// Structural, not prose: the page explains in a comment why the sidebar tree was removed, and a
// comment cannot mount anything. What would couple it to the shell is using the slot or its
// element, so that is what is refused.
assert.doesNotMatch(org, /sidebarSlot|sidebar-slot|querySelector\('\.sidebar|sidebar\.append\(/,
  'the org page must not touch the sidebar slot or element');
assert.doesNotMatch(app, /sidebar-slot/, 'the shell slot has no consumer left: it must be gone, not merely unused');
assert.doesNotMatch(css, /\.sidebar-slot/, 'and so must its CSS');
// 树控件本身仍然支持两种放置方式（那是最初的需求），由 `tree` harness 视图守：那里把它挂进
// 一个 .sidebar 形状的容器。控件里必须留着这个能力，页面不再用它而已。
assert.match(treeSrc, /mode = 'workspace'/, 'the control still defaults to the workspace placement');
assert.match(treeSrc, /const compact = mode === 'sidebar'/, 'and still supports the compact sidebar placement');

// --- the org page mounts one tree and keeps it in step with the detail panel ---------------

assert.match(org, /mode: 'workspace'/, 'the org page must mount a full tree');
assert.match(org, /mainTree\.setSelected\(id\)/, 'selecting a node must keep the tree highlight in step');
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

// --- 过滤支持拼音与英文；成员过滤器不滚动；选中的成员置顶 --------------------------------

// 拼音能力是**注入**的：控件自己不依赖拼音表（那是 Go 测试 TestConsoleFiltersUseThePinyinMatcher
// 也钉着的一条），页面通过 matcher 传进来；成员过滤直接用 matchesQuery。
assert.match(org, /import \{ matchesQuery \} from '\.\.\/pinyin\.js'/, 'the org page must use the pinyin matcher');
assert.match(org, /matcher: matchesQuery/, 'the node tree filter must match pinyin too');
// M72：过滤同时匹配账号名与飞书姓名（组织页的人员行就是账号行）。
assert.match(org, /matchesPerson\(account, search\)/, 'the member filter must go through the person matcher');
assert.match(org, /matchesQuery\(account\.name, search\) \|\| \(feishu \? matchesQuery\(feishu, search\) : false\)/,
  'the person filter must match the account name and the Feishu name');
assert.match(org, /type: 'search', placeholder: '按账号名或飞书姓名过滤（支持拼音/, 'the member filter must say that pinyin works');
assert.match(treeSrc, /matcher,/, 'the control must accept a matcher callback');
assert.doesNotMatch(treeSrc, /import[^;]*pinyin/, 'the control must not depend on the pinyin table itself');

// 过滤器必须在滚动区之外：`memberToolbar`（含搜索框）与 `list`（滚动区）是两个兄弟节点。
assert.match(org, /const memberToolbar = el\('div', \{ class: 'org-member-toolbar' \}, \[searchBox, selectedCount\]\)/,
  'the filter row must be its own element so it can stay put while the list scrolls');
assert.match(org, /const memberPanel = el\('div', \{ class: 'org-member-panel' \}, \[memberToolbar, list\]\)/,
  'the panel must hold the fixed toolbar and the scrolling list side by side');
// padding 归零是表格化的前提：表头（thead th）是 sticky 到滚动区顶部的，中间留一条内边距
// 就会漏出行内容从缝里钻过去。
assert.match(css, /\.org-members \{ max-height:280px; overflow:auto; padding:0; \}/,
  'only the list scrolls, and it must start flush so a sticky table header has nothing to leak through');
assert.doesNotMatch(css, /\.org-members \{[^}]*border:1px/,
  'the scrolling element must not be the bordered panel that also holds the filter');

// 选中置顶：排序必须在每次重绘时按"已勾选"分组，且勾选后立即重绘。
assert.match(org, /Number\(checked\.has\(right\.id\)\) - Number\(checked\.has\(left\.id\)\)/,
  'checked members must sort before unchecked ones');
// 勾选在人员行里，勾完立刻重绘，所以刚勾的账号会立刻置顶。
assert.match(org, /if \(entry\.box\.checked\) checked\.add\(account\.id\); else checked\.delete\(account\.id\);/,
  'the checkbox writes into the checked set');
assert.match(org, /onToggle: \(\) => paint\(\)/,
  'ticking a member must repaint, so it lands at the top immediately');
// 重绘复用行对象：展开中的行不会被一次勾选/过滤重绘扔掉，也不会因此重新拉一次 Key 列表。
assert.match(org, /const entries = new Map\(\);/,
  'rows must be reused across repaints, or an expanded row would collapse on every tick');
assert.match(org, /state\.open\.has\(account\.id\)/,
  'the expansion state must survive a rebuild of the table');

// --- 树的箭头必须是画出来的，不能是文字字形 -----------------------------------------------
//
// 现场反馈："父节点的左边有一个方块"——方块就是缺字形（tofu）。树控件原来用 ▸/▾
// （U+25B8/U+25BE）这对冷门几何字符，缺它的字体栈上就是个空方框。本仓库已有先例：
// 弹框关闭按钮的 ✕ 因同样原因改成了内联 SVG（commit 93c8b78）。
//
// 判据是"剥掉注释后的代码里不能再出现这些字形"：注释里提到它们是**解释**（必须留着，
// 否则下一个人会把它们改回来），而代码里出现就是 bug 回来了。
const treeCodeOnly = treeSrc
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/\/\/[^\n]*/g, '');
for (const glyph of ['▸', '▾', '·']) {
  assert.ok(!treeCodeOnly.includes(glyph),
    'tree.js must not put the ' + glyph + ' glyph in the DOM: it renders as an empty box where the font lacks it; draw it as SVG instead');
}
assert.match(treeSrc, /function arrowIcon\(/, 'the expand/collapse arrow must be drawn (arrowIcon)');
assert.match(treeSrc, /function leafIcon\(/, 'a leaf needs a drawn marker too, not a text dot');
assert.match(treeSrc, /createElementNS\('http:\/\/www\.w3\.org\/2000\/svg'/, 'icons are inline SVG, like the dialog close button');
assert.match(treeSrc, /\[kids \? arrowIcon\(expanded\) : leafIcon\(\)\]/, 'the toggle is built from the drawn icons');

// --- 勾选框不能被全局 `input { width:100% }` 撑满 ------------------------------------------
//
// 这条 bug 的形态是布局：文字一字不差，只有量几何才看得出来，所以浏览器走查里量了
// getBoundingClientRect（见 scripts/ui-harness/org.page.html）。这里再从源码一侧钉住根因：
// 成员行与工具栏筛选行都必须显式给出勾选框尺寸，且不能用块级的 `.field` 承载行内勾选框。
assert.match(css, /\.org-member input\[type=checkbox\] \{ flex:0 0 auto; width:16px/,
  'the member checkbox needs an explicit size: the global input{width:100%} rule stretches it');
assert.match(css, /\.org-member-name \{ display:block; min-width:0; overflow-wrap:anywhere; \}/,
  'a long account name must wrap instead of pushing the id out of its column');
assert.match(css, /\.filter-check input\[type=checkbox\] \{ flex:0 0 auto; width:16px/,
  'the toolbar filter checkbox needs the same explicit size');
assert.doesNotMatch(accounts, /class: 'field inline'/,
  "the toolbar filter must not use the block-level .field layout, which puts the checkbox and its text on separate lines");
assert.match(org, /class: 'org-member-name'/, 'the member name carries its own class so the layout rule is unambiguous');
assert.match(org, /class: 'org-member-table'/, 'the person list is a table, so its columns can be aligned');
assert.match(css, /\.org-member-table thead th \{ position:sticky; top:0;/,
  'the column labels must stay put while the list scrolls: a header that scrolls away is no header');

// --- the accounts page shows and filters by organization ----------------------------------

assert.match(accounts, /key: 'org_nodes', label: '所属组织'/, 'accounts must show the organization column');
assert.match(accounts, /api\.get\('\/org\/nodes', \{ limit: 1000 \}\)/, 'the filter options come from the org endpoint');
assert.match(accounts, /query\.org_node_id = orgFilter\.value/, 'the selected node must reach the query');
assert.match(accounts, /include_descendants/, 'the filter must expose the descendants switch');
// The parameters are spread from orgQuery(), which stays empty until a node is chosen, so an
// unfiltered list sends the request it always did. Pinning the helper's name keeps the intent
// (the filter reaches the list) without pinning the shape of the object literal.
assert.match(accounts, /api\.get\('\/accounts', \{ limit, offset, \.\.\.orgQuery\(\) \}\)/,
  'the account list must send the organization filter');
assert.match(accounts, /function orgQuery\(\)/, 'the filter parameters must come from one place');
// 手填「组织节点 id（逗号分隔）」已经换成勾选树：账号可同时属于多个节点这件事，靠人记 id 是记不住的。
assert.match(accountActions, /name: 'org_node_ids'/, 'the account editor must still carry the field');
assert.doesNotMatch(accounts, /组织节点 id（逗号分隔）/, 'the raw node-id input must be gone');
assert.match(accountActions, /openOrgPicker\(\{ title: title \|\| '分配组织', nodeIds: state\.refs\.map\(\(ref\) => ref\.id\) \}\)/,
  'the field opens the shared checkbox tree');
assert.match(accountActions, /org_node_ids: values\.org_node_ids \|\| \[\]/,
  'the editor must send the replacement membership list (always an array: empty means "no organization")');
assert.match(accountActions, /import \{ createAccount, editAccount \} from|export async function createAccount/,
  'the shared module owns the account forms');
// 组织页与账户页共用同一份实现：两处入口的字段与落库路径只写一次。
assert.match(org, /import \{ createAccount, editAccount \} from '\.\/account_actions\.js'/,
  'the org page must use the shared account forms');
assert.match(org, /import \{ openOrgPicker \} from '\.\/org_assign\.js'/,
  'the org page must use the shared organization picker');
assert.match(org, /orgRefs: \[\{ id: node\.id, name: node\.name, path: node\.path \|\| node\.name \}\]/,
  'a member created from a node is pre-assigned to that node');
assert.match(org, /onSubmit: \(ids\) => api\.patch\('\/accounts\/' \+ account\.id, \{ org_node_ids: ids \}\)/,
  'assigning from the person row replaces the membership list through the picker');
assert.match(orgAssign, /await onSubmit\(ids\)/,
  'the picker lets the caller own the write (a form defers it to its own 保存)');
assert.match(orgAssign, /api\.get\('\/org\/nodes', \{ limit: 1000 \}\)/,
  'the picker reads the node list itself, so it cannot show a stale tree');

// --- routing ------------------------------------------------------------------------------

assert.match(router, /path: '\/org', title: '组织架构', module: '\.\/pages\/org\.js'/,
  'the org page needs a route');

// --- 子节点必须缩进：每层 22px ------------------------------------------------------------
//
// 反馈现场是上线后的控制台，原话：「组织架构树控件子节点要缩进」。缩进本来就有——tree.js 的
// `INDENT` 是 14px，行的左内边距按 `8 + depth * INDENT` 铺开；问题是 14px 在 13.5px 字号下
// 差不多只有一个汉字宽，层级读不出来，看起来就像没缩进。所以改法是把这**一个常量**提到 22px，
// 而不是新加一条缩进路径。
//
// 这里钉两件事。第一是缩进的来源：必须是那一个具名常量，行内样式保持"基准 + 深度 × 缩进"的
// 形状——多出一条缩进路径（CSS 再补一份 padding-left，或按模式各算一套）会让几何不可推。
// 第二是**走查期望值与它同步**：`scripts/ui-harness/*.page.html` 里写死的期望值在"没有能跑的
// 浏览器"的地方（本工作区就是这样：/usr/bin/firefox 是 snap 空壳，跑起来只有 "no report"）
// 没人会执行到，改常量时最容易漏掉，而漏掉的表现是走查报红、看起来像刚改坏了。
// 走查侧真正量的东西是几何，见 tree.page.html 的 `workspaceIndentShiftsLabels` /
// `sidebarIndentShiftsLabels` 与 org.page.html 的 `indentShiftsLabels`。
const INDENT_WANT = 22;
const indentDecl = treeSrc.match(/const INDENT = (\d+);/);
assert.ok(indentDecl, '每层缩进必须由一个具名常量声明');
assert.equal(Number(indentDecl[1]), INDENT_WANT,
  '每层缩进必须是 ' + INDENT_WANT + 'px：14px 在一行 13.5px 字号下读不出层级（现场反馈「子节点要缩进」）');
assert.match(treeSrc, /padding-left:' \+ \(8 \+ depth \* INDENT\)/,
  '行的左内边距必须是"基准 + 深度 × 缩进"，缩进不能另开一条路径（CSS 再补一份会让几何不可推）');
for (const harnessPage of ['tree.page.html', 'org.page.html']) {
  const harnessSrc = await readFile(new URL('../../../scripts/ui-harness/' + harnessPage, import.meta.url), 'utf8');
  assert.match(harnessSrc, new RegExp('=== ' + INDENT_WANT + '\\b'),
    harnessPage + ' 的走查期望值必须与 tree.js 的 INDENT 相等，否则改常量时走查会静默失守');
}

console.log('Organization structure UI checks passed.');
