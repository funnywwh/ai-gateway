import { api } from '../api.js';
import { apiRoot } from '../base.js';
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

export async function render({ page, actions, session, route, navigate }) {
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
      // The Feishu identity bound to this key (M60): it is what lets its owner sign in to
      // the DSH portal without pasting a key. Ownership is proven through Feishu itself, so
      // this column is the only place an operator sees who is behind a key.
      { key: 'feishu', label: '飞书', render: (row) => feishuCell(row) },
      { key: 'last_used_at', label: '最近使用', render: (row) => formatTime(row.last_used_at) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => editKey(row, () => view.refresh()) }),
      el('button', { class: 'btn', text: row.status === 'active' ? '停用' : '启用', onclick: () => toggle(row, () => view.refresh()) }),
      el('button', { class: 'btn', text: '绑定飞书', onclick: () => bindFeishu(row) }),
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
  page.append(card('API Keys', view.node, [el('span', { class: 'muted', text: '明文只在创建时显示一次；默认只记录用户输入，思考与最终输出需单独勾选' })]));

  // The binding flow leaves the console for Feishu and comes back here with a result code
  // in the hash query. Reporting it once and dropping the parameter keeps a page refresh
  // from repeating a message about something that happened minutes ago.
  reportFeishuResult(route, navigate, () => view.refresh());

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
      { name: 'tags', label: 'Key 标签（逗号分隔）', hint: '与账号标签取并集；空输入清空 Key 自有标签', value: (row.tags || []).join(', ') },
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
      tags: splitTags(values.tags),
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

// feishuCell renders the key's Feishu identity (M60). The open id is shown in full inside
// the title so an operator comparing two keys can tell them apart, while the cell stays
// readable: the name is what a person recognises.
function feishuCell(row) {
  const feishu = row.feishu || {};
  if (!feishu.bound) return el('span', { class: 'muted', text: '未绑定' });
  const label = feishu.name || feishu.open_id || '已绑定';
  const title = [feishu.open_id, feishu.bound_by ? '由 ' + feishu.bound_by + ' 绑定' : '',
    feishu.bound_at ? '绑定于 ' + formatTime(feishu.bound_at) : ''].filter(Boolean).join(' · ');
  return el('span', { class: 'badge', text: label, title });
}

// bindFeishu leaves the console for the management endpoint, which redirects to Feishu. It
// is a full navigation rather than fetch: the consent screen belongs to Feishu, and the
// flow has to end as a top-level page for its state and cookies to be the browser's.
function bindFeishu(row) {
  window.location.assign(apiRoot() + '/keys/' + row.id + '/feishu/bind');
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

// feishuResults maps the callback's result code onto what the operator needs to know. The
// codes are the server's, not free text, so an unexpected one is reported as-is instead of
// being silently swallowed.
const FEISHU_RESULTS = {
  bound: { level: 'ok', text: '已绑定飞书账号' },
  replaced: { level: 'ok', text: '已改绑到新的飞书账号（原绑定已解除）' },
  cancelled: { level: 'error', text: '已取消飞书授权，未做任何改动' },
  conflict: { level: 'error', text: '该飞书账号已绑定到另一把 Key；请先在那把 Key 上解绑' },
  rejected: { level: 'error', text: '操作者已不是管理员，绑定未生效' },
  expired: { level: 'error', text: '授权已过期或被重复使用，请重新绑定' },
  invalid: { level: 'error', text: '授权请求无法校验，请重新绑定' },
  rate_limited: { level: 'error', text: '操作过于频繁，请稍后再试' },
  no_app_permission: { level: 'error', text: '你在飞书侧没有该应用的使用权限，请联系飞书管理员' },
  app_error: { level: 'error', text: '飞书应用凭据或可用范围有问题，请检查 aigw 配置与飞书后台' },
  error: { level: 'error', text: '绑定失败，请重试；若持续失败请查看 aigw 日志' },
};

// feishuDSHMessage explains what happened to the account's DSH state during a binding. The
// binding itself is about identity; this is the part that decides whether the person can sign
// in yet, so it has to be said out loud rather than left to the account page.
function feishuDSHMessage(params) {
  const state = params.get('dsh');
  if (!state) return null;
  const tenant = params.get('tenant') || '';
  const reason = params.get('dsh_reason') || '';
  switch (state) {
    case 'enabled':
      return { level: 'ok', text: '已自动启用该账号的 DSH（租户 ' + (tenant || '?') + '），现在即可用飞书登录' };
    case 'already':
      return { level: 'ok', text: '该账号此前已启用 DSH' + (tenant ? '（租户 ' + tenant + '）' : '') };
    case 'declined':
      return { level: 'error', text: '该账号曾被显式停用 DSH，因此未自动启用；如需登录请在账户页手动启用' };
    case 'failed':
      return { level: 'error', text: '自动启用 DSH 失败' + (reason ? '：' + reason : '') + '；绑定已保存，请在账户页重试启用' };
    case 'off':
      return null; // the deployment turned the automatic step off: nothing to report
    default:
      return { level: 'error', text: '未知的 DSH 启用结果：' + state };
  }
}

function reportFeishuResult(route, navigate, reload) {
  const params = route && route.params ? route.params : null;
  const result = params ? params.get('feishu') : null;
  if (!result) return;
  const known = FEISHU_RESULTS[result];
  toast(known ? known.text : '飞书操作返回了未知结果：' + result, known ? known.level : 'error');
  const dsh = params ? feishuDSHMessage(params) : null;
  if (dsh) toast(dsh.text, dsh.level);
  // The parameter is dropped immediately: this message describes something that already
  // happened, and it must not reappear on every refresh or back navigation.
  if (typeof navigate === 'function') navigate('/keys');
  reload();
}

function splitTags(value) {
  return (value || '').split(',').map((tag) => tag.trim()).filter(Boolean);
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
