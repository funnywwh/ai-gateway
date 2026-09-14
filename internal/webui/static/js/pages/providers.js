import { api } from '../api.js';
import { el, card, table, pagedTable, modal, toast, badge, jsonBlock, formatTime, confirmDialog, withBusy, modalHead, modalBody, modalActions } from '../ui.js';

const KINDS = ['openai-chat', 'openai-responses', 'testecho'];
const CONFIG_HINT = JSON.stringify({ base_url: 'https://api.example.com/v1' }, null, 2);

// The ceiling is enforced with a queue (M44): saying "并发上限" alone leaves an operator to
// guess whether the excess is refused or waits.
const CAPACITY_HINT = '同时在途的上游调用数；0=不限。超出后请求排队等待（部署配置决定等待上限，默认 30 秒），'
  + '等待超时或队列已满时该请求换下一个候选，全部候选耗尽返回 429 provider_busy。探测/重启不占名额。'

// capacityText renders one provider's live gate. A provider with no ceiling has no gate at
// all, which is not the same statement as "zero in flight", so it is shown as 不限.
function capacityText(capacity) {
  if (!capacity) return '不限';
  const limit = capacity.limit > 0 ? String(capacity.limit) : '不限';
  let text = capacity.inflight + ' 在途（上限 ' + limit + '）';
  if (capacity.waiting > 0) text += '，' + capacity.waiting + ' 排队';
  return text;
}

function capacityCell(row) {
  const capacity = row.capacity;
  if (!capacity) return '—';
  const detail = ['累计放行 ' + capacity.admitted]
    .concat(capacity.timed_out ? ['排队超时 ' + capacity.timed_out] : [])
    .concat(capacity.queue_full ? ['队满拒绝 ' + capacity.queue_full] : [])
    .concat(capacity.cancelled ? ['排队中取消 ' + capacity.cancelled] : [])
    .join('；');
  return el('span', {
    text: capacity.inflight + '/' + (capacity.limit > 0 ? capacity.limit : '不限')
      + (capacity.waiting > 0 ? '，' + capacity.waiting + ' 排队' : ''),
    title: detail,
  });
}

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '新建供应商', disabled: readonly });
  const kinds = el('button', { class: 'btn', text: '内建类型说明' });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, kinds, create);

  const view = pagedTable({
    columns: [
      { key: 'name', label: '名称' },
      { key: 'kind', label: '类型', render: (row) => el('code', { text: row.kind }) },
      { key: 'enabled', label: '启用', render: (row) => row.enabled ? badge('on', 'ok') : badge('off') },
      { key: 'priority', label: '优先级' },
      { key: 'weight', label: '权重' },
      { key: 'capacity', label: '在途/排队', render: (row) => capacityCell(row) },
      { key: 'has_credentials', label: '凭据', render: (row) => row.has_credentials
        ? badge((row.credential_keys || []).join(', ') || '已配置', 'ok') : badge('未配置', 'warn') },
      { key: 'last_error', label: '最近错误', render: (row) => row.last_error ? el('span', { class: 'muted', text: row.last_error }) : '—' },
      { key: 'updated_at', label: '更新时间', render: (row) => formatTime(row.updated_at) },
    ],
    rowActions: (row) => [
      el('button', { class: 'btn', text: '详情', onclick: () => detail(row, () => view.refresh(), readonly) }),
      el('button', { class: 'btn', text: '探测', onclick: (ev) => probe(row, ev.currentTarget, () => view.refresh()) }),
      readonly ? null : el('button', { class: 'btn btn-danger', text: '删除', onclick: () => remove(row, () => view.refresh()) }),
    ].filter(Boolean),
    load: ({ limit, offset }) => api.get('/providers', { limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('模型供应商', view.node, [
    el('span', { class: 'muted', text: '内建类型开箱可用；插件类型填写 plugin:<名称>，凭据加密存储且永不回显。' }),
    el('span', { class: 'muted', text: '每个类型的全部配置字段与密钥填法见「内建类型说明」或详情页的「配置说明」。' })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', () => createProvider({}, () => view.refresh()));
  kinds.addEventListener('click', () => kindDocs(() => view.refresh()).catch((err) => toast(api.errorMessage(err), 'error')));
  await view.refresh();
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
      { name: 'max_inflight', label: '最大并发（0=不限）', type: 'number', value: 0, hint: CAPACITY_HINT },
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
  const modelsSlot = el('div', {});
  const addModel = el('button', { class: 'btn btn-primary', text: '新增模型映射', disabled: readonly });
  const body = el('div', {}, [
    el('div', { class: 'split' }, [
      kv('ID', String(row.id)),
      kv('类型', row.kind),
      kv('状态', row.enabled ? '启用' : '停用'),
      kv('配置版本', String(row.config_version)),
      kv('凭据键', (row.credential_keys || []).join(', ') || '无'),
      kv('冷却至', formatTime(row.cooldown_until)),
      kv('在途/排队', capacityText(row.capacity)),
    ]),
    // The mapping decides whether the routes pointing here can be used at all, so it
    // comes before the configuration documentation: a model and a route without this
    // row is the "configured it and it still does not work" case.
    el('h4', { text: '模型映射' }),
    el('div', { class: 'toolbar' }, [addModel]),
    modelsSlot,
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

  const reloadModels = () => loadProviderModels(row.id, modelsSlot, readonly).catch((err) => {
    modelsSlot.replaceChildren(el('div', { class: 'empty', text: api.errorMessage(err) }));
  });
  addModel.addEventListener('click', async () => {
    if (await modelForm(row.id, null)) await reloadModels();
  });
  reloadModels();

  const editBtn = el('button', { class: 'btn btn-primary', text: '编辑', disabled: readonly });
  const refreshModels = el('button', { class: 'btn', text: '刷新模型发现' });
  const restart = el('button', { class: 'btn', text: '重启进程', disabled: readonly });
  const close = el('button', { class: 'btn', text: '关闭' });
  const dialog = el('div', { class: 'modal', style: 'width:min(900px,100%)' }, [
    modalHead('供应商 ' + row.name, () => backdrop.remove()),
    modalBody([body]),
    modalActions([refreshModels, restart, editBtn, close]),
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
    if (result.ok) await reloadModels();
  });
  document.getElementById('modal-root').append(backdrop);
}

// ---------------------------------------------------------------------------
// provider model mappings
// ---------------------------------------------------------------------------

// loadProviderModels renders one provider's mappings, plus the warning that matters
// most: a route pointing at this provider with no mapping here is excluded by the
// router as "not_mapped", and GET /v1/models then drops the model without a word.
// Before this section existed the console had no way to see or create that row at all,
// which made "I added the model and the route, why is it not there?" unanswerable.
async function loadProviderModels(providerID, slot, readonly) {
  const [payload, routesPayload] = await Promise.all([
    api.get('/providers/' + providerID + '/models', { limit: 500 }),
    api.get('/routes', { limit: 1000 }).catch(() => ({ data: [] })),
  ]);
  const list = payload.data || [];
  const mapped = new Set(list.map((pm) => pm.public_model));
  const orphans = (routesPayload.data || []).filter((route) => route.provider_id === providerID && !mapped.has(route.model));

  const nodes = [];
  if (orphans.length) {
    nodes.push(el('p', {}, [
      badge(orphans.length + ' 条路由缺映射', 'warn'),
      el('span', { text: ' ' + orphans.map((route) => route.model).join('、') +
        '：路由指向本供应商，但这里没有对应映射，路由会被判为 not_mapped —— 模型既不出现在 /v1/models，' +
        '客户请求也拿不到候选，而且不会报任何错。用「新增模型映射」补上（对客名必须与模型/路由里的名字一致）。' }),
    ]));
  }
  nodes.push(table({
    columns: [
      { key: 'public_model', label: '对客模型' },
      { key: 'upstream_model', label: '上游模型' },
      { key: 'enabled', label: '启用', render: (pm) => (pm.enabled ? badge('on', 'ok') : badge('off')) },
      { key: 'capabilities', label: '能力', render: (pm) => capabilityBadges(pm.capabilities) },
      { key: 'context_window', label: '上下文' },
      { key: 'max_output_tokens', label: '最大输出' },
      { key: 'pricing_rules', label: '成本规则', render: (pm) => (pm.pricing_rules ? badge('有', 'ok') : badge('无', 'warn')) },
      { key: 'source', label: '来源' },
    ],
    rows: list,
    empty: '还没有任何模型映射：模型与路由都建好了，这个供应商也供不了它（模型不会出现在 /v1/models）',
    rowActions: (pm) => [
      el('button', { class: 'btn', text: '编辑', disabled: readonly, onclick: async () => {
        if (await modelForm(providerID, pm)) await loadProviderModels(providerID, slot, readonly);
      } }),
      readonly ? null : el('button', { class: 'btn btn-danger', text: '删除', onclick: async () => {
        if (!await confirmDialog('删除模型映射', '删除后该供应商不再提供 ' + pm.public_model + '，指向它的路由会被判为 not_mapped（模型随之从 /v1/models 消失）。')) return;
        try {
          await api.del('/provider-models/' + pm.id);
          toast('已删除', 'ok');
        } catch (err) {
          toast(api.errorMessage(err), 'error');
        }
        await loadProviderModels(providerID, slot, readonly);
      } }),
    ].filter(Boolean),
  }).node);
  slot.replaceChildren(...nodes);
}

// capabilityBadges shows what the mapping declares. An empty set is not cosmetic: the
// router matches requested features against it, so a client sending tools or reasoning
// is either downgraded or rejected depending on routing.degradation.
function capabilityBadges(caps) {
  const declared = Object.keys(caps || {}).filter((key) => caps[key]);
  if (!declared.length) return badge('未声明', 'warn');
  return el('span', {}, declared.map((name) => badge(name)));
}

// modelForm creates or edits one mapping. Leaving a JSON field empty omits the key, and
// the API keeps the stored value for it; typing null clears it.
async function modelForm(providerID, pm) {
  const editing = !!pm;
  return modal({
    title: editing ? '编辑模型映射 · ' + pm.public_model : '新增模型映射',
    wide: true, submitLabel: editing ? '保存' : '创建',
    fields: [
      { name: 'public_model', label: '对客模型名', required: true, readonly: editing, value: pm ? pm.public_model : '',
        hint: editing ? '对客名是这条映射的身份，改不了；要改名请新建一条再删掉旧的' : '必须与「模型」页和「路由」页里用的名字逐字符一致' },
      { name: 'upstream_model', label: '上游模型名', value: pm ? pm.upstream_model : '',
        hint: '留空 = 与对客名相同（新行）；编辑时留空 = 保持原值' },
      { name: 'enabled', label: '启用', type: 'checkbox', value: pm ? pm.enabled !== false : true },
      { name: 'priority', label: '优先级（越小越优先）', type: 'number', value: pm ? pm.priority : 100 },
      { name: 'weight', label: '权重', type: 'number', value: pm ? pm.weight : 100 },
      { name: 'context_window', label: '上下文窗口（token，0 = 未声明）', type: 'number', value: pm ? pm.context_window : 0 },
      { name: 'max_output_tokens', label: '最大输出（token，0 = 未声明）', type: 'number', value: pm ? pm.max_output_tokens : 0 },
      { name: 'capabilities', label: '能力（JSON）', type: 'textarea', rows: 4, json: true,
        value: pm && pm.capabilities ? pm.capabilities : '',
        hint: '例如 {"stream":true,"tools":true,"reasoning":true}。留空 = 保持原值（新建则未声明），填 null 清空' },
      { name: 'capabilities_override', label: '能力校验', type: 'select', options: ['', 'inherit', 'strip', 'reject'],
        value: pm ? pm.capabilities_override || '' : '', hint: '空 = inherit（按能力表判定）' },
      { name: 'pricing_rules', label: '成本规则（JSON）', type: 'textarea', rows: 8, json: true,
        value: pm && pm.pricing_rules ? pm.pricing_rules : '',
        hint: '留空 = 保持原值（新建则不计成本），填 null 清空。售价默认按成本 × 倍数' },
    ],
    onSubmit: async (values) => {
      await api.post('/providers/' + providerID + '/models', values);
      toast(editing ? '已保存' : '已创建', 'ok');
      return true;
    },
  });
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
    modalHead('内建供应商类型：支持的配置说明', () => backdrop.remove()),
    modalBody([
      el('p', { class: 'muted', text: '插件类型（plugin:<名称>）的字段由插件在握手时声明，见该供应商详情页的「配置说明」。' }),
      body,
    ]),
    modalActions([close]),
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
      { name: 'max_inflight', label: '最大并发（0=不限）', type: 'number', value: row.max_inflight, hint: CAPACITY_HINT },
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
  const dialog = el('div', { class: 'modal' }, [modalHead('动作结果', close), modalBody([body])]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  // The result is read-only text, so the round ✕ is the close affordance. Closing
  // is bound to the backdrop (not to the dialog, which used to dismiss the result
  // on any click inside it, including a text selection).
  function close() { backdrop.remove(); }
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(); });
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
