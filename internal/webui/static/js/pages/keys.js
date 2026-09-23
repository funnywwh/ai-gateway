import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, statusBadge, formatTime, confirmDialog } from '../ui.js';
// Key 的创建/编辑/启停与「一次性明文」对话框是两个页面共用的实现（M72 起组织页也用）。
import { createKeyForAccount, editKey, toggleKey, showSecret } from './key_actions.js';

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
      { key: 'tags', label: 'Key 标签', render: (row) => (row.tags || []).join(', ') || '—' },
      { key: 'account_tags', label: '账号标签', render: (row) => (row.account_tags || []).join(', ') || '—' },
      { key: 'effective_tags', label: '生效标签', render: (row) => (row.effective_tags || []).join(', ') || '—' },
      { key: 'record_input_mode', label: '输入录制' },
      { key: 'record_output_text', label: '输出文本', render: (row) => (row.record_output_text ? '已开启' : '关闭') },
      { key: 'record_reasoning', label: '思考文本', render: (row) => (row.record_reasoning ? '已开启' : '关闭') },
      { key: 'policy', label: '配额', render: (row) => el('code', { text: JSON.stringify(row.policy || {}) }) },
      // The Feishu identity that used to be bound to this key (M60). Since M72 the identity
      // lives on the ACCOUNT and is what lets its owner sign in to the DSH portal; this column
      // is read-only and exists to show — and clean up — pre-migration leftovers.
      { key: 'feishu', label: '飞书（旧）', render: (row) => feishuCell(row) },
      { key: 'last_used_at', label: '最近使用', render: (row) => formatTime(row.last_used_at) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => editKey(row, () => view.refresh()) }),
      el('button', { class: 'btn', text: row.status === 'active' ? '停用' : '启用', onclick: () => toggle(row, () => view.refresh()) }),
      // Binding moved to the account (M72): the organization page's person list is where a
      // Feishu person is attached, so this page only offers to clear a leftover key binding.
      ...(row.feishu && row.feishu.bound
        ? [el('button', { class: 'btn btn-danger', text: '解绑飞书', onclick: () => unbindFeishu(row, () => view.refresh()) })]
        : []),
    ],
    // 账户名来自上面那份完整列表；Key 列表本身由服务端分页。
    load: async ({ limit, offset }) => {
      const [payload, accountsPayload] = await Promise.all([api.get('/keys', { limit, offset }), accountsPromise]);
      const byId = new Map(((accountsPayload.data) || []).map((a) => [a.id, a.name]));
      return { ...payload, data: (payload.data || []).map((key) => ({ ...key, account: byId.get(key.account_id) || ('#' + key.account_id) })) };
    },
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('API Keys', view.node, [
    el('span', { class: 'muted', text: '明文只在创建时显示一次；默认只记录用户输入（长消息按上限截断，非文本附件与工具内容不落库），思考与最终输出需单独勾选' }),
    el('span', { class: 'muted', text: '飞书身份已改为绑定在账号上（组织架构页展开账号 → 「绑定飞书」）；这里的「飞书（旧）」列只用于查看与清理升级前的 Key 级绑定' })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', async () => {
    const accounts = (await accountsPromise).data || [];
    if (!accounts.length) { toast('请先创建一个账户', 'error'); return; }
    // 账户下拉框要一次拿全：显式请求上限 1000（配置类列表的服务端上限）。
    const chosen = await modal({
      title: '新建 API Key',
      submitLabel: '下一步',
      fields: [{ name: 'account_id', label: '账户', type: 'select', options: accounts.map((a) => ({ value: a.id, label: a.name })) }],
      onSubmit: (values) => values,
    });
    if (!chosen) return;
    const account = accounts.find((a) => String(a.id) === String(chosen.account_id)) || accounts[0];
    const created = await createKeyForAccount(account);
    if (created) await view.refresh();
  });

  await view.refresh();
}

// feishuCell renders a leftover key-level Feishu identity (M60). It is read-only: from M72 the
// portal resolves an identity through the account, so showing it here is about telling an
// operator that this row predates the migration (or was left behind by a conflict).
function feishuCell(row) {
  const feishu = row.feishu || {};
  if (!feishu.bound) return el('span', { class: 'muted', text: '—' });
  const label = feishu.name || feishu.open_id || '已绑定';
  const title = [feishu.open_id, feishu.bound_by ? '由 ' + feishu.bound_by + ' 绑定' : '',
    feishu.bound_at ? '绑定于 ' + formatTime(feishu.bound_at) : '',
    'Key 级绑定已废弃：登录按账号级身份判定'].filter(Boolean).join(' · ');
  return el('span', { class: 'badge', text: label, title });
}

async function unbindFeishu(row, reload) {
  const feishu = row.feishu || {};
  const who = feishu.name || feishu.open_id || '该飞书账号';
  const ok = await confirmDialog('解绑飞书',
    '确认解除 ' + row.name + ' 与 ' + who + ' 的绑定吗？解绑后该账号将无法用飞书登录 DSH 门户；' +
    'Key 本身不受影响，仍可用于调用模型。');
  if (!ok) return;
  try {
    const result = await api.del('/keys/' + row.id + '/feishu');
    toast(result && result.unbound ? '已解绑' : '该 Key 本来就没有绑定', 'ok');
  } catch (err) {
    toast(api.errorMessage(err), 'error');
  }
  await reload();
}
