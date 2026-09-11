import { api } from '../api.js';
import { el, card, table, modal, toast, badge, jsonBlock, formatTime, confirmDialog, withBusy } from '../ui.js';

const KINDS = ['openai-chat', 'openai-responses', 'testecho'];
const CONFIG_HINT = JSON.stringify({ base_url: 'https://api.example.com/v1' }, null, 2);

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '新建供应商', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);
  let view;

  async function load() {
    const payload = await api.get('/providers');
    const rows = payload.data || [];
    if (!view) {
      view = table({
        columns: [
          { key: 'name', label: '名称' },
          { key: 'kind', label: '类型', render: (row) => el('code', { text: row.kind }) },
          { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
          { key: 'priority', label: '优先级' },
          { key: 'weight', label: '权重' },
          { key: 'has_credentials', label: '凭据', render: (row) => row.has_credentials
            ? badge((row.credential_keys || []).join(', ') || '已配置', 'ok') : badge('未配置', 'warn') },
          { key: 'last_error', label: '最近错误', render: (row) => row.last_error ? el('span', { class: 'muted', text: row.last_error }) : '—' },
          { key: 'updated_at', label: '更新时间', render: (row) => formatTime(row.updated_at) },
        ],
        rows,
        rowActions: (row) => [
          el('button', { class: 'btn', text: '详情', onclick: () => detail(row, load, readonly) }),
          el('button', { class: 'btn', text: '探测', onclick: (ev) => probe(row, ev.currentTarget, load) }),
          readonly ? null : el('button', { class: 'btn btn-danger', text: '删除', onclick: () => remove(row, load) }),
        ].filter(Boolean),
      });
      page.append(card('模型供应商', view.node, [
        el('span', { class: 'muted', text: '内建类型开箱可用；插件类型填写 plugin:<名称>，凭据加密存储且永不回显' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  create.addEventListener('click', async () => {
    const result = await modal({
      title: '新建供应商', wide: true, submitLabel: '创建',
      fields: [
        { name: 'name', label: '名称', required: true },
        { name: 'kind', label: '类型', required: true, hint: '内建：' + KINDS.join(' / ') + '；插件：plugin:名称' },
        { name: 'display_name', label: '显示名' },
        { name: 'priority', label: '优先级（越小越先）', type: 'number', value: 100 },
        { name: 'weight', label: '权重', type: 'number', value: 100 },
        { name: 'config', label: '配置（JSON）', type: 'textarea', json: true, value: CONFIG_HINT },
        { name: 'credentials', label: '凭据（JSON，只写不回显）', type: 'textarea', json: true, value: '{\n  "api_key": ""\n}' },
      ],
      onSubmit: async (values) => {
        const created = await api.post('/providers', values);
        toast('供应商已创建', 'ok');
        await load();
        return created;
      },
    });
    if (result) probe(result, null, load);
  });
  await load();
}

async function detail(row, reload, readonly) {
  const logs = await api.get('/providers/' + row.id + '/logs').catch(() => ({ data: [] }));
  const actions = await api.get('/providers/' + row.id + '/actions').catch(() => ({ data: [] }));
  const body = el('div', {}, [
    el('div', { class: 'split' }, [
      kv('ID', String(row.id)),
      kv('类型', row.kind),
      kv('状态', row.enabled ? '启用' : '停用'),
      kv('配置版本', String(row.config_version)),
      kv('凭据键', (row.credential_keys || []).join(', ') || '无'),
      kv('冷却至', formatTime(row.cooldown_until)),
    ]),
    el('h4', { text: '配置' }), jsonBlock(row.config),
    el('h4', { text: '最近探测' }), jsonBlock(row.health),
    el('h4', { text: '发现信息' }), jsonBlock(row.discovered),
    el('h4', { text: '进程日志（尾部）' }), el('pre', { class: 'mono', text: (logs.data || []).join('\n') || '（未运行或为内建供应商）' }),
  ]);
  if (actions.data && actions.data.length) {
    body.append(el('h4', { text: '可用动作' }), el('div', { class: 'toolbar' }, actions.data.map((action) =>
      el('button', { class: 'btn', text: action.title || action.name, disabled: readonly, onclick: () => runAction(row, action) }))));
  }

  const editBtn = el('button', { class: 'btn btn-primary', text: '编辑', disabled: readonly });
  const refreshModels = el('button', { class: 'btn', text: '刷新模型发现' });
  const restart = el('button', { class: 'btn', text: '重启进程', disabled: readonly });
  const close = el('button', { class: 'btn', text: '关闭' });
  const dialog = el('div', { class: 'modal', style: 'width:min(900px,100%)' }, [
    el('h3', { text: '供应商 ' + row.name }), body,
    el('div', { class: 'modal-actions' }, [refreshModels, restart, editBtn, close]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  close.addEventListener('click', () => backdrop.remove());
  editBtn.addEventListener('click', async () => { backdrop.remove(); await edit(row, reload); });
  restart.addEventListener('click', async () => {
    await api.post('/providers/' + row.id + '/restart');
    toast('已请求重启', 'ok');
  });
  refreshModels.addEventListener('click', async () => {
    const result = await api.post('/providers/' + row.id + '/models/refresh');
    toast(result.ok ? ('发现 ' + result.discovered + ' 个模型，新增 ' + result.added) : ('发现失败：' + result.error), result.ok ? 'ok' : 'error');
  });
  document.getElementById('modal-root').append(backdrop);
}

function kv(label, value) { return el('div', {}, [el('div', { class: 'muted', text: label }), el('div', { text: value })]); }

async function edit(row, reload) {
  const result = await modal({
    title: '编辑 ' + row.name, wide: true,
    fields: [
      { name: 'display_name', label: '显示名', value: row.display_name },
      { name: 'enabled', label: '启用', type: 'checkbox', value: row.enabled },
      { name: 'draining', label: '排空（不再接新请求）', type: 'checkbox', value: row.draining },
      { name: 'priority', label: '优先级', type: 'number', value: row.priority },
      { name: 'weight', label: '权重', type: 'number', value: row.weight },
      { name: 'max_inflight', label: '最大在途（0=不限）', type: 'number', value: row.max_inflight },
      { name: 'degradation', label: '能力降级策略', type: 'select', options: ['', 'none', 'fail_fast', 'best_effort'], value: row.degradation },
      { name: 'config', label: '配置（JSON）', type: 'textarea', json: true, value: row.config },
      { name: 'credentials', label: '凭据（JSON，留空=保持不变，{} = 清空）', type: 'textarea', json: true, value: '' },
      { name: 'reset_cooldown', label: '清除冷却', type: 'checkbox' },
    ],
    onSubmit: async (values) => {
      const payload = { ...values };
      if (payload.credentials === undefined) delete payload.credentials;
      const updated = await api.patch('/providers/' + row.id, payload);
      toast('已保存', 'ok');
      await reload();
      return updated;
    },
  });
  return result;
}

// probe exercises one provider. A health probe is a real upstream request (the codex
// adapter streams a completion), so it routinely takes tens of seconds: the button
// carries the progress so a slow answer never reads as a hung page. Errors are caught
// too — a probe can outlive a client or proxy timeout, and failing silently would
// leave the operator with no feedback at all.
async function probe(row, button, reload) {
  const pending = button ? null : toast(row.name + ' 正在探测…', '', { sticky: true });
  try {
    const result = await withBusy(button, '探测中',
      () => api.post('/providers/' + row.id + '/test?mode=health'));
    toast(result.ok
      ? row.name + ' 正常（' + result.latency_ms + 'ms）'
      : row.name + ' 探测失败：' + result.error,
      result.ok ? 'ok' : 'error');
  } catch (err) {
    toast(row.name + ' 探测失败：' + api.errorMessage(err), 'error');
  } finally {
    if (pending) pending.remove();
  }
  // The probe persists health/last_error, so refresh the row to show what it found.
  if (reload) await reload().catch(() => {});
}

async function runAction(row, action) {
  const params = await modal({
    title: '执行动作：' + (action.title || action.name),
    fields: [{ name: 'params', label: '参数（JSON）', type: 'textarea', json: true, value: '' }],
    onSubmit: (values) => values.params,
  });
  if (params === null) return;
  const result = await api.post('/providers/' + row.id + '/actions/' + action.name, params);
  toast('动作已执行', 'ok');
  const body = el('pre', { class: 'mono', text: JSON.stringify(result.result, null, 2) });
  const dialog = el('div', { class: 'modal' }, [el('h3', { text: '动作结果' }), body]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  dialog.addEventListener('click', () => backdrop.remove());
  document.getElementById('modal-root').append(backdrop);
}

async function remove(row, reload) {
  const ok = await confirmDialog('删除供应商', '确认删除 ' + row.name + ' 吗？引用了它的路由会被一并删除。');
  if (!ok) return;
  try {
    await api.del('/providers/' + row.id + '?force=true');
  } catch (err) {
    toast(api.errorMessage(err), 'error');
    return;
  }
  toast('已删除', 'ok');
  await reload();
}