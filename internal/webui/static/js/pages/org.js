// 组织架构页：一棵独立的组织树 + 选中节点的详情。
//
// 同一份数据渲染两棵树：侧边栏里的紧凑树（快速跳转、看层级）和工作区里的完整树（带成员数与
// 行内操作）。两处的选中状态互为镜像——在任何一处选中节点，另一处跟着选中，详情卡随刷新。
// 树控件本身（../tree.js）不知道组织架构，这里只负责把接口数据喂给它、把它的回调接回接口。

import { api } from '../api.js';
import { el, card, modal, toast, confirmDialog, badge } from '../ui.js';
import { tree } from '../tree.js';

// 成员勾选列表最多拉这么多账号。组织页需要展示"这个部门有哪些账号"，一次拉全量比做一套
// 分页多选更简单；账号数量超过这个上限时列表会截断，并明确提示去账户页按组织筛选。
const MEMBER_PICK_LIMIT = 1000;

export async function render({ page, actions, session, sidebar }) {
  const readonly = session.role !== 'admin';
  const refreshBtn = el('button', { class: 'btn', text: '刷新' });
  const createRoot = el('button', { class: 'btn btn-primary', text: '新建根节点', disabled: readonly });
  const expandAll = el('button', { class: 'btn', text: '展开全部' });
  const collapseAll = el('button', { class: 'btn', text: '折叠全部' });
  actions.append(refreshBtn, expandAll, collapseAll, createRoot);

  const state = { nodes: [], selectedId: null, accounts: [], accountsTruncated: false, membersLoaded: false };

  const sidebarTree = tree({
    mode: 'sidebar',
    expandDepth: 1,
    filter: false,
    emptyText: '暂无组织节点',
    renderLabel: (node) => node.name,
    onSelect: (node) => selectNode(node ? node.id : null, { from: 'sidebar' }),
  });
  const mainTree = tree({
    mode: 'workspace',
    filter: true,
    filterPlaceholder: '过滤节点名…',
    emptyText: '暂无组织节点，先新建一个根节点',
    renderLabel: (node) => node.name,
    renderMeta: (node) => metaFor(node),
    actions: (node) => readonly ? [] : [
      el('button', { class: 'btn tree-action', dataset: { action: 'add-child' }, text: '子节点' }),
      el('button', { class: 'btn tree-action', dataset: { action: 'edit' }, text: '编辑' }),
      el('button', { class: 'btn btn-danger tree-action', dataset: { action: 'delete' }, text: '删除' }),
    ],
    onSelect: (node) => selectNode(node ? node.id : null, { from: 'main' }),
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

  // The sidebar slot is the shell's, so this page only fills it when it exists (a harness or an
  // embedded context may render the page without a sidebar).
  if (sidebar) {
    sidebar.append(el('div', { class: 'org-sidebar-head', text: '组织架构' }), sidebarTree.node);
  }

  refreshBtn.addEventListener('click', () => load());
  expandAll.addEventListener('click', () => mainTree.expandAll());
  collapseAll.addEventListener('click', () => mainTree.collapseAll());
  createRoot.addEventListener('click', () => createNode(null));

  function metaFor(node) {
    const parts = [];
    if (node.account_count) parts.push(node.account_count + ' 个账号');
    const tags = node.tags || [];
    if (tags.length) parts.push(tags.join(' / '));
    return el('span', { class: 'org-meta', text: parts.join(' · ') });
  }

  function selectNode(id, { from }) {
    state.selectedId = id;
    if (from !== 'sidebar') sidebarTree.setSelected(id);
    if (from !== 'main') mainTree.setSelected(id);
    renderDetail();
  }

  function nodeById(id) {
    return state.nodes.find((node) => node.id === id) || null;
  }

  function renderDetail() {
    detail.replaceChildren();
    const node = nodeById(state.selectedId);
    if (!node) {
      detail.append(card('节点详情', [el('div', { class: 'empty', text: '选择左侧的一个节点查看详情' })]));
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

    const members = el('div', { class: 'org-members' });
    const checked = new Set();
    let search = '';

    const list = el('div', {});
    const searchBox = el('input', { type: 'search', placeholder: '按账号名过滤…' });
    searchBox.addEventListener('input', () => { search = searchBox.value.trim().toLowerCase(); paint(); });

    function paint() {
      list.replaceChildren();
      const wanted = state.accounts.filter((account) => !search || account.name.toLowerCase().includes(search));
      if (!wanted.length) {
        list.append(el('div', { class: 'empty', text: state.accounts.length ? '无匹配账号' : '没有可分配的账号' }));
        return;
      }
      for (const account of wanted) {
        const box = el('input', { type: 'checkbox', disabled: readonly });
        box.checked = checked.has(account.id);
        box.addEventListener('change', () => {
          if (box.checked) checked.add(account.id); else checked.delete(account.id);
        });
        list.append(el('label', { class: 'org-member' }, [box, el('span', { text: account.name }),
          el('span', { class: 'muted', text: '#' + account.id })]));
      }
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

    members.append(searchBox, list);
    paint();
    detail.append(card('节点详情：' + node.name, [info,
      el('div', { class: 'toolbar' }, [
        el('h3', { text: '成员账号', style: 'margin:0;flex:1' }),
        readonly ? el('span', { class: 'muted', text: '只读角色不能修改' }) : saveMembers]),
      members,
      state.accountsTruncated
        ? el('div', { class: 'muted', text: '账号列表已截断（只显示前 ' + MEMBER_PICK_LIMIT + ' 个）；完整列表见账户页按组织筛选' })
        : null,
    ]));

    // Fill the checkbox state from the server once the memberships are known.
    loadMembers(node.id, checked, saveMembers, paint);
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
    sidebarTree.refresh(state.nodes);
    mainTree.refresh(state.nodes);
    if (state.selectedId === null || !nodeById(state.selectedId)) {
      state.selectedId = state.nodes.length ? state.nodes[0].id : null;
    }
    sidebarTree.setSelected(state.selectedId);
    mainTree.setSelected(state.selectedId);
    renderDetail();
  }

  async function loadAccounts() {
    try {
      const payload = await api.get('/accounts', { limit: MEMBER_PICK_LIMIT });
      state.accounts = (payload.data || []).map((row) => ({ id: row.id, name: row.name }));
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

function splitList(value) {
  return (value || '').split(',').map((item) => item.trim()).filter(Boolean);
}
