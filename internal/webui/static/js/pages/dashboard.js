import { api } from '../api.js';
import { el, card, stat, toast } from '../ui.js';

export async function render({ page }) {
  const stats = await api.get('/stats');
  const registry = stats.registry || {};
  page.append(card('注册表', el('div', { class: 'grid' }, [
    stat('模型', registry.models ?? 0),
    stat('供应商', registry.providers ?? 0),
    stat('供应商模型', registry.provider_models ?? 0),
    stat('路由', registry.routes ?? 0),
    stat('映射规则', registry.mappings ?? 0),
    stat('标签', registry.tags ?? 0),
    stat('账户', registry.accounts ?? 0),
    stat('Key 缓存条目', stats.key_cache_entries ?? '—'),
  ]), [registry.ready ? el('span', { class: 'badge ok', text: 'ready' }) : el('span', { class: 'badge danger', text: 'not ready' })]));

  const metrics = Object.entries(stats.balancer || {});
  const metricRows = metrics.map(([key, value]) => ({
    target: key, inflight: value.inflight, total: value.total, failures: value.failures,
    latency_ms: Math.round(value.latency_ewma_ms || 0), samples: value.samples,
  }));
  page.append(card('供应商运行时（在途 / 成功率）', renderTable(['target', 'inflight', 'total', 'failures', 'latency_ms', 'samples'], metricRows, '还没有流量')));

  const cooldowns = Object.entries(stats.cooldowns || {}).map(([key, until]) => ({
    target: key, until: new Date(until).toLocaleString(),
  }));
  page.append(card('冷却中', renderTable(['target', 'until'], cooldowns, '当前没有被冷却的目标')));
}

function renderTable(columns, rows, empty) {
  if (!rows.length) return el('div', { class: 'empty', text: empty });
  return el('table', {}, [
    el('thead', {}, [el('tr', {}, columns.map((c) => el('th', { text: c })))]),
    el('tbody', {}, rows.map((row) => el('tr', {}, columns.map((c) => el('td', { text: format(row[c]) }))))),
  ]);
}

function format(value) {
  if (value === null || value === undefined || value === '') return '—';
  return String(value);
}