import { api } from '../api.js';
import { el, card, modal, toast, badge, withBusy } from '../ui.js';

// The route editor is deliberately model-centred: a route is unique per model/provider,
// so an operator can see and change the complete set without opening one dialog per row.
export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const createModel = el('button', { class: 'btn btn-primary', text: '新建模型', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, createModel);

  const filter = el('input', { class: 'route-model-filter', type: 'search', placeholder: '过滤模型名称、显示名或别名…', 'aria-label': '过滤模型' });
  const count = el('span', { class: 'muted' });
  const modelList = el('div', { class: 'route-model-list' });
  const detail = el('div', { class: 'route-editor-detail' });
  const left = el('section', { class: 'route-editor-pane route-editor-models' }, [
    el('div', { class: 'route-editor-pane-head' }, [el('h3', { text: '模型' })]),
    filter, count, modelList,
  ]);
  const right = el('section', { class: 'route-editor-pane route-editor-providers' }, [detail]);
  page.append(card('模型与路由', [
    el('p', { class: 'muted', text: '选择左侧模型，在右侧勾选可用供应商并填写各自的上游模型名；取消勾选并保存会删除该路由。' }),
    el('div', { class: 'route-editor' }, [left, right]),
  ]));

  let models = [];
  let providers = [];
  let routes = [];
  let selectedID = null;

  const selectedModel = () => models.find((model) => model.id === selectedID) || null;
  const routesForSelected = () => new Map(routes.filter((route) => route.model_id === selectedID)
    .map((route) => [route.provider_id, route]));

  function renderModels() {
    const needle = filter.value.trim().toLowerCase();
    const filtered = models.filter((model) => {
      const haystack = [model.public_name, model.display_name, ...(model.aliases || [])].join(' ').toLowerCase();
      return !needle || haystack.includes(needle);
    });
    count.textContent = needle ? '匹配 ' + filtered.length + ' / ' + models.length + ' 个模型' : '共 ' + models.length + ' 个模型';
    if (selectedID !== null && !models.some((model) => model.id === selectedID)) selectedID = null;
    modelList.replaceChildren(...(filtered.length ? filtered.map((model) => {
      const routeCount = routes.filter((route) => route.model_id === model.id).length;
      const button = el('button', {
        class: 'route-model-item' + (model.id === selectedID ? ' active' : ''),
        type: 'button', onclick: () => { selectedID = model.id; renderModels(); renderDetail(); },
      }, [
        el('span', { class: 'route-model-name' }, [el('code', { text: model.public_name }), model.enabled ? null : badge('停用', 'warn')]),
        el('span', { class: 'route-model-meta', text: (model.display_name || '未设置显示名') + ' · ' + routeCount + ' 个供应商' }),
      ]);
      return button;
    }) : [el('div', { class: 'empty', text: '没有匹配的模型' })]));
  }

  function renderDetail() {
    const model = selectedModel();
    if (!model) {
      detail.replaceChildren(el('div', { class: 'empty', text: models.length ? '请从左侧选择一个模型以编辑路由' : '还没有模型，请先新建模型' }));
      return;
    }
    const original = routesForSelected();
    const checked = new Map(Array.from(original, ([id, route]) => [id, { checked: true, upstream: route.upstream_model || model.public_name }]));
    const providerList = el('div', { class: 'route-provider-list' });
    const edit = el('button', { class: 'btn', text: '编辑模型', disabled: readonly });
    const save = el('button', { class: 'btn btn-primary', text: '保存路由', disabled: readonly });
    const header = el('div', { class: 'route-editor-pane-head' }, [
      el('div', {}, [el('h3', { text: '供应商路由' }), el('div', { class: 'muted', text: '对外模型：' + model.public_name })]),
      el('div', { class: 'actions' }, [edit, save]),
    ]);
    edit.addEventListener('click', () => editModel(model, () => reload(model.id)));

    function renderProviders() {
      providerList.replaceChildren(...(providers.length ? providers.map((provider) => {
        const state = checked.get(provider.id) || { checked: false, upstream: model.public_name };
        const checkboxID = 'route_provider_' + model.id + '_' + provider.id;
        const checkbox = el('input', { id: checkboxID, type: 'checkbox', disabled: readonly });
        checkbox.checked = state.checked;
        checkbox.addEventListener('change', () => {
          checked.set(provider.id, { checked: checkbox.checked, upstream: state.upstream || model.public_name });
          renderProviders();
        });
        const row = el('div', { class: 'route-provider-row' }, [
          el('label', { class: 'route-provider-choice', for: checkboxID }, [checkbox, el('span', {}, [
            el('strong', { text: provider.name }),
            el('span', { class: 'muted', text: provider.kind + (provider.enabled ? '' : ' · 已停用') }),
          ])]),
        ]);
        if (state.checked) {
          const upstream = el('input', { type: 'text', value: state.upstream, placeholder: '上游模型名', disabled: readonly, 'aria-label': provider.name + ' 的上游模型名' });
          upstream.addEventListener('input', () => checked.set(provider.id, { checked: true, upstream: upstream.value }));
          row.append(el('label', { class: 'route-upstream-field' }, [el('span', { text: '上游模型名' }), upstream]));
        }
        return row;
      }) : [el('div', { class: 'empty', text: '还没有供应商，请先在“模型供应商”页新建供应商' })]));
    }

    save.addEventListener('click', async () => {
      const next = new Map(Array.from(checked).filter(([, state]) => state.checked));
      for (const [providerID, state] of next) {
        if (!state.upstream.trim()) {
          toast((providers.find((provider) => provider.id === providerID)?.name || '供应商') + ' 的上游模型名必填', 'error');
          return;
        }
      }
      try {
        await withBusy(save, '保存中', async () => {
          const writes = [];
          for (const [providerID, state] of next) {
            const existing = original.get(providerID);
            if (!existing) writes.push(api.post('/routes', { model_id: model.id, provider_id: providerID, upstream_model: state.upstream.trim(), priority: 100, weight: 100, enabled: true }));
            else if (existing.upstream_model !== state.upstream.trim()) writes.push(api.patch('/routes/' + existing.id, { upstream_model: state.upstream.trim() }));
          }
          for (const [providerID, existing] of original) if (!next.has(providerID)) writes.push(api.del('/routes/' + existing.id));
          await Promise.all(writes);
        });
        toast('路由已保存', 'ok');
        await reload(model.id);
      } catch (err) {
        toast('保存未完全完成：' + api.errorMessage(err) + '。已重新加载服务端状态。', 'error');
        await reload(model.id);
      }
    });
    detail.replaceChildren(header, providerList);
    renderProviders();
  }

  async function reload(preferredID) {
    try {
      const [modelsPayload, providersPayload, routesPayload] = await Promise.all([
        api.get('/models', { limit: 1000 }), api.get('/providers', { limit: 1000 }), api.get('/routes', { limit: 1000 }),
      ]);
      models = modelsPayload.data || [];
      providers = providersPayload.data || [];
      routes = routesPayload.data || [];
      selectedID = preferredID && models.some((model) => model.id === preferredID) ? preferredID : (selectedID || models[0]?.id || null);
      renderModels(); renderDetail();
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  }

  filter.addEventListener('input', renderModels);
  refresh.addEventListener('click', () => reload(selectedID));
  createModel.addEventListener('click', async () => {
    await modal({
      title: '新建模型',
      fields: [
        { name: 'public_name', label: '对外模型名', required: true },
        { name: 'display_name', label: '显示名' },
        { name: 'aliases', label: '别名（JSON 数组）', type: 'textarea', json: true, value: '[]' },
        { name: 'reasoning_mode', label: '推理设置', type: 'select', options: reasoningModeOptions(), hint: reasoningModeHint() },
        { name: 'reasoning_effort', label: '推理强度（默认/强制模式生效）', type: 'select', options: reasoningEffortOptions(), hint: reasoningEffortHint() },
      ],
      onSubmit: async (values) => { const created = await api.post('/models', modelPayload(values)); toast('已创建', 'ok'); await reload(created.id); return true; },
    });
  });
  await reload();
}

function reasoningModeOptions() { return [{ value: 'inherit', label: '继承（清除模型级设置）' }, { value: 'default', label: '默认（未指定时使用以下强度）' }, { value: 'force', label: '强制（总是使用以下强度）' }]; }
async function editModel(model, reload) {
  await modal({
    title: '编辑模型 ' + model.public_name,
    fields: [
      { name: 'display_name', label: '显示名', value: model.display_name },
      { name: 'enabled', label: '启用', type: 'checkbox', value: model.enabled },
      { name: 'aliases', label: '别名（JSON 数组）', type: 'textarea', json: true, value: model.aliases || [] },
      { name: 'sale_pricing', label: '对客定价（JSON，M11a 计价引擎使用）', type: 'textarea', json: true, value: model.sale_pricing || '' },
      { name: 'reasoning_mode', label: '推理设置', type: 'select', options: reasoningModeOptions(), value: model.reasoning?.mode || 'inherit', hint: reasoningModeHint() },
      { name: 'reasoning_effort', label: '推理强度（默认/强制模式生效）', type: 'select', options: reasoningEffortOptions(), value: model.reasoning?.effort || 'medium', hint: reasoningEffortHint() },
    ],
    onSubmit: async (values) => { await api.patch('/models/' + encodeURIComponent(model.public_name), modelPayload(values)); toast('已保存', 'ok'); await reload(); return true; },
  });
}
function reasoningModeHint() { return '默认模式会保留客户端明确指定的强度（包括 none）；强制模式会覆盖强度，但保留请求的 summary。仅作用于该对外模型，所有路由共享此设置。'; }
function reasoningEffortHint() { return '仅默认/强制模式生效；请选目标上游支持的等级，不支持时由上游拒绝请求。'; }
function reasoningEffortOptions() { return ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].map((value) => ({ value, label: value })); }
function modelPayload({ reasoning_mode, reasoning_effort, ...values }) { values.reasoning = reasoning_mode === 'inherit' ? null : { mode: reasoning_mode, effort: reasoning_effort }; return values; }
