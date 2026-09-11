import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, badge, confirmDialog, formatTime } from '../ui.js';

const EVENTS = ['*', 'response.completed', 'response.failed', 'provider.cooled_down', 'apikey.created', 'backup.completed'];

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '新建 Hook', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);

  const view = pagedTable({
    columns: [
      { key: 'name', label: '名称' },
      { key: 'type', label: '类型', render: (row) => badge(row.type) },
      { key: 'url', label: '目标', render: (row) => el('code', { text: row.url }) },
      { key: 'events', label: '事件', render: (row) => (row.events || []).join(', ') || '全部' },
      { key: 'include_content', label: '附带内容', render: (row) => row.include_content ? badge('是', 'warn') : badge('否') },
      { key: 'sample_rate', label: '采样率' },
      { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
      { key: 'created_at', label: '创建时间', render: (row) => formatTime(row.created_at) },
    ],
    rowActions: (row) => readonly ? [] : [el('button', { class: 'btn btn-danger', text: '删除', onclick: () => remove(row, () => view.refresh()) })],
    load: ({ limit, offset }) => api.get('/hooks', { limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('Hooks（事件投递）', view.node, [
    el('span', { class: 'muted', text: 'webhook 使用 HMAC-SHA256 签名；附带内容会把请求/响应正文发出去，请谨慎开启' })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', async () => {
    await modal({
      title: '新建 Hook', wide: true,
      fields: [
        { name: 'name', label: '名称', required: true },
        { name: 'type', label: '类型', type: 'select', options: ['webhook', 'jsonl'] },
        { name: 'url', label: '目标地址或文件路径', required: true, hint: 'webhook 必须是 https（或在配置里放开 hooks.allow_insecure）' },
        { name: 'secret', label: '签名密钥', hint: '接收方用它在 X-AIGW-Signature 上验签' },
        { name: 'events', label: '订阅事件（JSON 数组，[] 表示全部）', type: 'textarea', json: true, value: JSON.stringify(['response.completed']) },
        { name: 'include_content', label: '附带请求/响应内容', type: 'checkbox' },
        { name: 'sample_rate', label: '采样率 (0,1]', type: 'number', value: 1 },
      ],
      onSubmit: async (values) => {
        await api.post('/hooks', { ...values, sample_rate: Number(values.sample_rate), include_content: !!values.include_content });
        toast('已创建', 'ok');
        await view.refresh();
        return true;
      },
    });
  });
  await view.refresh();
}

async function remove(row, reload) {
  const ok = await confirmDialog('删除 Hook', '确认删除 ' + row.name + ' 吗？之后的事件不会再投递到它。');
  if (!ok) return;
  await api.del('/hooks/' + row.id);
  toast('已删除', 'ok');
  await reload();
}

export { EVENTS };