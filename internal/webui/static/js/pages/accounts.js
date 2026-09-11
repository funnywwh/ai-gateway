import { api } from '../api.js';
import { el, card, table, modal, toast, statusBadge, confirmDialog } from '../ui.js';
import { initCurrency, money, ledgerCurrency } from '../money.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  await initCurrency();
  const create = el('button', { class: 'btn btn-primary', text: '新建账户', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);
  let view;

  async function load() {
    const payload = await api.get('/accounts');
    const rows = payload.data || [];
    if (!view) {
      view = table({
        columns: [
          { key: 'name', label: '名称' },
          { key: 'billing_mode', label: '计费模式' },
          { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
          { key: 'balance_micros', label: '余额', render: (row) => money(row.balance_micros) },
          { key: 'credit_limit_micros', label: '授信上限', render: (row) => money(row.credit_limit_micros) },
          { key: 'low_balance_threshold_micros', label: '低额阈值', render: (row) => money(row.low_balance_threshold_micros) },
        ],
        rows,
        rowActions: (row) => readonly ? [] : [
          el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, load) }),
        ],
      });
      page.append(card('账户', view.node, [
        el('span', { class: 'muted', text: '余额只能通过账本变动；这里只调整计费属性与状态' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  create.addEventListener('click', async () => {
    const result = await modal({
      title: '新建账户', submitLabel: '创建',
      fields: [
        { name: 'name', label: '名称', required: true },
        { name: 'billing_mode', label: '计费模式', type: 'select', options: ['prepaid', 'postpaid'] },
        { name: 'credit_limit_micros', label: '授信上限（微' + ledgerCurrency() + '）', type: 'number' },
        { name: 'low_balance_threshold_micros', label: '低额告警阈值（微' + ledgerCurrency() + '）', type: 'number' },
        { name: 'note', label: '备注' },
      ],
      onSubmit: (values) => api.post('/accounts', values),
    });
    if (result) { toast('账户已创建', 'ok'); await load(); }
  });
  await load();
}

async function edit(row, reload) {
  const result = await modal({
    title: '编辑账户 ' + row.name,
    fields: [
      { name: 'status', label: '状态', type: 'select', options: ['active', 'suspended', 'closed'], value: row.status },
      { name: 'billing_mode', label: '计费模式', type: 'select', options: ['prepaid', 'postpaid'], value: row.billing_mode },
      { name: 'credit_limit_micros', label: '授信上限（微' + ledgerCurrency() + '）', type: 'number', value: row.credit_limit_micros },
      { name: 'low_balance_threshold_micros', label: '低额阈值（微美元）', type: 'number', value: row.low_balance_threshold_micros },
      { name: 'overdraft_limit_micros', label: '在途透支上限（微' + ledgerCurrency() + '）', type: 'number', value: row.overdraft_limit_micros },
      { name: 'note', label: '备注', value: row.note },
    ],
    onSubmit: (values) => api.patch('/accounts/' + row.id, values),
  });
  if (result) { toast('已更新', 'ok'); await reload(); }
}

export { confirmDialog };