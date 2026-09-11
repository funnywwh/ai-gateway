import { api } from '../api.js';
import { el, card, pagedTable, toast, formatTime, jsonBlock } from '../ui.js';

export async function render({ page, actions }) {
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh);

  const view = pagedTable({
    columns: [
      { key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
      { key: 'actor', label: '操作者' },
      { key: 'action', label: '动作' },
      { key: 'target_type', label: '对象类型' },
      { key: 'target_id', label: '对象 ID', render: (row) => el('code', { text: row.target_id || '—' }) },
      { key: 'result', label: '结果', render: (row) => row.result === 'ok' ? el('span', { class: 'badge ok', text: 'ok' }) : el('span', { class: 'badge danger', text: row.result || '—' }) },
      { key: 'changes', label: '变更', render: (row) => jsonBlock(row.changes) },
    ],
    load: ({ limit, offset }) => api.get('/audit-logs', { limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('审计日志', view.node, [
    el('span', { class: 'muted', text: '凭据与令牌只记录布尔与字段名，永不记录明文' })]));

  refresh.addEventListener('click', () => view.refresh());
  await view.refresh();
}