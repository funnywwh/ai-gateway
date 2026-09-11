import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, statusBadge, formatTime, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';

// Accepted values mirror config.RecordingInputModes plus "inherit"; internal/webui's
// test asserts the console and the server never drift apart.
const INPUT_MODES = [
  { value: 'inherit', label: '继承全局默认（默认：只记用户输入）' },
  { value: 'user', label: 'user：只记用户输入' },
  { value: 'full', label: 'full：整份请求正文（排障用）' },
  { value: 'metadata', label: 'metadata：只记元数据（不落正文）' },
  { value: 'off', label: 'off：不记录输入' },
];

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  const create = el('button', { class: 'btn btn-primary', text: '新建 Key', disabled: readonly });
  actions.append(refresh, create);

  // 账户下拉框要一次拿全：显式请求上限 1000（配置类列表的服务端上限），分页表格不带这个 limit。
  const accountsPromise = api.get('/accounts', { limit: 1000 });
  const view = pagedTable({
    columns: [
      { key: 'name', label: '名称' },
      { key: 'account', label: '账户' },
      { key: 'key_prefix', label: '前缀', render: (row) => el('code', { text: row.key_prefix + '…' }) },
      { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
      { key: 'tags', label: '标签', render: (row) => (row.tags || []).join(', ') || '—' },
      { key: 'record_input_mode', label: '输入录制' },
      { key: 'record_output_text', label: '输出文本', render: (row) => (row.record_output_text ? '已开启' : '关闭') },
      { key: 'record_reasoning', label: '思考文本', render: (row) => (row.record_reasoning ? '已开启' : '关闭') },
      { key: 'policy', label: '配额', render: (row) => el('code', { text: JSON.stringify(row.policy || {}) }) },
      { key: 'last_used_at', label: '最近使用', render: (row) => formatTime(row.last_used_at) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => editKey(row, () => view.refresh()) }),
      el('button', { class: 'btn', text: row.status === 'active' ? '停用' : '启用', onclick: () => toggle(row, () => view.refresh()) }),
    ],
    // 账户名来自上面那份完整列表；Key 列表本身由服务端分页。
    load: async ({ limit, offset }) => {
      const [payload, accountsPayload] = await Promise.all([api.get('/keys', { limit, offset }), accountsPromise]);
      const byId = new Map(((accountsPayload.data) || []).map((a) => [a.id, a.name]));
      return { ...payload, data: (payload.data || []).map((key) => ({ ...key, account: byId.get(key.account_id) || ('#' + key.account_id) })) };
    },
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('API Keys', view.node, [el('span', { class: 'muted', text: '明文只在创建时显示一次；默认只记录用户输入，思考与最终输出需单独勾选' })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', async () => {
    const accounts = (await accountsPromise).data || [];
    if (!accounts.length) { toast('请先创建一个账户', 'error'); return; }
    const result = await modal({
      title: '新建 API Key',
      submitLabel: '创建',
      fields: [
        { name: 'name', label: '名称', required: true },
        { name: 'account_id', label: '账户', type: 'select', options: accounts.map((a) => ({ value: a.id, label: a.name })) },
        { name: 'tags', label: '标签（逗号分隔）', hint: '标签决定可用的模型与供应商' },
        { name: 'record_input_mode', label: '输入录制模式', type: 'select', options: INPUT_MODES, value: 'inherit' },
        { name: 'record_output_text', label: '记录最终输出文本', type: 'checkbox' },
        { name: 'record_reasoning', label: '记录思考文本', type: 'checkbox' },
      ],
      onSubmit: async (values) => {
        const created = await api.post('/keys', {
          name: values.name,
          account_id: Number(values.account_id),
          tags: values.tags ? values.tags.split(',').map((t) => t.trim()).filter(Boolean) : [],
        });
        // Recording switches are per-key policy, applied right after creation so a
        // closed dialog cannot leave the key in an unintended recording state.
        await api.patch('/keys/' + created.id, {
          record_input_mode: values.record_input_mode || 'inherit',
          record_output_text: !!values.record_output_text,
          record_reasoning: !!values.record_reasoning,
        });
        return created;
      },
    });
    if (!result) return;
    showSecret('API Key 已创建', result.key, () => view.refresh());
  });

  await view.refresh();
}

async function editKey(row, reload) {
  const result = await modal({
    title: '编辑 ' + row.name,
    fields: [
      { name: 'record_input_mode', label: '输入录制模式', type: 'select', options: INPUT_MODES, value: row.record_input_mode },
      { name: 'record_output_text', label: '记录最终输出文本', type: 'checkbox', value: row.record_output_text },
      { name: 'record_reasoning', label: '记录思考文本', type: 'checkbox', value: row.record_reasoning },
      { name: 'status', label: '状态', type: 'select', options: ['active', 'suspended', 'revoked'], value: row.status },
      // Quota policy. Only the flat fields are read (rpm/tpm/concurrency are enforced);
      // anything else is rejected by the server instead of being stored and ignored.
      { name: 'policy', label: '配额策略（扁平 JSON）', type: 'textarea', json: true, rows: 6,
        hint: '例：{"rpm":60,"concurrency":4}；留空=不改动', value: row.policy ? JSON.stringify(row.policy, null, 2) : '' },
    ],
    onSubmit: (values) => api.patch('/keys/' + row.id, {
      record_input_mode: values.record_input_mode,
      record_output_text: !!values.record_output_text,
      record_reasoning: !!values.record_reasoning,
      status: values.status,
      ...(values.policy === undefined ? {} : { policy: values.policy }),
    }),
  });
  if (result) { toast('已更新', 'ok'); await reload(); }
}

async function toggle(row, reload) {
  const next = row.status === 'active' ? 'suspended' : 'active';
  const ok = await confirmDialog(next === 'suspended' ? '停用 Key' : '启用 Key',
    '确认把 ' + row.name + ' 置为 ' + next + ' 吗？');
  if (!ok) return;
  await api.patch('/keys/' + row.id, { status: next });
  toast('已更新', 'ok');
  await reload();
}

function showSecret(title, secret, after) {
  const box = el('input', { value: secret, readonly: true });
  const done = el('button', { class: 'btn btn-primary', text: '我已保存' });
  const copy = el('button', { class: 'btn', text: '复制' });
  copy.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(secret); toast('已复制', 'ok'); }
    catch (err) { box.select(); document.execCommand('copy'); }
  });
  const dialog = el('div', { class: 'modal' }, [
    // The ✕ abandons the reveal without the refresh, exactly like closing the
    // dialog by hand: the secret is already stored server-side either way.
    modalHead(title, () => backdrop.remove()),
    modalBody([
      el('p', { class: 'muted', text: '这是明文唯一一次出现，关闭后无法再次获取。' }),
      box,
    ]),
    modalActions([copy, done]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  done.addEventListener('click', async () => { backdrop.remove(); await after(); });
  document.getElementById('modal-root').append(backdrop);
  box.select();
}