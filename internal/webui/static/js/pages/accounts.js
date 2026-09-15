import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, statusBadge, confirmDialog } from '../ui.js';
import { initCurrency, money, ledgerCurrency } from '../money.js';

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  await initCurrency();
  const create = el('button', { class: 'btn btn-primary', text: '新建账户', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);

  // 组织筛选：节点列表来自组织架构接口，选中后按节点过滤（默认包含子节点）。
  const orgFilter = el('select', { class: 'org-filter' }, [el('option', { value: '', text: '全部账户' })]);
  const orgDescendants = el('input', { type: 'checkbox', checked: true });
  // .filter-field / .filter-check are the toolbar's inline label+control pair. `.field` is the
  // wrong tool here: it is a block layout whose text span is display:block, so the checkbox and
  // its label end up on separate lines.
  const filterBox = el('div', { class: 'toolbar org-filter-bar' }, [
    el('label', { class: 'filter-field' }, [el('span', { text: '按组织' }), orgFilter]),
    el('label', { class: 'filter-check' }, [orgDescendants, el('span', { text: '含子节点' })]),
  ]);
  const query = { org_node_id: '', include_descendants: 'true' };

  // orgQuery only adds the organization parameters once a node is chosen. The unfiltered list
  // therefore sends exactly the request it always did, which keeps this page's URLs — and the
  // behaviour of a deployment without an organization tree — unchanged.
  function orgQuery() {
    return query.org_node_id ? { ...query } : {};
  }

  const view = pagedTable({
    columns: [
      { key: 'name', label: '名称' },
      { key: 'billing_mode', label: '计费模式' },
      { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
      { key: 'org_nodes', label: '所属组织', render: (row) => orgCell(row) },
      { key: 'tags', label: '标签', render: (row) => (row.tags || []).join(', ') || '—' },
      { key: 'balance_micros', label: '余额', render: (row) => money(row.balance_micros) },
      { key: 'credit_limit_micros', label: '授信上限', render: (row) => money(row.credit_limit_micros) },
      { key: 'low_balance_threshold_micros', label: '低额阈值', render: (row) => money(row.low_balance_threshold_micros) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, () => view.refresh()) }),
    ],
    load: ({ limit, offset }) => api.get('/accounts', { limit, offset, ...orgQuery() }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(
    filterBox,
    card('账户', view.node, [
      el('span', { class: 'muted', text: '余额只能通过账本变动；标签会被账号下所有 API Key 继承，组织节点的标签同样被整棵子树继承' })]));

  function applyOrgFilter() {
    query.org_node_id = orgFilter.value;
    query.include_descendants = orgDescendants.checked ? 'true' : 'false';
    view.reset();
  }
  orgFilter.addEventListener('change', applyOrgFilter);
  orgDescendants.addEventListener('change', applyOrgFilter);
  refresh.addEventListener('click', () => view.refresh());

  // The node list fills the filter. A deployment without an organization port (or a fixture
  // that does not answer this endpoint) simply keeps "全部账户" instead of failing the page.
  async function loadOrgOptions() {
    try {
      const payload = await api.get('/org/nodes', { limit: 1000 });
      const nodes = payload.data || [];
      if (!nodes.length) return;
      orgFilter.replaceChildren(
        el('option', { value: '', text: '全部账户' }),
        ...nodes.map((node) => el('option', {
          value: String(node.id),
          // Indentation is what makes the hierarchy readable in a flat select.
          text: '　'.repeat(node.depth) + (node.path || node.name),
        })));
    } catch (err) {
      // Keep the default option; the filter stays usable as "all accounts".
    }
  }

  create.addEventListener('click', async () => {
    const result = await modal({
      title: '新建账户', submitLabel: '创建',
      fields: [
        { name: 'name', label: '名称', required: true, hint: '支持邮箱、中文和其他 Unicode 字符；去除首尾空白后最多 64 个字符' },
        { name: 'billing_mode', label: '计费模式', type: 'select', options: ['prepaid', 'postpaid'] },
        { name: 'credit_limit_micros', label: '授信上限（微' + ledgerCurrency() + '）', type: 'number' },
        { name: 'low_balance_threshold_micros', label: '低额告警阈值（微' + ledgerCurrency() + '）', type: 'number' },
        { name: 'tags', label: '账号标签（逗号分隔）', hint: '所有 API Key 自动继承；留空表示不绑定标签' },
        { name: 'org_node_ids', label: '组织节点 id（逗号分隔）', hint: '账号可同时属于多个节点；节点上的标签会被该账号下所有 Key 继承' },
        { name: 'note', label: '备注' },
      ],
      onSubmit: (values) => api.post('/accounts', {
        ...values,
        tags: splitTags(values.tags),
        org_node_ids: splitIDs(values.org_node_ids),
      }),
    });
    if (result) { toast('账户已创建', 'ok'); await view.refresh(); }
  });

  await Promise.all([loadOrgOptions(), view.refresh()]);
}

// orgCell renders the account's organizations as their label paths, so an operator reads
// 总部/研发部 instead of a pair of ids.
function orgCell(row) {
  const refs = row.org_nodes || [];
  if (!refs.length) return el('span', { class: 'muted', text: '—' });
  return el('span', {}, refs.map((ref) => el('span', { class: 'badge', text: ref.path || ref.name })));
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
      { name: 'org_node_ids', label: '组织节点 id（逗号分隔）',
        hint: '整表替换：留空即移出全部组织，从节点继承来的标签授权随即失效',
        value: (row.org_node_ids || []).join(', ') },
      { name: 'note', label: '备注', value: row.note },
    ],
    onSubmit: (values) => api.patch('/accounts/' + row.id, {
      ...values,
      tags: splitTags(values.tags),
      org_node_ids: splitIDs(values.org_node_ids),
    }),
  });
  if (result) { toast('已更新', 'ok'); await reload(); }
}

function splitTags(value) {
  return (value || '').split(',').map((tag) => tag.trim()).filter(Boolean);
}

// splitIDs parses the comma-separated node ids into an array. It is always an array (never
// undefined), because an empty list is a meaningful instruction — "this account belongs to no
// organization" — and sending nothing would instead mean "leave the memberships alone".
function splitIDs(value) {
  return (value || '').split(',')
    .map((id) => id.trim())
    .filter(Boolean)
    .map(Number)
    .filter((id) => Number.isInteger(id) && id > 0);
}

export { confirmDialog };
