import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, badge, confirmDialog } from '../ui.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const createModel = el('button', { class: 'btn btn-primary', text: '新建模型', disabled: readonly });
  const createRoute = el('button', { class: 'btn', text: '新建路由', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, createModel, createRoute);

  // 供应商名给路由行做兜底显示，同时是「新建路由」的下拉框数据源：选择器要一次拿全，
  // 显式请求上限 1000（配置类列表的服务端上限）；两张分页表格各自带自己的 limit/offset。
  const providersPromise = api.get('/providers', { limit: 1000 });

  // 模型与路由是两张互相独立的表，各自持有自己的窗口；route_count 直接由服务端返回，
  // 不再在客户端按当前页的路由重新推导（那只会数到本页的路由）。
  const modelView = pagedTable({
    columns: [
      { key: 'public_name', label: '对外模型名', render: (row) => el('code', { text: row.public_name }) },
      { key: 'display_name', label: '显示名' },
      { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
      { key: 'route_count', label: '路由数' },
      { key: 'aliases', label: '别名', render: (row) => (row.aliases || []).join(', ') || '—' },
      { key: 'sale_pricing', label: '对客定价', render: (row) => row.sale_pricing ? el('code', { text: JSON.stringify(row.sale_pricing) }) : el('span', { class: 'muted', text: '未设置' }) },
    ],
    rowActions: (row) => readonly ? [] : [el('button', { class: 'btn', text: '编辑', onclick: () => editModel(row, () => modelView.refresh()) })],
    load: ({ limit, offset }) => api.get('/models', { limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('模型', modelView.node, [el('span', { class: 'muted', text: '模型只启停不删除，避免历史账单失去模型名' })]));

  const routeView = pagedTable({
    columns: [
      { key: 'model', label: '模型' },
      { key: 'provider', label: '供应商' },
      { key: 'upstream_model', label: '上游模型名', render: (row) => el('code', { text: row.upstream_model }) },
      { key: 'priority', label: '优先级' },
      { key: 'weight', label: '权重' },
      { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
      { key: 'cooldown_until', label: '冷却至' },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '调整', onclick: () => editRoute(row, () => routeView.refresh()) }),
      el('button', { class: 'btn btn-danger', text: '删除', onclick: () => removeRoute(row, () => routeView.refresh()) }),
    ],
    load: async ({ limit, offset }) => {
      const [payload, providersPayload] = await Promise.all([api.get('/routes', { limit, offset }), providersPromise]);
      const providerName = new Map(((providersPayload.data) || []).map((p) => [p.id, p.name]));
      return { ...payload, data: (payload.data || []).map((route) => ({ ...route, provider: route.provider || providerName.get(route.provider_id) })) };
    },
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('路由（同一模型可挂多个供应商做负载均衡）', routeView.node));

  refresh.addEventListener('click', () => { modelView.refresh(); routeView.refresh(); });
  createModel.addEventListener('click', async () => {
    const result = await modal({
      title: '新建模型',
      fields: [
        { name: 'public_name', label: '对外模型名', required: true },
        { name: 'display_name', label: '显示名' },
        { name: 'aliases', label: '别名（JSON 数组）', type: 'textarea', json: true, value: '[]' },
      ],
      onSubmit: async (values) => { await api.post('/models', values); toast('已创建', 'ok'); await modelView.refresh(); return true; },
    });
    return result;
  });
  createRoute.addEventListener('click', async () => {
    // 模型/供应商下拉框都要一次拿全，所以显式请求上限 1000。
    const [modelsPayload, providersPayload] = await Promise.all([api.get('/models', { limit: 1000 }), providersPromise]);
    const models = modelsPayload.data || [];
    const providers = providersPayload.data || [];
    if (!models.length || !providers.length) { toast('需要至少一个模型和一个供应商', 'error'); return; }
    await modal({
      title: '新建路由',
      fields: [
        { name: 'model_id', label: '模型', type: 'select', options: models.map((m) => ({ value: m.id, label: m.public_name })) },
        { name: 'provider_id', label: '供应商', type: 'select', options: providers.map((p) => ({ value: p.id, label: p.name })) },
        { name: 'upstream_model', label: '上游模型名', required: true },
        { name: 'priority', label: '优先级（同层内均衡）', type: 'number', value: 100 },
        { name: 'weight', label: '权重', type: 'number', value: 100 },
      ],
      onSubmit: async (values) => {
        await api.post('/routes', { ...values, model_id: Number(values.model_id), provider_id: Number(values.provider_id) });
        toast('已创建', 'ok');
        await routeView.refresh();
        return true;
      },
    });
  });
  await Promise.all([modelView.refresh(), routeView.refresh()]);
}

async function editModel(row, reload) {
  const result = await modal({
    title: '编辑模型 ' + row.public_name,
    fields: [
      { name: 'display_name', label: '显示名', value: row.display_name },
      { name: 'enabled', label: '启用', type: 'checkbox', value: row.enabled },
      { name: 'aliases', label: '别名（JSON 数组）', type: 'textarea', json: true, value: row.aliases || [] },
      { name: 'sale_pricing', label: '对客定价（JSON，M11a 计价引擎使用）', type: 'textarea', json: true, value: row.sale_pricing || '' },
    ],
    onSubmit: async (values) => { await api.patch('/models/' + encodeURIComponent(row.public_name), values); toast('已保存', 'ok'); await reload(); return true; },
  });
  return result;
}

async function editRoute(row, reload) {
  await modal({
    title: '调整路由 ' + row.model + ' → ' + row.provider,
    fields: [
      { name: 'upstream_model', label: '上游模型名', required: true, value: row.upstream_model },
      { name: 'priority', label: '优先级', type: 'number', value: row.priority },
      { name: 'weight', label: '权重', type: 'number', value: row.weight },
      { name: 'enabled', label: '启用', type: 'checkbox', value: row.enabled },
      { name: 'reset_cooldown', label: '清除冷却', type: 'checkbox' },
    ],
    onSubmit: async (values) => { await api.patch('/routes/' + row.id, values); toast('已保存', 'ok'); await reload(); return true; },
  });
}

async function removeRoute(row, reload) {
  const ok = await confirmDialog('删除路由', '确认删除 ' + row.model + ' → ' + row.provider + ' 吗？');
  if (!ok) return;
  await api.del('/routes/' + row.id);
  toast('已删除', 'ok');
  await reload();
}