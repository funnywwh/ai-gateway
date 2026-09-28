// Hash router. Pages are ES modules loaded on demand, so adding a screen means
// adding a file and a table entry here - the shell never changes.

export const routes = [
  { path: '/', title: '概览', module: './pages/dashboard.js', group: '总览' },
  { path: '/chat', title: '智能问答', module: './pages/chat.js', group: '总览' },
  { path: '/skills', title: '技能库', module: './pages/skills.js', group: '总览' },
  { path: '/keys', title: 'API Keys', module: './pages/keys.js', group: '访问控制' },
  { path: '/accounts', title: '账户', module: './pages/accounts.js', group: '访问控制' },
  { path: '/org', title: '组织架构', module: './pages/org.js', group: '访问控制' },
  { path: '/companies', title: '公司', module: './pages/companies.js', group: '访问控制' },
  { path: '/tags', title: '标签', module: './pages/tags.js', group: '访问控制' },
  { path: '/mcp-tokens', title: 'MCP 令牌', module: './pages/mcp.js', group: '访问控制' },
  { path: '/redemption-codes', title: '兑换码', module: './pages/codes.js', group: '访问控制' },
  { path: '/admins', title: '管理员', module: './pages/admins.js', group: '访问控制' },
  { path: '/providers', title: '模型供应商', module: './pages/providers.js', group: '路由配置' },
  { path: '/models', title: '模型与路由', module: './pages/models.js', group: '路由配置' },
  { path: '/mappings', title: '模型映射', module: './pages/mappings.js', group: '路由配置' },
  { path: '/pricing', title: '定价', module: './pages/pricing.js', group: '计费' },
  { path: '/billing', title: '账本与充值', module: './pages/billing.js', group: '计费' },
  { path: '/invoices', title: '账单', module: './pages/billing.js', group: '计费' },
  { path: '/reconciliation', title: '对账', module: './pages/billing.js', group: '计费' },
  { path: '/requests', title: '请求日志', module: './pages/requests.js', group: '可观测' },
  { path: '/hooks', title: 'Hooks', module: './pages/hooks.js', group: '可观测' },
  { path: '/audit', title: '审计日志', module: './pages/audit.js', group: '可观测' },
  { path: '/dsh-nodes', title: 'DSH 节点', module: './pages/dshgw_nodes.js', group: '运维' },
  { path: '/backups', title: '备份', module: './pages/backups.js', group: '运维' },
  { path: '/settings', title: '设置', module: './pages/settings.js', group: '运维' },
];

const cache = new Map();

// currentRoute resolves the hash. The path may carry a query string ("#/chat?session=…"),
// which pages use for deep links into a specific conversation; the path alone decides which
// route matches, so adding a query never changes which page is shown.
export function currentRoute() {
  const hash = window.location.hash.replace(/^#/, '') || '/';
  const [path, search] = splitHash(hash);
  const route = routes.find((entry) => entry.path === path) || routes[0];
  return Object.assign({}, route, { params: new URLSearchParams(search || '') });
}

function splitHash(hash) {
  const index = hash.indexOf('?');
  if (index < 0) return [hash, ''];
  return [hash.slice(0, index), hash.slice(index + 1)];
}

export function startRouter(onNavigate) {
  const handle = () => onNavigate(currentRoute());
  window.addEventListener('hashchange', handle);
  handle();
}

export async function loadPage(route) {
  if (!cache.has(route.module)) cache.set(route.module, import(route.module));
  return cache.get(route.module);
}

export function navigate(path) { window.location.hash = '#' + path; }

// Groups are foldable, and what is folded is remembered per browser: the menu is 24 links in a
// 100vh scrolling column, so the groups an operator is not using cost him scroll distance on
// every visit. The key follows the display currency's (`aigw.display_currency`, money.js): a view
// preference lives in this browser, and the server is never told about it.
const FOLD_KEY = 'aigw.nav_folded';
let folded = null;

// foldedGroups is the set of folded group names, read from localStorage once and kept in a
// module-level Set afterwards. It cannot live in the DOM: renderNav rebuilds the whole nav on
// every navigation, exactly when the state has to survive. Any failure — a private-mode
// localStorage, a hand-edited value, storage disabled by policy — degrades to "nothing folded",
// never to a console that will not draw its own menu.
function foldedGroups() {
  if (folded) return folded;
  folded = new Set();
  try {
    const raw = window.localStorage.getItem(FOLD_KEY);
    const list = raw ? JSON.parse(raw) : [];
    if (Array.isArray(list)) list.forEach((name) => { if (typeof name === 'string') folded.add(name); });
  } catch (err) { /* unreadable storage: this page keeps its own state in memory */ }
  return folded;
}

function persistFolded(set) {
  try { window.localStorage.setItem(FOLD_KEY, JSON.stringify([...set])); } catch (err) {
    // Private mode: folding still works for this page, it just will not survive a reload.
  }
}

// groupHeader builds the clickable group title. It is a <button> (reachable by Tab, toggled by
// Enter/Space natively) carrying aria-expanded and aria-controls, so a folded group is
// distinguishable from one that happens to have no items.
//
// The arrow is drawn by app.css (a border triangle) rather than typed: ▸/▾ (U+25B8/U+25BE) render
// as an empty box on a font stack that lacks them — the accident that once put "方块" in front of
// every parent row in the tree (arrowIcon in tree.js) and reduced the dialog's ✕ to nothing
// (closeIcon in ui.js). An icon the console depends on is never a glyph.
function groupHeader(name, open, index) {
  const head = el2('button', {
    class: 'group',
    type: 'button',
    'aria-expanded': open ? 'true' : 'false',
    'aria-controls': 'nav-group-' + index,
  });
  head.append(el2('span', { text: name }), el2('span', { class: 'nav-chevron', 'aria-hidden': 'true' }));
  return head;
}

// toggleGroup flips one group in place, updating the nodes it was handed instead of asking
// renderNav to rebuild the nav: the button keeps focus and the menu keeps its scroll position.
function toggleGroup(head, box, group) {
  const open = head.getAttribute('aria-expanded') !== 'true';
  head.setAttribute('aria-expanded', open ? 'true' : 'false');
  box.hidden = !open;
  const set = foldedGroups();
  if (open) set.delete(group); else set.add(group);
  persistFolded(set);
}

// appendGroup adds one group's header plus its (possibly folded) item container, wiring the
// toggle, and returns the container the caller appends that group's links to.
//
// Everything the click handler needs is this call's own parameter or constant. That is the point:
// a binding shared by every iteration of the render loop would leave each group's toggle folding
// and remembering the LAST group — a bug no source reading notices, because the handler looks
// right next to the node it was built from.
function appendGroup(container, name, open, index) {
  const head = groupHeader(name, open, index);
  // The links go inside their own container rather than straight into <nav>: `hidden` is what
  // folds them, and the UA rule `[hidden]{display:none}` loses to the author's
  // `.sidebar nav a{display:block}` — the attribute would sit on the element while every link
  // stayed on screen. A wrapper no author rule gives a display to cannot be overridden that way,
  // and app.css states the rule again explicitly.
  const items = el2('div', { class: 'nav-group', id: 'nav-group-' + index });
  items.hidden = !open;
  head.addEventListener('click', () => toggleGroup(head, items, name));
  container.append(head, items);
  return items;
}

export function renderNav(container) {
  container.replaceChildren();
  const active = currentRoute().path;
  const activeGroup = (routes.find((route) => route.path === active) || routes[0]).group;
  const foldedAway = foldedGroups();
  let group = null;
  let box = null;
  let index = -1;
  for (const route of routes) {
    if (route.group !== group) {
      group = route.group;
      index += 1;
      // The group the operator is standing in is never drawn folded: a deep link, a reload or a
      // page's own navigate() button must not hide the current location from the menu. This is
      // also why rendering never writes the fold back — only toggleGroup does.
      box = appendGroup(container, group, group === activeGroup || !foldedAway.has(group), index);
    }
    box.append(el2('a', {
      href: '#' + route.path,
      class: route.path === active ? 'active' : '',
      text: route.title + (route.milestone ? ' · ' + route.milestone : ''),
    }));
  }
}

function el2(tag, attrs) {
  const node = document.createElement(tag);
  Object.entries(attrs).forEach(([key, value]) => {
    if (key === 'text') node.textContent = value; else node.setAttribute(key, value);
  });
  return node;
}