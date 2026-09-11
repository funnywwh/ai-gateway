import { api } from '../api.js';
import { el, card, table, modal, toast, statusBadge, formatTime, confirmDialog, modalHead } from '../ui.js';

const INPUT_MODES = [
  { value: 'inherit', label: '继承全局默认（输入 full）' },
  { value: 'full', label: 'full：完整记录输入' },
  { value: 'meta', label: 'meta：只记元数据' },
  { value: 'off', label: 'off：不记录输入' },
];

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  const create = el('button', { class: 'btn btn-primary', text: '新建 Key', disabled: readonly });
  actions.append(refresh, create);

  const accountsPromise = api.get('/accounts');
  let view;
  async function load() {
    const payload = await api.get('/keys');
    const accounts = (await accountsPromise).data || [];
    const byId = new Map(accounts.map((a) => [a.id, a.name]));
    const rows = (payload.data || []).map((key) => ({ ...key, account: byId.get(key.account_id) || ('#' + key.account_id) }));
    if (!view) {
      view = table({
        columns: [
          { key: 'name', label: '名称' },
          { key: 'account', label: '账户' },
          { key: 'key_prefix', label: '前缀', render: (row) => el('code', { text: row.key_prefix + '…' }) },
          { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
          { key: 'tags', label: '标签', render: (row) => (row.tags || []).join(', ') || '—' },
          { key: 'record_input_mode', label: '输入录制' },
          { key: 'record_output_text', label: '输出文本', render: (row) => (row.record_output_text ? '已开启' : '关闭') },
          { key: 'record_reasoning', label: '思考文本', render: (row) => (row.record_reasoning ? '已开启' : '关闭') },
          { key: 'last_used_at', label: '最近使用', render: (row) => formatTime(row.last_used_at) },
        ],
        rows,
        rowActions: (row) => readonly ? [] : [
          el('button', { class: 'btn', text: '编辑', onclick: () => editKey(row, load) }),
          el('button', { class: 'btn', text: row.status === 'active' ? '停用' : '启用', onclick: () => toggle(row, load) }),
        ],
      });
      page.append(card('API Keys', view.node, [el('span', { class: 'muted', text: '明文只在创建时显示一次；输入默认记录，思考与最终输出需单独勾选' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
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
    showSecret('API Key 已创建', result.key, load);
  });

  await load();
}

async function editKey(row, reload) {
  const result = await modal({
    title: '编辑 ' + row.name,
    fields: [
      { name: 'record_input_mode', label: '输入录制模式', type: 'select', options: INPUT_MODES, value: row.record_input_mode },
      { name: 'record_output_text', label: '记录最终输出文本', type: 'checkbox', value: row.record_output_text },
      { name: 'record_reasoning', label: '记录思考文本', type: 'checkbox', value: row.record_reasoning },
      { name: 'status', label: '状态', type: 'select', options: ['active', 'suspended', 'revoked'], value: row.status },
    ],
    onSubmit: (values) => api.patch('/keys/' + row.id, {
      record_input_mode: values.record_input_mode,
      record_output_text: !!values.record_output_text,
      record_reasoning: !!values.record_reasoning,
      status: values.status,
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
    el('p', { class: 'muted', text: '这是明文唯一一次出现，关闭后无法再次获取。' }),
    box,
    el('div', { class: 'modal-actions' }, [copy, done]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  done.addEventListener('click', async () => { backdrop.remove(); await after(); });
  document.getElementById('modal-root').append(backdrop);
  box.select();
}