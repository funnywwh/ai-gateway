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

// Groups are foldable and start folded: the menu is 24 links in a 100vh scrolling column, and an
// operator reads it to go somewhere, not to read all of it. What is drawn open by default is the
// group the current page lives in (see renderNav) — so the menu opens as "where I am", not as a
// wall of links. Which groups the operator opened himself is remembered per browser; the key
// follows the display currency's (`aigw.display_currency`, money.js): a view preference lives in
// this browser, and the server is never told about it.
//
// The stored set is the OPENED groups, not the folded ones (it was `aigw.nav_folded` until the
// default flipped): with a folded default, "absent from the set" already means folded, and only
// the opened ones need remembering. An old `aigw.nav_folded` value is simply not read any more —
// every group it named is folded by default now anyway, and one is not worth a migration.
const OPEN_KEY = 'aigw.nav_open';
let opened = null;

// openedGroups is the set of group names the operator opened, read from localStorage once and kept
// in a module-level Set afterwards. It cannot live in the DOM: renderNav rebuilds the whole nav on
// every navigation, exactly when the state has to survive. Any failure — a private-mode
// localStorage, a hand-edited value, storage disabled by policy — degrades to "nothing opened",
// never to a console that will not draw its own menu.
function openedGroups() {
  if (opened) return opened;
  opened = new Set();
  try {
    const raw = window.localStorage.getItem(OPEN_KEY);
    const list = raw ? JSON.parse(raw) : [];
    if (Array.isArray(list)) list.forEach((name) => { if (typeof name === 'string') opened.add(name); });
  } catch (err) { /* unreadable storage: this page keeps its own state in memory */ }
  return opened;
}

function persistOpened(set) {
  try { window.localStorage.setItem(OPEN_KEY, JSON.stringify([...set])); } catch (err) {
    // Private mode: opening a group still works for this page, it just will not survive a reload.
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
// The set it records holds the OPENED groups, so opening adds and closing removes — the other way
// round would persist exactly the opposite of what the operator did.
function toggleGroup(head, box, group) {
  const open = head.getAttribute('aria-expanded') !== 'true';
  head.setAttribute('aria-expanded', open ? 'true' : 'false');
  box.hidden = !open;
  const set = openedGroups();
  if (open) set.add(group); else set.delete(group);
  persistOpened(set);
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
  const openedUp = openedGroups();
  let group = null;
  let box = null;
  let index = -1;
  for (const route of routes) {
    if (route.group !== group) {
      group = route.group;
      index += 1;
      // A group is drawn folded unless the operator opened it. The one exception is the group the
      // current page lives in: a deep link, a reload or a page's own navigate() button must leave
      // the operator's location visible in the menu. That is also why rendering never writes the
      // set back — only toggleGroup does.
      box = appendGroup(container, group, group === activeGroup || openedUp.has(group), index);
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