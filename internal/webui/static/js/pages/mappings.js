import { api } from '../api.js';
import { el, card, table, modal, toast, badge, confirmDialog, jsonBlock } from '../ui.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '新建规则', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);

  // The simulator renders the same resolution path the data plane uses.
  const probeInput = el('input', { placeholder: '输入客户端请求的模型名，例如 gpt-4o-mini' });
  const probeBtn = el('button', { class: 'btn', text: '试算' });
  const probeOut = el('div');
  probeBtn.addEventListener('click', async () => {
    if (!probeInput.value.trim()) return;
    try {
      const result = await api.get('/router/explain', { model: probeInput.value.trim() });
      probeOut.replaceChildren(jsonBlock(result));
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  });
  page.append(card('解析试算（与数据面同一套规则）', [
    el('div', { class: 'toolbar' }, [probeInput, probeBtn]), probeOut,
  ]));

  let view;
  async function load() {
    const payload = await api.get('/model-mappings');
    const rows = payload.data || [];
    if (!view) {
      view = table({
        columns: [
          { key: 'priority', label: '优先级' },
          { key: 'kind', label: '类型', render: (row) => badge(row.kind) },
          { key: 'pattern', label: '匹配', render: (row) => el('code', { text: row.pattern }) },
          { key: 'target_model', label: '目标模型' },
          { key: 'target_provider_id', label: '目标供应商', render: (row) => row.target_provider_id || '—' },
          { key: 'target_upstream_model', label: '上游模型名模板', render: (row) => el('code', { text: row.target_upstream_model || '—' }) },
          { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
        ],
        rows,
        rowActions: (row) => readonly ? [] : [el('button', { class: 'btn btn-danger', text: '删除', onclick: () => remove(row, load) })],
      });
      page.append(card('模型映射规则', view.node, [
        el('span', { class: 'muted', text: '按优先级从小到大匹配；模板支持 {model}、{1}、{name} 捕获组' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  create.addEventListener('click', async () => {
    const providers = (await api.get('/providers')).data || [];
    await modal({
      title: '新建映射规则', wide: true,
      fields: [
        { name: 'kind', label: '匹配类型', type: 'select', options: ['exact', 'prefix', 'glob', 'regex'] },
        { name: 'pattern', label: '匹配表达式', required: true, hint: '例如 gpt-* 或 ^gpt-(?P<name>.+)$' },
        { name: 'target_model', label: '目标模型（留空则需指定供应商）' },
        { name: 'target_provider_id', label: '目标供应商', type: 'select', options: [{ value: '', label: '（不钉死）' }].concat(providers.map((p) => ({ value: p.id, label: p.name }))) },
        { name: 'target_upstream_model', label: '上游模型名模板', placeholder: '{model}' },
        { name: 'priority', label: '优先级', type: 'number', value: 100 },
        { name: 'note', label: '备注' },
      ],
      onSubmit: async (values) => {
        const payload = { ...values, priority: Number(values.priority) };
        if (values.target_provider_id) payload.target_provider_id = Number(values.target_provider_id);
        else delete payload.target_provider_id;
        await api.post('/model-mappings', payload);
        toast('已创建', 'ok');
        await load();
        return true;
      },
    });
  });
  await load();
}

async function remove(row, reload) {
  const ok = await confirmDialog('删除映射规则', '确认删除 ' + row.kind + ' / ' + row.pattern + ' 吗？');
  if (!ok) return;
  await api.del('/model-mappings/' + row.id);
  toast('已删除', 'ok');
  await reload();
}