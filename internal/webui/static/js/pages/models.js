import { api } from '../api.js';
import { el, card, table, modal, toast, badge, confirmDialog } from '../ui.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const createModel = el('button', { class: 'btn btn-primary', text: '新建模型', disabled: readonly });
  const createRoute = el('button', { class: 'btn', text: '新建路由', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, createModel, createRoute);
  let modelView;
  let routeView;

  async function load() {
    const [modelsPayload, routesPayload, providersPayload] = await Promise.all([
      api.get('/models'), api.get('/routes'), api.get('/providers'),
    ]);
    const models = modelsPayload.data || [];
    const providers = providersPayload.data || [];
    const routes = routesPayload.data || [];
    const routeCount = new Map();
    routes.forEach((route) => routeCount.set(route.model_id, (routeCount.get(route.model_id) || 0) + 1));
    const modelRows = models.map((model) => ({ ...model, route_count: routeCount.get(model.id) || 0 }));

    if (!modelView) {
      modelView = table({
        columns: [
          { key: 'public_name', label: '对外模型名', render: (row) => el('code', { text: row.public_name }) },
          { key: 'display_name', label: '显示名' },
          { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
          { key: 'route_count', label: '路由数' },
          { key: 'aliases', label: '别名', render: (row) => (row.aliases || []).join(', ') || '—' },
          { key: 'sale_pricing', label: '对客定价', render: (row) => row.sale_pricing ? el('code', { text: JSON.stringify(row.sale_pricing) }) : el('span', { class: 'muted', text: '未设置' }) },
        ],
        rows: modelRows,
        rowActions: (row) => readonly ? [] : [el('button', { class: 'btn', text: '编辑', onclick: () => editModel(row, load) })],
      });
      page.append(card('模型', modelView.node, [el('span', { class: 'muted', text: '模型只启停不删除，避免历史账单失去模型名' })]));
    } else {
      modelView.refresh(modelRows);
    }

    const providerName = new Map(providers.map((p) => [p.id, p.name]));
    const routeRows = routes.map((route) => ({ ...route, provider: route.provider || providerName.get(route.provider_id) }));
    if (!routeView) {
      routeView = table({
        columns: [
          { key: 'model', label: '模型' },
          { key: 'provider', label: '供应商' },
          { key: 'upstream_model', label: '上游模型名', render: (row) => el('code', { text: row.upstream_model }) },
          { key: 'priority', label: '优先级' },
          { key: 'weight', label: '权重' },
          { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
          { key: 'cooldown_until', label: '冷却至' },
        ],
        rows: routeRows,
        rowActions: (row) => readonly ? [] : [
          el('button', { class: 'btn', text: '调整', onclick: () => editRoute(row, load) }),
          el('button', { class: 'btn btn-danger', text: '删除', onclick: () => removeRoute(row, load) }),
        ],
      });
      page.append(card('路由（同一模型可挂多个供应商做负载均衡）', routeView.node));
    } else {
      routeView.refresh(routeRows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  createModel.addEventListener('click', async () => {
    const result = await modal({
      title: '新建模型',
      fields: [
        { name: 'public_name', label: '对外模型名', required: true },
        { name: 'display_name', label: '显示名' },
        { name: 'aliases', label: '别名（JSON 数组）', type: 'textarea', json: true, value: '[]' },
      ],
      onSubmit: async (values) => { await api.post('/models', values); toast('已创建', 'ok'); await load(); return true; },
    });
    return result;
  });
  createRoute.addEventListener('click', async () => {
    const [modelsPayload, providersPayload] = await Promise.all([api.get('/models'), api.get('/providers')]);
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
        await load();
        return true;
      },
    });
  });
  await load();
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