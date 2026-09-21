// 组织架构页：一棵独立的组织树 + 选中节点的人员（账号）列表。
//
// M72 起这里是"账号/人员"的主界面：人员列表的每一行都能展开，展开处是该账号的账号字段、
// Key 列表与逐账号操作（新建/编辑/启停 Key、绑定飞书、启用/停用 DSH、分配组织）。树仍然只
// 渲染在工作区里：它曾经同时挂一份到左侧栏（紧凑模式）并让两处选中互相镜像，产品上判定为
// 冗余（同一棵树在同一屏出现两次，反而让左侧栏的全局导航变挤）；树控件本身（../tree.js）
// 仍然支持 `mode:'sidebar'`，那项能力由 scripts/ui-harness 的 `tree` 视图单独守着。
// 这个页面只负责把接口数据喂给控件、把它的回调接回接口。

import { api } from '../api.js';
import { el, card, modal, toast, confirmDialog, badge, formatTime } from '../ui.js';
import { tree } from '../tree.js';
import { matchesQuery } from '../pinyin.js';
import { openFeishuSync } from './org_feishu.js';
// Key 的创建/编辑/启停是组织页与 API Keys 页共用的实现（M72）：明文只出现一次的那套顺序
// 只有一份，就不会有页面忘记它。
import { createKeyForAccount, editKey, toggleKey } from './key_actions.js';
// 账号级飞书绑定：人员弹窗（不是扫码）与解绑。
import { openFeishuPersonPicker, unbindAccountFeishu } from './account_feishu.js';

// 成员勾选列表最多拉这么多账号。组织页需要展示"这个部门有哪些账号"，一次拉全量比做一套
// 分页多选更简单；账号数量超过这个上限时列表会截断，并明确提示去账户页按组织筛选。
const MEMBER_PICK_LIMIT = 1000;

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const refreshBtn = el('button', { class: 'btn', text: '刷新' });
  const createRoot = el('button', { class: 'btn btn-primary', text: '新建根节点', disabled: readonly });
  const expandAll = el('button', { class: 'btn', text: '展开全部' });
  const collapseAll = el('button', { class: 'btn', text: '折叠全部' });
  // 「同步飞书」是管理员动作（它会在飞书上调用 API 并创建节点），只读角色看到的按钮是灰的，
  // 与「新建根节点」同一口径。
  const syncFeishu = el('button', {
    class: 'btn', text: '同步飞书', disabled: readonly,
    title: readonly ? '只读角色不能同步飞书' : '读取飞书通讯录，按部门与人员合并到本地组织架构',
  });
  actions.append(refreshBtn, expandAll, collapseAll, createRoot, syncFeishu);

  const state = { nodes: [], selectedId: null, accounts: [], accountsTruncated: false, membersLoaded: false };

  const mainTree = tree({
    mode: 'workspace',
    filter: true,
    filterPlaceholder: '过滤节点名（支持拼音）…',
    matcher: matchesQuery,
    emptyText: '暂无组织节点，先新建一个根节点',
    renderLabel: (node) => node.name,
    renderMeta: (node) => metaFor(node),
    actions: (node) => readonly ? [] : [
      el('button', { class: 'btn tree-action', dataset: { action: 'add-child' }, text: '子节点' }),
      el('button', { class: 'btn tree-action', dataset: { action: 'edit' }, text: '编辑' }),
      el('button', { class: 'btn btn-danger tree-action', dataset: { action: 'delete' }, text: '删除' }),
    ],
    onSelect: (node) => selectNode(node ? node.id : null),
    onAction: (name, node) => {
      if (name === 'add-child') createNode(node);
      else if (name === 'edit') editNode(node);
      else if (name === 'delete') removeNode(node);
    },
  });

  const detail = el('div', { class: 'org-detail' });
  const treeCard = card('组织架构', mainTree.node, [
    el('span', { class: 'muted', text: '节点上的标签会被整棵子树继承；账号可同时属于多个节点' })]);
  treeCard.classList.add('org-tree-card');
  page.append(el('div', { class: 'org-layout' }, [treeCard, detail]));

  refreshBtn.addEventListener('click', () => load());
  expandAll.addEventListener('click', () => mainTree.expandAll());
  collapseAll.addEventListener('click', () => mainTree.collapseAll());
  createRoot.addEventListener('click', () => createNode(null));
  // 同步完成后整页重载：同步会创建节点、改变账号归属，树与成员数都要重新读。
  syncFeishu.addEventListener('click', () => openFeishuSync({ onDone: (changed) => { if (changed) load(); } }));

  function metaFor(node) {
    const parts = [];
    if (node.account_count) parts.push(node.account_count + ' 个账号');
    const tags = node.tags || [];
    if (tags.length) parts.push(tags.join(' / '));
    return el('span', { class: 'org-meta', text: parts.join(' · ') });
  }

  function selectNode(id) {
    state.selectedId = id;
    mainTree.setSelected(id);
    renderDetail();
  }

  function nodeById(id) {
    return state.nodes.find((node) => node.id === id) || null;
  }

  function renderDetail() {
    detail.replaceChildren();
    // 未归属账户（M72）：不在任何节点下的账号也要有人管，而 GET /accounts 是唯一能回答
    // 「谁不属于任何节点」的接口。它是一个**视图**而不是节点：勾选与保存成员在这里没有意义
    // （没有节点可写），所以这一行的勾选框会被关掉。
    const unassigned = state.accounts.filter((account) => !(account.org_node_ids || []).length);
    const node = nodeById(state.selectedId);
    if (!node) {
      detail.append(card('节点详情', [
        el('div', { class: 'empty', text: '选择左侧的一个节点查看详情' }),
        unassignedRow(unassigned),
        unassigned.length ? personList(null, unassigned) : null,
      ].filter(Boolean)));
      return;
    }
    const info = el('div', {}, [
      row('名称', node.name),
      row('路径', el('span', { class: 'org-path', text: node.path || node.name })),
      row('层级', '第 ' + (node.depth + 1) + ' 层'),
      row('排序', String(node.sort_order)),
      row('备注', node.note || '—'),
      row('标签', (node.tags || []).length
        ? el('span', {}, (node.tags || []).map((tag) => badge(tag, 'ok')))
        : el('span', { class: 'muted', text: '未绑定标签（该子树不继承任何组织的标签）' })),
    ]);

    const checked = new Set();
    const selectedCount = el('span', { class: 'muted' });
    const searchBox = el('input', { type: 'search', placeholder: '按账号名或飞书姓名过滤（支持拼音，如 zhangsan）…' });
    const list = el('div', { class: 'org-members' });
    // The filter sits in its own row above the scrolling list, so it stays put while the
    // operator scrolls through candidates — a filter that scrolls away is unusable exactly
    // when the list is long enough to need filtering.
    const memberToolbar = el('div', { class: 'org-member-toolbar' }, [searchBox, selectedCount]);

    let search = '';
    searchBox.addEventListener('input', () => { search = searchBox.value; paint(); });

    // paint renders the person rows. The checkbox means "is a member of THIS node", so it is only
    // editable while the whole account list is shown: with a search filter on, "保存成员" would
    // replace the node's membership with whatever subset happens to be visible.
    function paint() {
      list.replaceChildren();
      const filtering = search.trim() !== '';
      const wanted = state.accounts
        .filter((account) => matchesPerson(account, search))
        // Already-checked accounts come first, so the members of this node stay visible at the
        // top of a long account list; a freshly ticked account jumps there immediately.
        .sort((left, right) => Number(checked.has(right.id)) - Number(checked.has(left.id))
          || left.name.localeCompare(right.name, 'zh-Hans-CN'));
      selectedCount.textContent = checked.size ? '已选 ' + checked.size + ' 个' : '';
      if (!wanted.length) {
        list.append(el('div', { class: 'empty', text: state.accounts.length ? '无匹配账号' : '没有可分配的账号' }));
        return;
      }
      for (const account of wanted) list.append(personRow(account, node, checked, paint, filtering));
    }

    const saveMembers = el('button', {
      class: 'btn btn-primary', text: '保存成员',
      disabled: readonly || !state.membersLoaded,
      title: readonly ? '只读角色不能修改成员' : '',
    });
    saveMembers.addEventListener('click', async () => {
      saveMembers.disabled = true;
      try {
        await api.put('/org/nodes/' + node.id + '/accounts', { account_ids: [...checked] });
        toast('成员已保存', 'ok');
        await load();
      } catch (err) {
        toast(api.errorMessage(err), 'error');
        saveMembers.disabled = readonly;
      }
    });

    const memberPanel = el('div', { class: 'org-member-panel' }, [memberToolbar, list]);
    paint();
    detail.append(card('节点详情：' + node.name, [info,
      el('div', { class: 'toolbar' }, [
        el('h3', { text: '人员（账号）', style: 'margin:0;flex:1' }),
        el('span', { class: 'muted', text: '展开一行可以看到该账号的 Key 列表与飞书身份' }),
        readonly ? el('span', { class: 'muted', text: '只读角色不能修改' }) : saveMembers]),
      memberPanel,
      unassignedRow(unassigned),
      state.accountsTruncated
        ? el('div', { class: 'muted', text: '账号列表已截断（只显示前 ' + MEMBER_PICK_LIMIT + ' 个）；完整列表见账户页按组织筛选' })
        : null,
    ]));

    // Fill the checkbox state from the server once the memberships are known.
    loadMembers(node.id, checked, saveMembers, paint);
  }

  // unassignedRow reports the accounts that belong to no node at all, and opens a read-only list
  // of them. They need somewhere to live: an account created from the directory sync before any
  // node exists (or one an operator moved out) otherwise only shows up on the accounts page.
  function unassignedRow(unassigned) {
    const line = el('div', { class: 'org-unassigned' });
    if (!unassigned.length) {
      line.append(el('span', { class: 'muted', text: '所有账号都归属至少一个节点' }));
      return line;
    }
    const toggle = el('button', { class: 'btn', text: '未归属账户 ' + unassigned.length + ' 个' });
    const box = el('div', { class: 'org-unassigned-list', hidden: true });
    let open = false;
    toggle.addEventListener('click', () => {
      open = !open;
      box.hidden = !open;
      if (open && !box.childElementCount) box.append(personList(null, unassigned));
    });
    line.append(toggle, el('span', { class: 'muted', text: '不在任何节点下的账号：展开后可用「分配组织」把它们挂到节点上' }), box);
    return line;
  }

  // personRow renders one account: the membership checkbox, what the account is (DSH state, Feishu
  // identity, key count) and an expander that shows its keys and the operations on them (M72).
  function personRow(account, node, checked, repaint, filtering) {
    const box = el('input', { type: 'checkbox', disabled: readonly, title: '加入这个节点' });
    box.checked = checked.has(account.id);
    box.addEventListener('change', () => {
      if (box.checked) checked.add(account.id); else checked.delete(account.id);
      repaint();
    });
    const expand = el('button', { class: 'btn org-member-toggle', text: '展开' });
    const summary = el('label', { class: 'org-member' }, [
      box,
      // The name and id carry their own classes: the row's layout rules key off them, and a
      // bare <span> would have to be targeted positionally in CSS.
      el('span', { class: 'org-member-name', text: account.name }),
      dshBadge(account),
      feishuBadge(account),
      keyBadge(account),
      el('span', { class: 'org-member-id muted', text: '#' + account.id }),
      expand,
    ]);
    const container = el('div', { class: 'org-person' }, [summary]);
    if (filtering) {
      // A filtered list is a view, not the node's membership: saving from here would drop the
      // rows the filter hid.
      box.disabled = true;
      box.title = '过滤时不能改成员：清空过滤框后可以勾选';
    }
    expand.addEventListener('click', async (ev) => {
      ev.preventDefault();
      const open = container.classList.toggle('open');
      expand.textContent = open ? '收起' : '展开';
      if (open && !container.querySelector('.org-person-detail')) {
        container.append(await personDetail(account));
      }
    });
    return container;
  }
  // state.membersLoaded guards the save button until the node's current members are known:
  // saving an empty set before the read lands would clear the department.
  async function loadMembers(nodeID, checked, saveButton, paint) {
    state.membersLoaded = false;
    checked.clear();
    try {
      const payload = await api.get('/org/nodes/' + nodeID + '/accounts', { limit: MEMBER_PICK_LIMIT });
      for (const account of payload.data || []) checked.add(account.id);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
    // A slower earlier request must not enable the button for a node the operator has since
    // navigated away from.
    if (state.selectedId !== nodeID) return;
    state.membersLoaded = true;
    saveButton.disabled = readonly;
    paint();
  }

  // personList renders a read-only list of accounts (the unassigned view): no checkboxes, because
  // there is no node to write membership to.
  function personList(_node, accounts) {
    const box = el('div', { class: 'org-members org-members-plain' });
    for (const account of accounts) box.append(personRow(account, null, new Set(), () => {}, false));
    return box;
  }

  // dshBadge states the account's DSH situation in the three values an operator has to tell apart
  // (M72): enabled, explicitly disabled by an administrator, or never enabled.
  function dshBadge(account) {
    if (account.dsh_enabled) return badge('DSH 已启用', 'ok', account.dsh_tenant || '');
    if (account.dsh_disabled_at) return badge('DSH 已停用（管理员）', 'warn', '自动启用不会撤销它');
    if (account.dsh_effective) return badge('DSH 登录即可用', 'ok', '本部署开启了自动启用：首次登录会创建租户');
    return el('span', { class: 'muted', text: 'DSH 未启用' });
  }

  function feishuBadge(account) {
    const feishu = account.feishu || {};
    if (!feishu.bound) return el('span', { class: 'muted', text: '未绑飞书' });
    return badge('飞书：' + (feishu.name || feishu.open_id), 'ok', feishu.open_id);
  }

  function keyBadge(account) {
    const active = account.active_key_count || 0;
    const total = account.key_count || 0;
    if (!total) return el('span', { class: 'muted', text: '0 个 Key' });
    return badge(active + ' / ' + total + ' 个 Key', active > 1 ? 'warn' : '',
      active > 1 ? '这个账号有多把可用 Key：门户登录会先让人选一把（只影响登录归属与审计）' : '');
  }

  // personDetail is the expanded half of a person row: the account's own fields, its Key list
  // (fetched here, per account), and the operations that belong to that account.
  async function personDetail(account) {
    const panel = el('div', { class: 'org-person-detail' });
    panel.append(el('div', { class: 'org-person-facts' }, [
      el('span', { class: 'muted', text: '状态 ' + (account.status || 'active') }),
      el('span', { class: 'muted', text: '计费 ' + (account.billing_mode || '—') }),
      el('span', { class: 'muted', text: '标签 ' + ((account.tags || []).join(', ') || '—') }),
      el('span', { class: 'muted', text: '组织 ' + ((account.org_node_ids || []).length ? (account.org_nodes || []).map((n) => n.path || n.name).join(' / ') : '未归属') }),
    ]));
    const actions = el('div', { class: 'toolbar org-person-actions' }, [
      el('button', { class: 'btn', text: '新建 Key', disabled: readonly, onclick: () => addKey(account, refreshDetail) }),
      el('button', {
        class: 'btn', text: account.dsh_enabled ? '停用 DSH' : '启用 DSH', disabled: readonly,
        onclick: () => toggleDSH(account, refreshDetail),
      }),
      el('button', {
        class: 'btn', text: (account.feishu && account.feishu.bound) ? '解绑飞书' : '绑定飞书', disabled: readonly,
        onclick: () => account.feishu && account.feishu.bound ? unbindFeishu(account, refreshDetail) : bindFeishu(account, refreshDetail),
      }),
      el('button', { class: 'btn', text: '分配组织', disabled: readonly, onclick: () => assignOrgs(account, refreshDetail) }),
    ]);
    const keysBox = el('div', { class: 'org-person-keys' }, [el('div', { class: 'muted', text: '正在读取 Key…' })]);
    panel.append(actions, keysBox);

    async function refreshDetail() {
      const fresh = await reloadAccount(account.id);
      keysBox.replaceChildren();
      if (!fresh) {
        keysBox.append(el('div', { class: 'muted', text: '账号信息读取失败' }));
        return;
      }
      panel.replaceChildren();
      panel.append(await personDetail(fresh));
    }

    try {
      const payload = await api.get('/keys', { account_id: account.id, limit: 100 });
      const keys = payload.data || [];
      keysBox.replaceChildren();
      if (!keys.length) {
        keysBox.append(el('div', { class: 'muted', text: '这个账号还没有 Key：没有 Key 就无法登录门户（可用「新建 Key」）' }));
        return panel;
      }
      keysBox.append(el('h4', { text: 'Key（' + keys.length + '）' }));
      for (const key of keys) {
        const isWorker = String(key.name || '').startsWith('dshgw-');
        keysBox.append(el('div', { class: 'org-person-key' }, [
          el('span', { class: 'org-person-key-name', text: key.name }),
          el('code', { text: (key.key_prefix || '') + '…' }),
          el('span', { class: 'muted', text: isWorker ? '网关自动创建（worker 凭据，不参与选择）' : (key.status === 'active' ? '可用' : key.status) }),
          el('span', { class: 'muted', text: key.last_used_at ? '最近使用 ' + formatTime(key.last_used_at) : '从未使用' }),
          isWorker ? null : el('button', {
            class: 'btn btn-small', text: '编辑', disabled: readonly,
            onclick: () => editKey(key, refreshDetail),
          }),
          isWorker ? null : el('button', {
            class: 'btn btn-small', text: key.status === 'active' ? '停用' : '启用', disabled: readonly,
            onclick: () => toggleKey(key, refreshDetail),
          }),
        ].filter(Boolean)));
      }
    } catch (err) {
      keysBox.replaceChildren(el('div', { class: 'muted', text: api.errorMessage(err) }));
    }
    return panel;
  }

  // --- 逐账号操作（M72）：组织页的展开行里可用的动作 --------------------------------

  // addKey creates a Key for this account. The plaintext is shown once, which is why the shared
  // creator owns the whole sequence rather than this page rebuilding it.
  async function addKey(account, refresh) {
    const created = await createKeyForAccount(account, { name: '' });
    if (created) await refresh();
  }

  // toggleDSH flips the account's DSH switch from the person row. It says the same things as the
  // accounts page, because the consequence is the same: disabling stops the worker and records an
  // explicit disable that dshgw.auto_enable will not undo.
  async function toggleDSH(account, refresh) {
    if (account.dsh_enabled) {
      const ok = await confirmDialog('停用 DSH',
        '停用账号 ' + account.name + ' 的 dsh？将停止其 worker 并吊销 worker 专用 Key；新登录被拒绝，' +
        '既有会话按网关 dsh_enforce 档位失效。工作区与 dsh 数据保留，重新启用即恢复。\n' +
        '这是「显式停用」：即使本部署开启了自动启用，也不会在下次登录时自动重新启用它。');
      if (!ok) return;
      try {
        await api.post('/accounts/' + account.id + '/dsh', { enabled: false });
        toast('已停用 DSH', 'ok');
        await load();
      } catch (err) {
        toast(api.errorMessage(err), 'error');
      }
      return;
    }
    const suggested = account.dsh_tenant || slugFromAccount(account.name);
    const result = await modal({
      title: '启用 DSH — ' + account.name,
      submitLabel: '启用',
      fields: [{ name: 'tenant', label: 'dsh 租户名', value: suggested,
        hint: '小写字母/数字/连字符；留空则沿用既有映射或按账号名自动生成' }],
      onSubmit: (values) => api.post('/accounts/' + account.id + '/dsh', {
        enabled: true, ...(values.tenant ? { tenant: values.tenant } : {}),
      }),
    });
    if (!result) return;
    toast('已启用 DSH（租户 ' + (result.tenant || suggested) + '）', 'ok');
    await load();
  }

  // bindFeishu opens the person picker (M72): the administrator chooses who this account is,
  // which is why binding needs no consent screen any more.
  async function bindFeishu(account, refresh) {
    const bound = await openFeishuPersonPicker({ account });
    if (bound) {
      await load();
      await refresh();
    }
  }

  async function unbindFeishu(account, refresh) {
    if (await unbindAccountFeishu(account)) {
      await load();
      await refresh();
    }
  }

  // assignOrgs replaces the account's organization memberships (the same field the accounts page
  // edits). It is how an unassigned account gets a home.
  async function assignOrgs(account, refresh) {
    const result = await modal({
      title: '分配组织 — ' + account.name,
      submitLabel: '保存',
      fields: [{
        name: 'org_node_ids', label: '组织节点 id（逗号分隔）',
        hint: '整表替换：留空即移出全部组织，从节点继承来的标签授权随即失效',
        value: (account.org_node_ids || []).join(', '),
      }],
      onSubmit: (values) => api.patch('/accounts/' + account.id, { org_node_ids: splitList(values.org_node_ids).map(Number) }),
    });
    if (!result) return;
    toast('组织归属已更新', 'ok');
    await load();
    await refresh();
  }

  // reloadAccount re-reads one account so an expanded row shows what the last write produced.
  async function reloadAccount(id) {
    try {
      const payload = await api.get('/accounts', { limit: MEMBER_PICK_LIMIT });
      return (payload.data || []).find((row) => row.id === id) || null;
    } catch (err) {
      toast(api.errorMessage(err), 'error');
      return null;
    }
  }

  function row(label, value) {
    return el('div', { class: 'org-row' }, [el('span', { class: 'k', text: label }), el('span', { class: 'v' }, [value])]);
  }

  async function load() {
    try {
      const payload = await api.get('/org/nodes', { limit: 1000 });
      state.nodes = payload.data || [];
    } catch (err) {
      toast(api.errorMessage(err), 'error');
      state.nodes = [];
    }
    mainTree.refresh(state.nodes);
    if (state.selectedId === null || !nodeById(state.selectedId)) {
      state.selectedId = state.nodes.length ? state.nodes[0].id : null;
    }
    mainTree.setSelected(state.selectedId);
    renderDetail();
  }

  async function loadAccounts() {
    try {
      const payload = await api.get('/accounts', { limit: MEMBER_PICK_LIMIT });
      // The person rows need the whole account, not just its name (M72): DSH state, Feishu
      // identity and key counts all render in the list.
      state.accounts = payload.data || [];
      state.accountsTruncated = (payload.total || state.accounts.length) > state.accounts.length;
    } catch (err) {
      state.accounts = [];
    }
  }

  async function createNode(parent) {
    const result = await modal({
      title: parent ? '在「' + parent.name + '」下新建子节点' : '新建根节点',
      fields: [
        { name: 'name', label: '名称', required: true, hint: '同一父节点下不能重名，最多 64 个字符' },
        { name: 'tags', label: '标签（逗号分隔）', hint: '整棵子树继承这些标签；名字必须已存在，否则会被拒绝' },
        { name: 'note', label: '备注' },
        { name: 'sort_order', label: '排序', type: 'number', value: 100 },
      ],
      onSubmit: (values) => api.post('/org/nodes', {
        name: values.name,
        parent_id: parent ? parent.id : undefined,
        tags: splitList(values.tags),
        note: values.note,
        sort_order: Number(values.sort_order) || 0,
      }),
    });
    if (result) {
      toast('节点已创建', 'ok');
      await load();
    }
  }

  async function editNode(node) {
    const result = await modal({
      title: '编辑节点：' + node.name,
      fields: [
        { name: 'name', label: '名称', required: true, value: node.name },
        { name: 'tags', label: '标签（逗号分隔）', value: (node.tags || []).join(', '),
          hint: '整棵子树继承这些标签；改这里会立即改变该子树下所有 API Key 的授权' },
        { name: 'parent_id', label: '父节点 id（留空 = 根节点）', type: 'number',
          value: node.parent_id === null || node.parent_id === undefined ? '' : node.parent_id,
          hint: '不能移到自身或自己的子孙节点下；整棵子树会跟着移动' },
        { name: 'sort_order', label: '排序', type: 'number', value: node.sort_order },
        { name: 'note', label: '备注', value: node.note || '' },
      ],
      onSubmit: (values) => api.patch('/org/nodes/' + node.id, {
        name: values.name,
        tags: splitList(values.tags),
        parent_id: values.parent_id === '' ? 0 : Number(values.parent_id),
        sort_order: Number(values.sort_order) || 0,
        note: values.note,
      }),
    });
    if (result) {
      toast('节点已更新', 'ok');
      await load();
    }
  }

  async function removeNode(node) {
    const ok = await confirmDialog('删除组织节点',
      '确认删除「' + node.name + '」吗？如果它有子节点，整棵子树会被一起删除；其中的成员关系会消失，账号本身不受影响，但这些账号继承来的标签授权会立即失效。');
    if (!ok) return;
    try {
      await api.del('/org/nodes/' + node.id + '?cascade=true');
      toast('节点已删除', 'ok');
      state.selectedId = null;
      await load();
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  await Promise.all([loadAccounts(), load()]);
}

// matchesPerson filters by account name OR Feishu name, so an operator who knows the person from
// the directory can find the account even when the two names differ.
function matchesPerson(account, search) {
  if (!search || !search.trim()) return true;
  const feishu = (account.feishu && account.feishu.name) || '';
  return matchesQuery(account.name, search) || (feishu ? matchesQuery(feishu, search) : false);
}

function splitList(value) {
  return (value || '').split(',').map((item) => item.trim()).filter(Boolean);
}

// slugFromAccount mirrors the server's tenant-name candidate so the dialog suggests the same name
// the server would pick; the server stays the authority on uniqueness.
function slugFromAccount(name) {
  let slug = 'dsh-' + String(name || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  if (slug.length > 26) slug = slug.slice(0, 26).replace(/-+$/, '');
  return /^[a-z][a-z0-9-]{0,25}[a-z]$|^[a-z]$/.test(slug) ? slug : 'dsh-tenant';
}
