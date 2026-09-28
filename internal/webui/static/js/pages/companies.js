// 「公司」页（M93）：登记要导入组织架构的客户公司（各自一个飞书自建应用）。
//
// 这一页回答两个问题：**现在能导入哪些公司**（含它们各自的公司节点与已导入规模），以及
// **怎么加一家新公司**。密钥的规矩写在界面上、也写在服务端：加密落库、永不回显、不进日志与审计，
// 所以列表只显示"已配置/未配置"，编辑对话框里留空就表示不改。
//
// 身份应用（本公司）与 feishu.companies 里登记的公司在这一页是**只读**的：前者属于部署配置，
// 后者是"配置即真源"。页面把它们标出来并指明去哪改。

import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, badge, confirmDialog } from '../ui.js';

export async function render({ page, actions, session, navigate }) {
  const readonly = session.role !== 'admin';
  let secretsReady = true;

  const create = el('button', { class: 'btn btn-primary', text: '新建公司', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(create, refresh);

  const notice = el('div', { class: 'card-note org-company-notice' });

  const view = pagedTable({
    columns: [
      { key: 'name', label: '公司', render: (row) => companyCell(row) },
      { key: 'app_id', label: 'App ID', render: (row) => el('code', { text: row.app_id }) },
      { key: 'enabled', label: '状态', render: (row) => statusCell(row) },
      { key: 'root_node', label: '公司节点', render: (row) => rootCell(row) },
      { key: 'company_nodes', label: '节点数' },
      { key: 'linked_accounts', label: '已映射账号' },
    ],
    empty: '还没有公司',
    rowActions: (row) => actionsFor(row),
    load: async ({ limit, offset }) => {
      const payload = await api.get('/org/feishu/companies', { limit, offset });
      secretsReady = payload.secrets_ready !== false;
      create.disabled = readonly || !secretsReady;
      notice.replaceChildren(noticeLine(payload));
      return payload;
    },
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', () => openEditor(null, () => view.refresh()));
  page.append(card('飞书公司', view.node, [
    el('span', { class: 'muted', text: '每家公司一个自建应用，只用于读取通讯录并导入组织架构；密钥加密落库，永不回显' })]));
  page.append(notice);

  // --- helpers ------------------------------------------------------------

  function noticeLine(payload) {
    if (!secretsReady) {
      return el('div', { class: 'muted' }, [
        el('strong', { text: '这个部署没有配置 credentials_key' }),
        el('span', {
          text: '：公司密钥无法加密落库，因此不能在控制台新建公司。' +
            '要么在配置里补上 credentials_key 后重启，要么改用 feishu.companies 写死公司（那种方式在本页只读）。',
        }),
      ]);
    }
    return el('div', { class: 'muted', text: '身份应用与 feishu.companies 登记的公司在本页只读；控制台登记的公司可以改名、换密钥、停用或删除登记（删除只删登记，节点与人员映射保留）。' });
  }

  function companyCell(row) {
    const cells = [el('span', { class: 'org-company-name', text: row.name })];
    const source = row.source === 'identity' ? '身份应用'
      : (row.source === 'config' ? '由配置提供' : '控制台');
    cells.push(badge(source, row.source === 'console' ? '' : 'ok'));
    if (row.note) cells.push(el('span', { class: 'muted', text: row.note }));
    for (const warning of row.warnings || []) {
      cells.push(el('div', { class: 'muted org-company-warning', text: warningText(warning) }));
    }
    return el('div', { class: 'org-company-cell' }, cells);
  }

  function statusCell(row) {
    if (!row.enabled) return badge('已停用', 'danger');
    if (row.source === 'console' && row.client_ready === false) return badge('密钥不可用', 'danger');
    if (row.source === 'console' && row.secret_configured === false) return badge('未填密钥', 'danger');
    return badge('启用', 'ok');
  }

  function rootCell(row) {
    if (row.root_blocked) return badge('冲突：' + (row.root_node_name || ''), 'danger');
    if (row.root_node_id) return el('span', { text: row.root_node_name || '' });
    if (row.root_will_create) return el('span', { class: 'muted', text: '未建（同步时新建「' + (row.root_node_name || '') + '」）' });
    return el('span', { class: 'muted', text: '—' });
  }

  function actionsFor(row) {
    const buttons = [];
    // 同步始终可用（它是这家公司最常做的事），其余动作只对控制台登记的公司开放。
    buttons.push(el('button', {
      class: 'btn', text: '同步',
      onclick: () => navigate('/org?company=' + encodeURIComponent(row.app_id)),
    }));
    if (row.source !== 'console') {
      buttons.push(el('span', { class: 'muted', text: row.source === 'identity' ? '由 feishu.app_id 管理' : '由 feishu.companies 管理' }));
      return buttons;
    }
    buttons.push(el('button', { class: 'btn', text: '测试连接', onclick: () => probe(row) }));
    if (!readonly) {
      buttons.push(el('button', { class: 'btn', text: '编辑', onclick: () => openEditor(row, () => view.refresh()) }));
      if (row.shadowed_by_config) {
        buttons.push(el('button', { class: 'btn btn-danger', text: '删除冗余登记', onclick: () => remove(row, () => view.refresh()) }));
      } else {
        buttons.push(el('button', {
          class: 'btn', text: row.enabled ? '停用' : '启用',
          onclick: () => toggle(row),
        }));
        buttons.push(el('button', { class: 'btn btn-danger', text: '删除登记', onclick: () => remove(row, () => view.refresh()) }));
      }
    }
    return buttons;
  }

  function warningText(warning) {
    const [code] = String(warning).split(':');
    switch (code) {
      case 'shadowed_by_config':
        return '这条登记被 feishu.companies 覆盖（配置优先）；可删除这条冗余登记';
      case 'duplicate_name':
        return '公司名与配置里的公司重复：按名字解析会落到配置那家，请改名或用 app_id';
      default:
        return warning;
    }
  }

  // --- writes -------------------------------------------------------------

  async function toggle(row) {
    const enabling = !row.enabled;
    if (!enabling) {
      const ok = await confirmDialog('停用公司「' + row.name + '」',
        '停用后这家公司不再出现在同步下拉里，按 app_id/公司名同步会 400；\n' +
        '已导入的节点、成员关系与人员映射都保留，随时可以再启用。');
      if (!ok) return;
    }
    try {
      await api.patch('/org/feishu/companies/' + row.id, { enabled: enabling });
      toast(enabling ? '已启用「' + row.name + '」' : '已停用「' + row.name + '」', 'ok');
      await view.refresh();
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  async function remove(row, done) {
    const ok = await confirmDialog('删除公司「' + row.name + '」的登记',
      '只删**登记**：已导入的组织节点 ' + (row.company_nodes || 0) + ' 个、人员映射 ' +
      (row.linked_accounts || 0) + ' 条都保留，重新登记同一个 app_id 会重新认领它们。\n\n' +
      '要连数据一起清：先在组织架构页删节点（cascade），并用「清理映射」接口删人员映射。');
    if (!ok) return;
    try {
      const result = await api.del('/org/feishu/companies/' + row.id);
      toast('已删除登记「' + row.name + '」' + (result.deleted ? '' : '（原本就不存在）'), 'ok');
      if (done) await done();
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  async function probe(row) {
    try {
      const result = await api.post('/org/feishu/companies/probe', { id: row.id });
      toast(probeMessage(result), result.ok ? 'ok' : 'error');
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  function probeMessage(result) {
    const prefix = '「测试连接」：';
    if (result.ok) {
      const samples = (result.samples || []).map((sample) => sample.name || sample.id).join('、');
      return prefix + (result.message || '可读') + '（读到 ' + (result.departments_seen || 0) + ' 个部门' +
        (samples ? '，例如 ' + samples : '') + '）';
    }
    return prefix + (result.message || '失败（' + result.stage + '）');
  }

  // openEditor is the create/edit dialog. The secret field is a password input that is never
  // prefilled: "leave it empty" is how the operator says "keep the stored one".
  function openEditor(row, done) {
    const editing = !!row;
    const fields = [
      { name: 'name', label: '公司名', required: true, value: editing ? row.name : '',
        hint: '≤64 字符，与其它公司重名会被拒绝；它也是公司节点的默认名' },
      { name: 'app_id', label: 'App ID', required: !editing, value: editing ? row.app_id : '',
        hint: editing ? '创建后不可更改（它是已导入数据的归属键）' : 'cli_…，来自那家公司自己飞书应用的「凭证与基础信息」',
        readonly: editing },
      { name: 'app_secret', label: 'App Secret', type: 'password', required: !editing,
        hint: editing ? '留空 = 保持不变；填了就替换（加密落库，永不回显）' : '加密落库（credentials_key），永不回显、不进日志与审计' },
      { name: 'root_node', label: '公司根节点名（可选）', value: editing ? (row.root_node_name || '') : '',
        hint: '留空则用公司名；指向已存在的同名根节点会被认领' },
      { name: 'note', label: '备注', value: editing ? (row.note || '') : '' },
      { name: 'enabled', label: '启用', type: 'checkbox', value: editing ? row.enabled : true,
        hint: '停用 = 暂不参与同步（数据保留）' },
    ];
    return modal({
      title: editing ? '编辑公司「' + row.name + '」' : '新建公司',
      fields,
      submitLabel: editing ? '保存' : '创建',
      // 「先测试连接」用刚刚填的值探测一次，不关窗、不落库：密钥抄错与权限没发版本都能当场发现。
      extraActions: [{ label: '先测试连接', onClick: (values) => testBeforeSave(values) }],
      onSubmit: async (values) => {
        const body = {
          name: values.name, root_node: values.root_node, note: values.note,
          enabled: values.enabled !== false,
        };
        if (!editing) body.app_id = values.app_id;
        if (values.app_secret) body.app_secret = values.app_secret;
        const result = editing
          ? await api.patch('/org/feishu/companies/' + row.id, body)
          : await api.post('/org/feishu/companies', body);
        toast(editing ? '已保存「' + values.name + '」' : '已登记「' + values.name + '」，可以开始同步了', 'ok');
        return result;
      },
    }).then(async (result) => {
      if (result && done) await done();
      return result;
    });
  }

  async function testBeforeSave(values) {
    const appID = String(values.app_id || '').trim();
    const secret = String(values.app_secret || '').trim();
    if (!secret) {
      toast('先填一次 App Secret 才能测试（或保存后在列表里按「测试连接」用已保存的密钥）', 'error');
      return;
    }
    const result = await api.post('/org/feishu/companies/probe', { app_id: appID, app_secret: secret });
    toast(probeMessage(result), result.ok ? 'ok' : 'error');
  }

  return view.refresh();
}
