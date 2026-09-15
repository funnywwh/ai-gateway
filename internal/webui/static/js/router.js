// Hash router. Pages are ES modules loaded on demand, so adding a screen means
// adding a file and a table entry here - the shell never changes.

export const routes = [
  { path: '/', title: '概览', module: './pages/dashboard.js', group: '总览' },
  { path: '/chat', title: '智能问答', module: './pages/chat.js', group: '总览' },
  { path: '/skills', title: '技能库', module: './pages/skills.js', group: '总览' },
  { path: '/keys', title: 'API Keys', module: './pages/keys.js', group: '访问控制' },
  { path: '/accounts', title: '账户', module: './pages/accounts.js', group: '访问控制' },
  { path: '/org', title: '组织架构', module: './pages/org.js', group: '访问控制' },
  { path: '/tags', title: '标签', module: './pages/tags.js', group: '访问控制' },
  { path: '/mcp-tokens', title: 'MCP 令牌', module: './pages/mcp.js', group: '访问控制' },
  { path: '/redemption-codes', title: '兑换码', module: './pages/codes.js', group: '访问控制' },
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

export function renderNav(container) {
  container.replaceChildren();
  let group = null;
  const active = currentRoute().path;
  for (const route of routes) {
    if (route.group !== group) {
      group = route.group;
      container.append(el2('div', { class: 'group', text: group }));
    }
    container.append(el2('a', {
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