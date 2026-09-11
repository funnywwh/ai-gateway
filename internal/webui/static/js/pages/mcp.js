import { api } from '../api.js';
import { el, card, table, modal, toast, statusBadge, formatTime, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '签发令牌', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);
  const accountsPromise = api.get('/accounts');
  let view;

  async function load() {
    const payload = await api.get('/mcp-tokens');
    const accounts = (await accountsPromise).data || [];
    const byId = new Map(accounts.map((a) => [a.id, a.name]));
    const rows = (payload.data || []).map((token) => ({ ...token, account: byId.get(token.account_id) || ('#' + token.account_id) }));
    if (!view) {
      view = table({
        columns: [
          { key: 'name', label: '名称' },
          { key: 'account', label: '账户' },
          { key: 'token_prefix', label: '前缀', render: (row) => el('code', { text: row.token_prefix + '…' }) },
          { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
          { key: 'last_used_at', label: '最近使用', render: (row) => formatTime(row.last_used_at) },
          { key: 'expires_at', label: '过期', render: (row) => formatTime(row.expires_at) },
        ],
        rows,
        rowActions: (row) => readonly || row.status === 'revoked' ? [] : [
          el('button', { class: 'btn btn-danger', text: '吊销', onclick: () => revoke(row, load) }),
        ],
      });
      page.append(card('MCP 令牌', view.node, [
        el('span', { class: 'muted', text: 'MCP 端点只读，且按账户强作用域；API Key 不能用于 /mcp' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  create.addEventListener('click', async () => {
    const accounts = (await accountsPromise).data || [];
    if (!accounts.length) { toast('请先创建一个账户', 'error'); return; }
    const result = await modal({
      title: '签发 MCP 令牌', submitLabel: '签发',
      fields: [
        { name: 'name', label: '名称', required: true },
        { name: 'account_id', label: '账户', type: 'select', options: accounts.map((a) => ({ value: a.id, label: a.name })) },
        { name: 'expires_at', label: '过期时间（RFC3339，可留空）', placeholder: '2026-12-31T00:00:00Z' },
        { name: 'note', label: '备注' },
      ],
      onSubmit: (values) => api.post('/mcp-tokens', {
        name: values.name, account_id: Number(values.account_id),
        expires_at: values.expires_at || undefined, note: values.note,
      }),
    });
    if (!result) return;
    showToken(result.token, load);
  });
  await load();
}

async function revoke(row, reload) {
  const ok = await confirmDialog('吊销令牌', '确认吊销 ' + row.name + ' 吗？使用它的客户端会立即收到 401。');
  if (!ok) return;
  await api.del('/mcp-tokens/' + row.id);
  toast('已吊销', 'ok');
  await reload();
}

function showToken(token, after) {
  const box = el('input', { value: token, readonly: true });
  const done = el('button', { class: 'btn btn-primary', text: '我已保存' });
  const dialog = el('div', { class: 'modal' }, [
    modalHead('MCP 令牌已签发', () => backdrop.remove()),
    modalBody([
      el('p', { class: 'muted', text: '明文只显示一次。客户端用 Authorization: Bearer <token> 调用 POST /mcp。' }),
      box,
    ]),
    modalActions([done]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  done.addEventListener('click', async () => { backdrop.remove(); await after(); });
  document.getElementById('modal-root').append(backdrop);
  box.select();
}