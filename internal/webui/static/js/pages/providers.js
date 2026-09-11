import { api } from '../api.js';
import { el, card, table, modal, toast, badge, jsonBlock, formatTime, confirmDialog, withBusy } from '../ui.js';

const KINDS = ['openai-chat', 'openai-responses', 'testecho'];
const CONFIG_HINT = JSON.stringify({ base_url: 'https://api.example.com/v1' }, null, 2);

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '新建供应商', disabled: readonly });
  const kinds = el('button', { class: 'btn', text: '内建类型说明' });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, kinds, create);
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
        el('span', { class: 'muted', text: '内建类型开箱可用；插件类型填写 plugin:<名称>，凭据加密存储且永不回显。' }),
        el('span', { class: 'muted', text: '每个类型的全部配置字段与密钥填法见「内建类型说明」或详情页的「配置说明」。' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  create.addEventListener('click', () => createProvider({}, load));
  kinds.addEventListener('click', () => kindDocs(load).catch((err) => toast(api.errorMessage(err), 'error')));
  await load();
}

// createProvider opens the create form, optionally pre-filled from one kind's
// documented template (the "内建类型说明 → 用此模板新建" path).
async function createProvider(preset, reload) {
  const result = await modal({
    title: preset.kind ? '新建供应商（' + preset.kind + ' 模板）' : '新建供应商', wide: true, submitLabel: '创建',
    fields: [
      { name: 'name', label: '名称', required: true },
      { name: 'kind', label: '类型', required: true, value: preset.kind || '',
        hint: '内建：' + KINDS.join(' / ') + '；插件：plugin:名称。字段说明见「内建类型说明」' },
      { name: 'display_name', label: '显示名' },
      { name: 'priority', label: '优先级（越小越先）', type: 'number', value: 100 },
      { name: 'weight', label: '权重', type: 'number', value: 100 },
      { name: 'config', label: '配置（JSON，字段说明见详情页「配置说明」）', type: 'textarea', json: true,
        value: preset.config || CONFIG_HINT },
      { name: 'credentials', label: '凭据（JSON，只写不回显）', type: 'textarea', json: true, value: '{\n  "api_key": ""\n}' },
    ],
    onSubmit: async (values) => {
      const created = await api.post('/providers', values);
      toast('供应商已创建', 'ok');
      if (reload) await reload();
      return created;
    },
  });
  if (result) probe(result, null, reload);
}

async function detail(row, reload, readonly) {
  // The list payload stays lean; the detail payload carries the kind's schema so the
  // operator sees every supported field (and where the API key belongs) right here.
  const full = await api.get('/providers/' + row.id).catch(() => ({}));
  const provider = { ...row, ...full };
  const logs = await api.get('/providers/' + row.id + '/logs').catch(() => ({ data: [] }));
  const actions = await api.get('/providers/' + row.id + '/actions').catch(() => ({ data: [] }));
  const docsSlot = el('div', {});
  const body = el('div', {}, [
    el('div', { class: 'split' }, [
      kv('ID', String(row.id)),
      kv('类型', row.kind),
      kv('状态', row.enabled ? '启用' : '停用'),
      kv('配置版本', String(row.config_version)),
      kv('凭据键', (row.credential_keys || []).join(', ') || '无'),
      kv('冷却至', formatTime(row.cooldown_until)),
    ]),
    el('h4', { text: '配置说明' }), docsSlot,
    el('h4', { text: '配置' }), jsonBlock(provider.config),
    el('h4', { text: '最近探测' }), jsonBlock(provider.health),
    el('h4', { text: '发现信息' }), jsonBlock(provider.discovered),
    el('h4', { text: '进程日志（尾部）' }), el('pre', { class: 'mono', text: (logs.data || []).join('\n') || '（未运行或为内建供应商）' }),
  ]);
  if (actions.data && actions.data.length) {
    body.append(el('h4', { text: '可用动作' }), el('div', { class: 'toolbar' }, actions.data.map((action) =>
      el('button', { class: 'btn', text: action.title || action.name, disabled: readonly, onclick: () => runAction(row, action) }))));
  }

  // Re-rendering is how the plugin path picks up a freshly read handshake.
  function renderDocs(doc) {
    docsSlot.replaceChildren(docsSection(doc, {
      onPluginDeclared: async () => {
        const probed = await api.post('/providers/' + row.id + '/test?mode=info');
        if (!probed.ok) throw new Error(probed.error || '插件未响应');
        const refreshed = await api.get('/providers/' + row.id).catch(() => null);
        renderDocs({ ...doc, ...(refreshed || {}) });
        toast('已读取插件声明', 'ok');
      },
    }));
  }
  renderDocs(provider);

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

// ---------------------------------------------------------------------------
// configuration documentation
// ---------------------------------------------------------------------------

// docsSection renders one provider's (or kind's) configuration documentation:
// the kind note, the config field table, the credential field table and the
// template. It is pure data -> DOM, so it can be re-rendered after a plugin
// handshake without rebuilding the dialog around it.
function docsSection(doc, options) {
  const opts = options || {};
  const source = doc.schema_source || 'unknown';
  const wrap = el('div', {});
  if (doc.kind_note) wrap.append(el('p', { class: 'muted', text: doc.kind_note }));

  if (source === 'plugin' && !doc.config_schema) {
    const button = el('button', { class: 'btn', text: '读取插件声明（会启动/连接该插件进程）' });
    button.addEventListener('click', async () => {
      try {
        await withBusy(button, '读取中', () => opts.onPluginDeclared ? opts.onPluginDeclared() : Promise.resolve());
      } catch (err) {
        toast(api.errorMessage(err), 'error');
      }
    });
    wrap.append(el('p', { class: 'muted', text: '该供应商是插件：配置与凭据字段由插件在握手时声明，网关不预设。' }), button);
    if (doc.config) wrap.append(el('h5', { text: '当前配置的键' }), keyList(doc.config));
    return wrap;
  }

  wrap.append(el('h5', { text: '配置字段（config）' }), docTable(doc.config_schema));
  wrap.append(el('h5', { text: '凭据字段（credentials，加密落库、永不回显）' }), docTable(doc.credentials_schema));
  if (doc.config_template) {
    wrap.append(el('details', {}, [
      el('summary', { text: '配置模板（可复制到「编辑 → 配置」）' }), jsonBlock(doc.config_template),
    ]));
  }
  return wrap;
}

// keyList lists the keys an instance actually uses, for plugins that declare no
// schema: it says what is configured, never what is supported.
function keyList(config) {
  const keys = config && typeof config === 'object' ? Object.keys(config) : [];
  if (!keys.length) return el('span', { class: 'muted', text: '（当前配置为空）' });
  return el('div', { class: 'toolbar' }, keys.map((key) => badge(key)));
}

function docTable(schema) {
  if (!schema || typeof schema !== 'object') {
    return el('span', { class: 'muted', text: '（该类型未声明字段说明）' });
  }
  const rows = docRows(schema);
  if (!rows.length) return el('span', { class: 'muted', text: '（该类型未声明可配置字段）' });
  const basic = rows.filter((row) => !row.prop['x-advanced']);
  const advanced = rows.filter((row) => row.prop['x-advanced']);
  const wrap = el('div', {});
  if (basic.length) wrap.append(docTableNode(basic));
  if (advanced.length) {
    wrap.append(el('details', {}, [
      el('summary', { text: '高级（' + advanced.length + ' 项，默认值对多数部署可用）' }), docTableNode(advanced),
    ]));
  }
  return wrap;
}

function docTableNode(rows) {
  return table({
    columns: [
      { key: 'path', label: '字段', render: (row) => el('code', { text: row.path }) },
      { key: 'type', label: '类型', render: (row) => typeLabel(row.prop) },
      { key: 'default', label: '默认值', render: (row) => defaultLabel(row.prop) },
      { key: 'desc', label: '说明', render: (row) => descCell(row.prop, row.required) },
    ],
    rows,
  }).node;
}

// docRows flattens a schema into one row per field, dotted for nested objects and
// with [] for array elements, so nothing supported stays hidden. The schema-level
// `required` list is folded into the rows it names.
function docRows(schema) {
  const rows = [];
  function walk(props, prefix, required) {
    for (const [name, prop] of Object.entries(props || {})) {
      const value = prop && typeof prop === 'object' ? prop : {};
      const path = prefix ? prefix + '.' + name : name;
      const array = value.type === 'array';
      rows.push({ path: path + (array ? '[]' : ''), prop: value, required: !prefix && required.includes(name) });
      if (value.properties) walk(value.properties, path, []);
      if (array && value.items && value.items.properties) walk(value.items.properties, path + '[]', []);
    }
  }
  walk(schema.properties, '', Array.isArray(schema.required) ? schema.required : []);
  return rows;
}

function typeLabel(prop) {
  const type = prop.type || '—';
  if (Array.isArray(prop.enum) && prop.enum.length) {
    return el('span', {}, [el('code', { text: type }), el('span', { class: 'muted', text: ' ' + prop.enum.map((v) => (v === '' ? '(空)' : v)).join(' / ') })]);
  }
  return el('code', { text: type });
}

function defaultLabel(prop) {
  if (!('default' in prop)) return el('span', { class: 'muted', text: '—' });
  const value = prop.default;
  if (value === '') return el('code', { text: '""（空）' });
  return el('code', { text: typeof value === 'string' ? value : JSON.stringify(value) });
}

function descCell(prop, required) {
  const parts = [];
  if (prop.description) parts.push(el('span', { text: prop.description }));
  const marks = [];
  if (prop['x-required'] === true || required === true) marks.push(badge('必填', 'warn'));
  if (prop['x-secret'] === true) marks.push(badge('密钥', 'warn'));
  if (prop['x-prefer-credential']) marks.push(badge('建议填「凭据」栏'));
  if (marks.length) parts.push(el('div', { class: 'toolbar' }, marks));
  return parts.length ? el('div', {}, parts) : el('span', { class: 'muted', text: '—' });
}

// kindDocs is the pre-create reference: every builtin kind with its complete field
// table and template, plus a jump into the create form pre-filled with that kind.
async function kindDocs(reload) {
  const payload = await api.get('/provider-kinds');
  const kinds = payload.data || [];
  const body = el('div', {}, kinds.map((kind) => el('div', {}, [
    el('h4', { text: kind.kind }),
    docsSection(kind),
    el('div', { class: 'toolbar' }, [
      el('button', {
        class: 'btn', text: '用此模板新建',
        onclick: () => {
          backdrop.remove();
          createProvider({ kind: kind.kind, config: templateText(kind.config_template) }, reload);
        },
      }),
    ]),
  ])));
  const close = el('button', { class: 'btn', text: '关闭', onclick: () => backdrop.remove() });
  const dialog = el('div', { class: 'modal', style: 'width:min(1000px,100%)' }, [
    el('h3', { text: '内建供应商类型：支持的配置说明' }),
    el('p', { class: 'muted', text: '插件类型（plugin:<名称>）的字段由插件在握手时声明，见该供应商详情页的「配置说明」。' }),
    body,
    el('div', { class: 'modal-actions' }, [close]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) backdrop.remove(); });
  document.getElementById('modal-root').append(backdrop);
}

function templateText(template) {
  if (!template) return CONFIG_HINT;
  if (typeof template === 'string') {
    try { return JSON.stringify(JSON.parse(template), null, 2); } catch (err) { return template; }
  }
  return JSON.stringify(template, null, 2);
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
      { name: 'config', label: '配置（JSON，字段说明见详情页「配置说明」）', type: 'textarea', json: true, value: row.config },
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
