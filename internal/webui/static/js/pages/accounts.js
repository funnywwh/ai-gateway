import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, statusBadge, confirmDialog } from '../ui.js';
import { initCurrency, money, ledgerCurrency } from '../money.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  await initCurrency();
  const create = el('button', { class: 'btn btn-primary', text: '新建账户', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);

  const view = pagedTable({
    columns: [
      { key: 'name', label: '名称' },
      { key: 'billing_mode', label: '计费模式' },
      { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
      { key: 'tags', label: '标签', render: (row) => (row.tags || []).join(', ') || '—' },
      { key: 'balance_micros', label: '余额', render: (row) => money(row.balance_micros) },
      { key: 'credit_limit_micros', label: '授信上限', render: (row) => money(row.credit_limit_micros) },
      { key: 'low_balance_threshold_micros', label: '低额阈值', render: (row) => money(row.low_balance_threshold_micros) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, () => view.refresh()) }),
    ],
    load: ({ limit, offset }) => api.get('/accounts', { limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('账户', view.node, [
    el('span', { class: 'muted', text: '余额只能通过账本变动；标签会被账号下所有 API Key 继承，并与 Key 标签取并集' })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', async () => {
    const result = await modal({
      title: '新建账户', submitLabel: '创建',
      fields: [
        { name: 'name', label: '名称', required: true, hint: '支持邮箱、中文和其他 Unicode 字符；去除首尾空白后最多 64 个字符' },
        { name: 'billing_mode', label: '计费模式', type: 'select', options: ['prepaid', 'postpaid'] },
        { name: 'credit_limit_micros', label: '授信上限（微' + ledgerCurrency() + '）', type: 'number' },
        { name: 'low_balance_threshold_micros', label: '低额告警阈值（微' + ledgerCurrency() + '）', type: 'number' },
        { name: 'tags', label: '账号标签（逗号分隔）', hint: '所有 API Key 自动继承；留空表示不绑定标签' },
        { name: 'note', label: '备注' },
      ],
      onSubmit: (values) => api.post('/accounts', { ...values, tags: splitTags(values.tags) }),
    });
    if (result) { toast('账户已创建', 'ok'); await view.refresh(); }
  });
  await view.refresh();
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
      { name: 'tags', label: '账号标签（逗号分隔）', hint: '空输入会清空账号标签；所有 Key 会动态继承', value: (row.tags || []).join(', ') },
      { name: 'note', label: '备注', value: row.note },
    ],
    onSubmit: (values) => api.patch('/accounts/' + row.id, { ...values, tags: splitTags(values.tags) }),
  });
  if (result) { toast('已更新', 'ok'); await reload(); }
}

function splitTags(value) {
  return (value || '').split(',').map((tag) => tag.trim()).filter(Boolean);
}

export { confirmDialog };
