// 控制台主菜单的分组折叠（M95）的**源码不变式**。
//
// 真实几何与交互由 scripts/ui-harness 的 `sidebar` 视图在浏览器里量（本环境没有可用的 firefox，
// 见 scripts/ui-harness/README.md），这里钉住的是**没有浏览器也能判定**的那几件事：状态存在哪、
// DOM 契约长什么样、折叠为什么必须走 `hidden` 而不是行内样式、以及壳层与走查的接线没断。
//
// 每一条都对应一个"看起来完成、其实不工作"的形状：
//   * 状态放进 DOM —— renderNav 每次导航都重建整块导航，折叠会在第一次点链接时丢掉；
//   * 把 `hidden` 打在 <a> 上 —— `.sidebar nav a{display:block}` 会盖掉 UA 的 `[hidden]`，
//     DOM 里属性在、屏幕上链接照样在（与 2026-09-23 组织树丢缩进同一类事故）；
//   * 存储裸读裸写 —— 隐私模式下 localStorage 抛错，整个控制台白屏；
//   * 走查页与 run.sh 没接线 —— 断言写好了但没人执行（本仓库重复踩过的坑）。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const read = (relative) => readFile(new URL(relative, import.meta.url), 'utf8');

const router = await read('../static/js/router.js');
const css = await read('../static/app.css');
const app = await read('../static/js/app.js');

// --- ① 打开过的分组：浏览器本地偏好，读写都不许抛出去 ---------------------------------------

assert.match(router, /const OPEN_KEY = 'aigw\.nav_open';/,
  '记住的是**被打开过的**分组（aigw.nav_open）：默认折叠之后，"不在集合里"已经等于折叠，只有打开过的需要记');
assert.match(router, /try \{[^}]*localStorage\.getItem\(OPEN_KEY\)/,
  '读存储必须包 try：隐私模式/被策略禁用时 localStorage 会抛错，菜单不能因此画不出来');
assert.match(router, /try \{[^}]*localStorage\.setItem\(OPEN_KEY/,
  '写存储同样必须包 try');
assert.match(router, /JSON\.parse\(raw\)/, '存储内容按 JSON 解析');
assert.match(router, /Array\.isArray\(list\)/,
  '值被手工改成别的形状时降级成"什么都没打开"，而不是抛错或把字符串当分组名');
assert.match(router, /JSON\.stringify\(\[\.\.\.set\]\)/,
  '写回去的是分组名的 JSON 数组（跨版本可读，分组改名时旧记录自然失效）');
assert.match(router, /if \(open\) set\.add\(group\); else set\.delete\(group\);/,
  '集合的语义是"打开过"：点开就加入、收起就移除 —— 写反了就是把操作者做的事持久化成反的');
assert.doesNotMatch(router, /localStorage\.[A-Za-z]+\([^)]*nav_folded/,
  '旧的 aigw.nav_folded 不再读也不再写：默认折叠之后，它记过的那些组本来就都折着，不值得为它写一段迁移'
  + '（注释里可以提它的来历，但不能再拿它当存储用）');

// --- ② 默认折叠 + 当前分组恒展开 ------------------------------------------------------------

assert.match(router, /const activeGroup = \(routes\.find\(\(route\) => route\.path === active\) \|\| routes\[0\]\)\.group;/,
  '要先算出当前路由所在的组');
assert.match(router, /group === activeGroup \|\| openedUp\.has\(group\)/,
  '渲染规则：默认折叠，只有"当前路由所在的分组"与"操作者打开过的分组"是展开的');

// --- ③ 组标题是按钮，且带完整 ARIA 契约 ---------------------------------------------------

assert.doesNotMatch(router, /el2\('div', \{ class: 'group'/,
  '组标题不能再是纯文本 div（那正是"不可折叠"的形状）');
assert.match(router, /el2\('button', \{/, '组标题是原生 button：Tab 可达、Enter/Space 原生切换');
assert.match(router, /class: 'group',\n\s*type: 'button',/, 'button 要显式 type=button（不参与任何表单语义）');
assert.match(router, /'aria-expanded': open \? 'true' : 'false'/, '收起/展开必须反映在 aria-expanded 上');
assert.match(router, /'aria-controls': 'nav-group-' \+ index/, 'aria-controls 指向该组的容器');
assert.equal((router.match(/'nav-group-' \+ index/g) || []).length, 2,
  'aria-controls 与容器 id 必须由同一个索引生成（写死字符串会让两者悄悄对不上）');
assert.match(router, /class: 'nav-chevron', 'aria-hidden': 'true'/,
  '箭头是装饰：aria-hidden，状态由 aria-expanded 表达');

// --- ④ 折叠走 hidden（有包装容器），不走行内样式 -------------------------------------------

assert.match(router, /const items = el2\('div', \{ class: 'nav-group', id: 'nav-group-' \+ index \}\);/,
  '条目要包在 .nav-group 容器里：hidden 直接打在 <a> 上会被 `.sidebar nav a{display:block}` 盖掉');
assert.match(router, /head\.addEventListener\('click', \(\) => toggleGroup\(head, items, name\)\);/,
  'click 处理器要用**这一次调用自己的**绑定：闭包一个循环里复用的变量，会让每一组都去折叠并记住最后一组');
assert.match(router, /function appendGroup\(container, name, open, index\)/,
  '每一组的标题/容器/处理器在一个独立作用域里建：这是上面那条不变式的落地方式');
assert.match(router, /items\.hidden = !open;/, '渲染时用容器上的 hidden 落地折叠状态');
assert.match(router, /box\.hidden = !open;/, '点击时就地翻转同一个属性（不整块重渲染，焦点与滚动位置都留着）');
assert.doesNotMatch(router, /setAttribute\(\s*['"]style['"]|\.style\./,
  '折叠不许走行内样式：控制台 CSP 是 style-src \'self\'，style 属性会被整条丢弃（见 style_csp_test.mjs）');

// --- ⑤ app.css：按钮复位、CSS 画的箭头、以及显式写出的 [hidden] 规则 ------------------------

assert.match(css, /\.sidebar nav \.group \{[^}]*display:flex;/,
  '组标题是 flex 行：左边文字、右边箭头');
assert.match(css, /\.sidebar nav \.group \{[^}]*width:100%;/, '按钮不能缩成内容宽度（点不到整行）');
assert.match(css, /\.sidebar nav \.group \{[^}]*border:0;/, '去掉浏览器给按钮的默认边框');
assert.match(css, /\.sidebar nav \.group \{[^}]*background:none;/, '去掉浏览器给按钮的默认底色');
assert.match(css, /\.sidebar nav \.nav-chevron \{[^}]*border-left:4px solid currentColor;/,
  '箭头用 CSS 画的三角，不打字形（▸/▾ 在缺字体的机器上是空框，见 tree.js/ui.js 两处前车之鉴）');
assert.match(css, /\.sidebar nav \.group\[aria-expanded=true\] \.nav-chevron \{ transform: rotate\(90deg\); \}/,
  '箭头方向由 aria-expanded 驱动（状态只有一个真源）；不带引号是刻意的：esbuild 会去掉标识符值的引号，而 minify 的选择器守卫要求源码与镜像逐字一致');
assert.match(css, /\.sidebar nav \.nav-group\[hidden\] \{ display:none; \}/,
  "显式写出 [hidden] 规则：UA 的 `[hidden]{display:none}` 会被作者样式盖掉，而 `.sidebar nav a{display:block}` 正是其中之一");

// 新规则必须全部限定在 `.sidebar nav` 下：别处也有叫 group 的东西（例如 billing.js 的表格列）。
const selectors = [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{/g)]
  .map((match) => match[1].trim())
  .filter((block) => !block.startsWith('@'))
  .flatMap((block) => block.split(',').map((part) => part.trim()));
const navSelectors = selectors.filter((sel) => /\.group\b|\.nav-group\b|\.nav-chevron\b/.test(sel));
assert.ok(navSelectors.length >= 6, `折叠相关的选择器应当在 app.css 里成组出现，只找到 ${navSelectors.length} 条`);
for (const sel of navSelectors) {
  assert.ok(sel.startsWith('.sidebar nav '),
    `折叠相关的选择器必须限定在 .sidebar nav 之下，否则会波及别处的 .group：${sel}`);
}

// --- ⑥ 壳层接线：导航容器仍是 app.js 里那个 nav ---------------------------------------------

assert.match(app, /renderNav\(nav\);/,
  '壳层必须继续把导航容器交给 renderNav（折叠完全发生在它内部，壳层不需要知道）');
assert.match(app, /el\('aside', \{ class: 'sidebar' \}/,
  '导航仍在 .sidebar 里 —— 新样式全部以它为作用域，换容器就等于换了一套样式');

// --- ⑦ 走查接线：视图注册与页面 import ------------------------------------------------------

const runSh = await read('../../../scripts/ui-harness/run.sh');
assert.match(runSh, /^VIEWS="[^"]*\bsidebar\b[^"]*"$/m,
  'sidebar 视图必须挂进 run.sh 的 VIEWS 清单，否则断言永远不会被执行');
assert.match(runSh, /sidebar\) echo "sidebar\.html"/, '视图必须映射到 sidebar.html');
assert.match(runSh, /cp "\$ROOT\/scripts\/ui-harness\/sidebar\.page\.html"/,
  'sidebar 页没有接口要 stub，照 csp 页的做法直接复制进工作目录');

const harnessPage = await read('../../../scripts/ui-harness/sidebar.page.html');
assert.match(harnessPage, /import \{ renderNav \} from '\/js\/router\.js';/,
  '走查页必须渲染真实的 renderNav，而不是自己写一份导航');
assert.match(harnessPage, /import\('\/js\/router\.js\?reload=/,
  '冷启动必须换一个模块实例：同一个实例的模块级缓存会让"刷新后仍收起"验成假绿');
assert.match(harnessPage, /strictChecks: true/,
  '这一页的 checks 全是判定项，不能让非布尔值混过去');
assert.match(harnessPage, /getAttribute\('aria-controls'\)/,
  '走查页要照实现的 DOM 契约取容器（aria-controls → 它的目标），而不是写死 nav-group-N 这种下标 id');

// --- ⑧ 把真模块跑一遍：折叠/展开/记忆/当前分组恒展开/存储坏掉的降级 --------------------------
//
// 上面 ①–⑦ 是"代码里有没有这些东西"，这一段是"它到底会不会那样跑"。真机几何（offsetHeight、
// CSS 画的箭头、[hidden] 有没有被盖掉）只有浏览器能证明，由 scripts/ui-harness 的 `sidebar`
// 视图负责；但折叠的**逻辑**——谁被收起、刷新后还记不记得、导航进被收起的那组会怎样、存储坏掉
// 时会不会白屏——在这里就能用真模块跑出来，而本环境恰恰没有可用的浏览器。
//
// 模块用 vm.SourceTextModule 加载，DOM 用一个最小的替身（router.js 只用到 createElement /
// setAttribute / append / replaceChildren / hidden / addEventListener）。替身里 `hidden` 是属性、
// 不反射成 attribute：属性那点差异由浏览器视图去量（它读 offsetHeight），这里量的是逻辑。

function memoryStorage(initial) {
  const map = new Map(Object.entries(initial || {}));
  return {
    getItem: (key) => (map.has(key) ? map.get(key) : null),
    setItem: (key, value) => { map.set(key, String(value)); },
    dump: () => Object.fromEntries(map),
  };
}

// brokenStorage 是"隐私模式/被策略禁用"的形状：连读都抛。
function brokenStorage() {
  return {
    getItem() { throw new Error('storage is not available'); },
    setItem() { throw new Error('storage is not available'); },
    dump: () => ({}),
  };
}

function fakeDom(storage) {
  function makeNode(tag) {
    const el = {
      tag, children: [], attributes: {}, listeners: {},
      className: '', textContent: '', hidden: false,
      append(...items) { for (const item of items.flat()) el.children.push(item); },
      replaceChildren(...items) { el.children = items.flat(); },
      setAttribute(name, value) {
        el.attributes[name] = String(value);
        if (name === 'class') el.className = String(value);
      },
      getAttribute(name) { return name in el.attributes ? el.attributes[name] : null; },
      addEventListener(name, listener) { el.listeners[name] = listener; },
      click() { if (el.listeners.click) el.listeners.click(); },
    };
    return el;
  }
  const win = {
    location: { hash: '#/' },
    localStorage: storage,
    addEventListener() { /* 这一页不跑 startRouter，导航靠改 hash 后自己重渲染 */ },
  };
  const context = vm.createContext({
    console, Error, Promise, URLSearchParams,
    document: { createElement: (tag) => makeNode(tag) },
    window: win,
  });
  return { context, win, makeNode };
}

let moduleSeq = 0;
async function loadRouter(context) {
  // 同一个 context 里新建的模块实例 = 一次新的页面加载：模块级缓存是空的，状态只能来自存储。
  const module = new vm.SourceTextModule(router, { identifier: 'router.js#' + (moduleSeq += 1), context });
  await module.link(async (specifier) => {
    throw new Error('router.js 现在没有 import；新增依赖时在这个 linker 里补一个 SyntheticModule：' + specifier);
  });
  await module.evaluate();
  return module.namespace;
}

const groupHeads = (nav) => nav.children.filter((child) => child.tag === 'button');
const groupNames = (nav) => groupHeads(nav).map((head) => head.children[0].textContent);
const headOf = (nav, name) => groupHeads(nav).find((head) => head.children[0].textContent === name);
const boxOf = (nav, name) => {
  const head = headOf(nav, name);
  return head ? nav.children[nav.children.indexOf(head) + 1] : null;
};
const linksOf = (nav, name) => (boxOf(nav, name) ? boxOf(nav, name).children : []);
const isOpen = (nav, name) => !!headOf(nav, name) && headOf(nav, name).getAttribute('aria-expanded') === 'true'
  && boxOf(nav, name).hidden === false;

// —— 默认（无存储）：只展开当前页面所在的分组（hash '#/' → 总览），24 条链接仍在 DOM 里 ——
const first = fakeDom(memoryStorage());
const routerMod = await loadRouter(first.context);
let nav = first.makeNode('nav');
routerMod.renderNav(nav);
assert.deepEqual(groupNames(nav), ['总览', '访问控制', '路由配置', '计费', '可观测', '运维'],
  '六组必须按路由表的顺序渲染，名字就是存储里用的键');
assert.equal(nav.children.filter((child) => child.tag === 'a').length, 0,
  '条目必须全部落在分组容器里：直接挂在 nav 上的链接不会跟着分组一起收起');
assert.equal(groupNames(nav).reduce((sum, name) => sum + linksOf(nav, name).length, 0), 24,
  '24 条路由一条不少，且都在各自的分组容器里（默认折叠只是收着，不是不画）');
assert.deepEqual(groupNames(nav).filter((name) => isOpen(nav, name)), ['总览'],
  '默认（无存储）只展开当前页面所在的分组，其余 5 组折叠');
assert.ok(groupHeads(nav).every((head) => head.tag === 'button'
  && head.attributes['aria-controls'] === boxOf(nav, head.children[0].textContent).attributes.id),
  'aria-controls 必须指向本组的容器 id');
assert.ok(nav.children.every((child, index) => (child.tag === 'button') === (index % 2 === 0)),
  'DOM 顺序是 标题-容器-标题-容器：容器紧跟在它的标题后面');

// —— 点开一个折叠的分组：只动它自己，并且写进存储 ——
headOf(nav, '访问控制').click();
assert.equal(headOf(nav, '访问控制').getAttribute('aria-expanded'), 'true', '点击折叠的分组必须展开');
assert.equal(boxOf(nav, '访问控制').hidden, false, '点击后容器必须取掉 hidden');
assert.deepEqual(groupNames(nav).filter((name) => isOpen(nav, name)), ['总览', '访问控制'],
  '只展开被点的那一组，其余不受影响');
assert.deepEqual(JSON.parse(first.win.localStorage.getItem('aigw.nav_open')), ['访问控制'],
  '打开过的分组必须写进 aigw.nav_open（分组名的 JSON 数组）');

// —— 再点一次：收起来，并从存储里删掉 ——
headOf(nav, '访问控制').click();
assert.equal(isOpen(nav, '访问控制'), false, '再点一次收起');
assert.deepEqual(JSON.parse(first.win.localStorage.getItem('aigw.nav_open')), [],
  '收起必须从存储里移除，否则下次打开还会自己展开');
headOf(nav, '访问控制').click();

// —— 同一次页面会话里重渲染（每次导航都会发生）：打开状态不能丢 ——
nav = first.makeNode('nav');
routerMod.renderNav(nav);
assert.equal(isOpen(nav, '访问控制'), true, 'renderNav 每次导航都重建导航，打开状态必须活过重建');
assert.equal(isOpen(nav, '计费'), false, '没打开过的分组照旧折叠');

// —— 冷启动（刷新）：换一个模块实例，状态只能来自存储 ——
const coldMod = await loadRouter(first.context);
nav = first.makeNode('nav');
coldMod.renderNav(nav);
assert.equal(isOpen(nav, '访问控制'), true, '刷新后仍然记得打开过它（从 localStorage 读回来）');
assert.equal(isOpen(nav, '总览'), true, '当前分组（总览）无论如何都展开');
assert.deepEqual(groupNames(nav).filter((name) => isOpen(nav, name)), ['总览', '访问控制'],
  '其余 4 组保持折叠');

// —— 导航进一个折叠的分组：当前分组恒展开，当前项高亮，存储不被改写 ——
first.win.location.hash = '#/accounts';
nav = first.makeNode('nav');
coldMod.renderNav(nav);
assert.equal(isOpen(nav, '访问控制'), true, '当前页面所在的分组必须展开，否则"我在哪"从菜单里消失了');
assert.deepEqual(linksOf(nav, '访问控制').filter((link) => link.className.includes('active'))
  .map((link) => link.attributes.href), ['#/accounts'], '当前项仍然是高亮的那个');
assert.deepEqual(JSON.parse(first.win.localStorage.getItem('aigw.nav_open')), ['访问控制'],
  '渲染只读存储：自动展开不该写盘');

// —— 导航进一个从没打开过的分组：它"当前"，所以照样展开（这条正是默认折叠最危险的角落）——
first.win.location.hash = '#/pricing';
nav = first.makeNode('nav');
coldMod.renderNav(nav);
assert.equal(isOpen(nav, '计费'), true, '当前分组即使从没被打开过也要展开');
assert.deepEqual(JSON.parse(first.win.localStorage.getItem('aigw.nav_open')), ['访问控制'],
  '这次自动展开同样不写盘');

// —— 在活动分组里显式点击：就地收起；下一次渲染它按"当前分组恒展开"回来 ——
headOf(nav, '计费').click();
assert.equal(isOpen(nav, '计费'), false, '显式点击当前分组也照做（点击是操作者的意思）');
nav = first.makeNode('nav');
coldMod.renderNav(nav);
assert.equal(isOpen(nav, '计费'), true, '但渲染规则压过它：下一次渲染当前分组仍然展开');

// —— 存储坏掉/内容不对：降级成"只有当前分组展开"，绝不抛错 ——
for (const [label, storage] of [
  ['localStorage 抛错（隐私模式/被策略禁用）', brokenStorage()],
  ['值不是合法 JSON', memoryStorage({ 'aigw.nav_open': '{' })],
  ['值不是数组', memoryStorage({ 'aigw.nav_open': '"访问控制"' })],
  ['数组里混了非字符串', memoryStorage({ 'aigw.nav_open': '[1,null,"访问控制"]' })],
]) {
  const dom = fakeDom(storage);
  const mod = await loadRouter(dom.context);
  const bad = dom.makeNode('nav');
  mod.renderNav(bad);
  assert.equal(bad.children.filter((child) => child.tag === 'button').length, 6,
    `存储出问题时导航仍然要完整画出来：${label}`);
  const open = groupNames(bad).filter((name) => isOpen(bad, name));
  const want = label.startsWith('数组里混') ? ['总览', '访问控制'] : ['总览'];
  assert.deepEqual(open, want,
    `降级成"只有当前分组展开"（坏值里的非字符串不算数），而不是白屏或半截菜单：${label}`);
}

console.log('nav_fold_test.mjs: all checks passed');
