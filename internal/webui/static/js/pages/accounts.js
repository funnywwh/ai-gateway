import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, statusBadge, confirmDialog, formatTime } from '../ui.js';
import { initCurrency, money, ledgerCurrency } from '../money.js';
// 账号的创建/编辑（含所属组织勾选树）与组织页共用一份实现，见 account_actions.js。
import { createAccount, editAccount } from './account_actions.js';

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
      { key: 'dsh_enabled', label: 'DSH', render: (row) => dshCell(row) },
      { key: 'feishu', label: '飞书', render: (row) => feishuCell(row) },
      { key: 'key_count', label: 'Key', render: (row) => keyCountCell(row) },
      { key: 'org_nodes', label: '所属组织', render: (row) => orgCell(row) },
      { key: 'tags', label: '标签', render: (row) => (row.tags || []).join(', ') || '—' },
      { key: 'balance_micros', label: '余额', render: (row) => money(row.balance_micros) },
      { key: 'credit_limit_micros', label: '授信上限', render: (row) => money(row.credit_limit_micros) },
      { key: 'low_balance_threshold_micros', label: '低额阈值', render: (row) => money(row.low_balance_threshold_micros) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, () => view.refresh()) }),
      el('button', {
        class: 'btn', text: row.dsh_enabled ? '停用 DSH' : '启用 DSH',
        onclick: () => toggleDSH(row, () => view.refresh()),
      }),
    ],
    load: ({ limit, offset }) => api.get('/accounts', { limit, offset, ...orgQuery() }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(
    filterBox,
    card('账户', view.node, [
      el('span', { class: 'muted', text: '余额只能通过账本变动；标签会被账号下所有 API Key 继承，组织节点的标签同样被整棵子树继承' }),
      el('span', { class: 'muted', text: '逐人操作（Key 列表、绑定飞书、启用/停用 DSH）在组织架构页展开账号即可；本页是跨组织的总览' })]));

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
    const created = await createAccount({ title: '新建账户', submitLabel: '创建' });
    if (!created) return;
    toast('账户已创建', 'ok');
    await view.refresh();
  });

  await Promise.all([loadOrgOptions(), view.refresh()]);
}

// dshCell renders the account's dsh gateway opt-in (M52) with the distinction M72 introduced.
// There are three states an operator has to be able to tell apart, and the row carries all three:
//
//   * 已启用          — dsh_enabled, a tenant exists;
//   * 已停用（管理员） — an administrator pressed 停用; dshgw.auto_enable does NOT undo it;
//   * 未启用          — never enabled, which is the state auto_enable turns into "usable at
//                       first login" (the server decides; this page only reports).
function dshCell(row) {
  if (row.dsh_enabled) return el('span', { class: 'badge', text: '已启用 · ' + (row.dsh_tenant || '?') });
  if (row.dsh_disabled_at) {
    return el('span', {
      class: 'badge warn', title: '管理员于 ' + formatTime(row.dsh_disabled_at) + ' 显式停用；自动启用不会撤销它',
      text: '已停用（管理员）' + (row.dsh_tenant ? ' · ' + row.dsh_tenant : ''),
    });
  }
  if (row.dsh_effective) {
    return el('span', { class: 'badge', title: '本部署开启了自动启用：该账号首次登录时会自动创建租户', text: '未启用（登录即可用）' });
  }
  return el('span', { class: 'muted', text: row.dsh_tenant ? '未启用 · ' + row.dsh_tenant : '未启用' });
}

// feishuCell shows the account's Feishu identity (M72): it is the portal login identity, so an
// operator looking at an account needs to see whether anybody can sign in as it.
function feishuCell(row) {
  const feishu = row.feishu || {};
  if (!feishu.bound) return el('span', { class: 'muted', text: '未绑定' });
  const title = [feishu.open_id, feishu.bound_by ? '由 ' + feishu.bound_by + ' 绑定' : '',
    feishu.bound_at ? '绑定于 ' + formatTime(feishu.bound_at) : '',
    '在组织架构页展开该账号可以改绑'].filter(Boolean).join(' · ');
  return el('span', { class: 'badge', text: feishu.name || feishu.open_id || '已绑定', title });
}

// keyCountCell answers "how many keys can this account log in with" — the number that decides
// whether the portal will ask the person to pick one (M72).
function keyCountCell(row) {
  const total = row.key_count || 0;
  const active = row.active_key_count || 0;
  if (!total) return el('span', { class: 'muted', text: '0' });
  return el('span', {
    class: 'badge', title: active > 1 ? '门户登录会先让这个人选一把 Key（只影响登录归属与审计）' : '',
    text: active + ' / ' + total,
  });
}

// toggleDSH drives the account-level dsh gateway lifecycle (M52-rev2).
// Enabling provisions everything through the local dshgw channel: it mints a dedicated
// worker key, creates (or starts and re-keys) the tenant and records the mapping, so
// every key of the account — present and future — can log into that tenant. Disabling
// stops the worker and revokes the worker keys; workspace and dsh data are kept.
async function toggleDSH(row, reload) {
  const enabling = !row.dsh_enabled;
  if (enabling) {
    // 已有映射优先，否则用服务端下发的规则名（M74）。规则（`dsh-<账号拼音>-<账号ID>`）只有服务端那一份
    // 实现：页面预填它给出的值，而不是自己再拼一遍。
    const suggested = row.dsh_tenant || row.dsh_tenant_suggested || '';
    const result = await modal({
      title: '启用 DSH — ' + row.name,
      submitLabel: '启用',
      fields: [
        { name: 'tenant', label: 'dsh 租户名', value: suggested,
          hint: '小写字母/数字/连字符；留空则沿用既有映射，或按账号名自动生成（例：陈景峰 / 10 → ' +
            'dsh-chenjingfeng-10）。将自动创建租户与 worker，账号下所有 Key（含新建）都能登录该租户；' +
            '已存在的租户名不会被改动' },
      ],
      onSubmit: (values) => api.post('/accounts/' + row.id + '/dsh', {
        enabled: true, ...(values.tenant ? { tenant: values.tenant } : {}),
      }),
    });
    if (result) { toast('已启用 DSH（租户 ' + (result.tenant || suggested) + '）', 'ok'); await reload(); }
    return;
  }
  const ok = await confirmDialog('停用 DSH',
    '停用账户 ' + row.name + ' 的 dsh？将停止其 worker 并吊销 worker 专用 Key；' +
    '新登录被拒绝，既有会话按网关 dsh_enforce 档位失效。工作区与 dsh 数据保留，重新启用即恢复。\n' +
    '这是「显式停用」：即使本部署开启了「所有激活账号默认可用 DSH」，也不会在下次登录时自动重新启用它。');
  if (!ok) return;
  try {
    await api.post('/accounts/' + row.id + '/dsh', { enabled: false });
    toast('已停用 DSH', 'ok');
    await reload();
  } catch (err) {
    toast(api.errorMessage(err), 'error');
  }
}

// The dialog's suggestion used to be computed here (slugFromAccount). Since M74 the server derives it
// and ships it as dsh_tenant_suggested, so the rule has exactly one implementation.

// orgCell renders the account's organizations as their label paths, so an operator reads
// 总部/研发部 instead of a pair of ids.
function orgCell(row) {
  const refs = row.org_nodes || [];
  if (!refs.length) return el('span', { class: 'muted', text: '—' });
  return el('span', {}, refs.map((ref) => el('span', { class: 'badge', text: ref.path || ref.name })));
}

async function edit(row, reload) {
  const updated = await editAccount(row);
  if (!updated) return;
  toast('已更新', 'ok');
  await reload();
}

function splitTags(value) {
  return (value || '').split(',').map((tag) => tag.trim()).filter(Boolean);
}

