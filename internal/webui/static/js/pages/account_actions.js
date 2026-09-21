// 账号（= 人员/成员）的创建与编辑：账户页与组织架构页共用这一份实现。
//
// 抽出来的理由和 key_actions.js 一样具体：账号的字段（计费模式、授信上限、低额阈值、标签、所属组织）
// 决定这个账号能不能用、能花多少，任何一处入口抄漏一个字段，都会变成"从这边建的账号和从那边建的
// 不一样"。所以两个页面调的是同一段代码，差别只有标题与「所属组织」的初值。
import { api } from '../api.js';
import { el, modal } from '../ui.js';
import { initCurrency, ledgerCurrency } from '../money.js';
import { openOrgPicker } from './org_assign.js';

// orgField 是表单里的「所属组织」：它不是一个输入框，而是一行当前归属 + 一个打开勾选树的按钮。
//
// refs 里存节点引用（id/name/path），提交时只取 id：勾选树返回的也是引用，所以"刚勾完"和"原来就有"
// 两种情况下这一行都能把路径显示出来。
function orgField({ title, refs }) {
  const state = { refs: (refs || []).slice() };
  const view = el('div', { class: 'org-picked' });

  function paint() {
    const list = state.refs.length
      ? state.refs.map((ref) => el('span', { class: 'badge', text: ref.path || ref.name }))
      : [el('span', { class: 'muted', text: '不属于任何组织' })];
    view.replaceChildren(
      el('span', { class: 'org-picked-list' }, list),
      el('button', {
        class: 'btn', type: 'button', text: '分配组织…',
        onclick: async () => {
          const picked = await openOrgPicker({ title: title || '分配组织', nodeIds: state.refs.map((ref) => ref.id) });
          if (!picked) return;
          state.refs = picked.refs;
          paint();
        },
      }));
  }
  paint();

  return {
    name: 'org_node_ids',
    label: '所属组织',
    // 自定义字段：render 给出控件，get 给出要提交的值（见 ui.js 的 modal()）。
    render: () => view,
    get: () => state.refs.map((ref) => ref.id),
  };
}

async function accountFields({ tags = [], orgRefs = [], editing }) {
  // 币种表决定金额字段的标注（微美元 / 微人民币…）：先在弹窗里读一次，读不到就退回默认币种标注，
  // 而不是让弹窗打不开。
  try { await initCurrency(); } catch (err) { /* 标注退回默认币种 */ }
  const money = '（微' + ledgerCurrency() + '）';
  const fields = [];
  if (editing) {
    fields.push({ name: 'status', label: '状态', type: 'select', options: ['active', 'suspended', 'closed'], value: editing.status });
  } else {
    fields.push({ name: 'name', label: '名称', required: true, hint: '支持邮箱、中文和其他 Unicode 字符；去除首尾空白后最多 64 个字符' });
  }
  fields.push(
    { name: 'billing_mode', label: '计费模式', type: 'select', options: ['prepaid', 'postpaid'], value: editing ? editing.billing_mode : undefined },
    { name: 'credit_limit_micros', label: '授信上限' + money, type: 'number', value: editing ? editing.credit_limit_micros : undefined },
    { name: 'low_balance_threshold_micros', label: '低额告警阈值' + money, type: 'number', value: editing ? editing.low_balance_threshold_micros : undefined },
  );
  if (editing) {
    fields.push({ name: 'overdraft_limit_micros', label: '在途透支上限（微' + ledgerCurrency() + '）', type: 'number', value: editing.overdraft_limit_micros });
  }
  fields.push(
    { name: 'tags', label: '账号标签（逗号分隔）', value: (tags || []).join(', '), hint: '所有 API Key 自动继承；留空表示不绑定标签' },
    // 组织不再是"填 id"：勾选树是同一个控件，账户页与组织页给的是同一份数据。
    orgField({ title: '分配组织', refs: orgRefs }),
    { name: 'note', label: '备注', value: editing ? editing.note : undefined },
  );
  return fields;
}

// createAccount 建一个账号（可预置所属组织，例如在某个节点下「新建成员」）。返回新账号，取消返回 null。
export async function createAccount({ title, submitLabel, orgRefs } = {}) {
  const fields = await accountFields({ orgRefs });
  return await modal({
    title: title || '新建账户',
    submitLabel: submitLabel || '创建',
    fields,
    onSubmit: (values) => api.post('/accounts', {
      ...values,
      tags: splitTags(values.tags),
      // 永远是数组：空数组是"不属于任何组织"这条明确指令，省略字段则是"别动归属"。
      org_node_ids: values.org_node_ids || [],
    }),
  });
}

// editAccount 改一个账号的字段（含所属组织）。返回更新后的账号，取消返回 null。
export async function editAccount(account, { title } = {}) {
  const fields = await accountFields({ tags: account.tags, orgRefs: account.org_nodes, editing: account });
  return await modal({
    title: title || '编辑账户 ' + account.name,
    fields,
    onSubmit: (values) => api.patch('/accounts/' + account.id, {
      ...values,
      tags: splitTags(values.tags),
      org_node_ids: values.org_node_ids || [],
    }),
  });
}

function splitTags(value) {
  return (value || '').split(',').map((tag) => tag.trim()).filter(Boolean);
}
