// 组织架构页：一棵独立的组织树 + 选中节点的详情。
//
// 树只渲染在工作区里。它曾经同时挂一份到左侧栏（紧凑模式）并让两处选中互相镜像，产品上判定
// 为冗余：同一棵树在同一屏出现两次，反而让左侧栏的全局导航变挤。树控件本身（../tree.js）
// 仍然支持 `mode:'sidebar'`，那项能力由 scripts/ui-harness 的 `tree` 视图单独守着。
// 这个页面只负责把接口数据喂给控件、把它的回调接回接口。

import { api } from '../api.js';
import { el, card, modal, toast, confirmDialog, badge } from '../ui.js';
import { tree } from '../tree.js';
import { matchesQuery } from '../pinyin.js';

// 成员勾选列表最多拉这么多账号。组织页需要展示"这个部门有哪些账号"，一次拉全量比做一套
// 分页多选更简单；账号数量超过这个上限时列表会截断，并明确提示去账户页按组织筛选。
const MEMBER_PICK_LIMIT = 1000;

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const refreshBtn = el('button', { class: 'btn', text: '刷新' });
  const createRoot = el('button', { class: 'btn btn-primary', text: '新建根节点', disabled: readonly });
  const expandAll = el('button', { class: 'btn', text: '展开全部' });
  const collapseAll = el('button', { class: 'btn', text: '折叠全部' });
  actions.append(refreshBtn, expandAll, collapseAll, createRoot);

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

    const checked = new Set();
    let search = '';

    const list = el('div', { class: 'org-members' });
    const selectedCount = el('span', { class: 'muted' });
    const searchBox = el('input', { type: 'search', placeholder: '按账号名过滤（支持拼音，如 zhangsan）…' });
    searchBox.addEventListener('input', () => { search = searchBox.value; paint(); });
    // The filter sits in its own row above the scrolling list, so it stays put while the
    // operator scrolls through candidates — a filter that scrolls away is unusable exactly
    // when the list is long enough to need filtering.
    const memberToolbar = el('div', { class: 'org-member-toolbar' }, [searchBox, selectedCount]);

    function paint() {
      // Already-checked accounts come first, so the members of this node stay visible at the
      // top of a long account list. Sorting happens on every repaint, which is what makes a
      // freshly ticked account jump to the top immediately.
      const wanted = state.accounts
        .filter((account) => matchesQuery(account.name, search))
        .sort((left, right) => Number(checked.has(right.id)) - Number(checked.has(left.id))
          || left.name.localeCompare(right.name, 'zh-Hans-CN'));
      selectedCount.textContent = checked.size ? '已选 ' + checked.size + ' 个' : '';
      list.replaceChildren();
      if (!wanted.length) {
        list.append(el('div', { class: 'empty', text: state.accounts.length ? '无匹配账号' : '没有可分配的账号' }));
        return;
      }
      for (const account of wanted) {
        const box = el('input', { type: 'checkbox', disabled: readonly });
        box.checked = checked.has(account.id);
        box.addEventListener('change', () => {
          if (box.checked) checked.add(account.id); else checked.delete(account.id);
          paint();
        });
        // The name and id carry their own classes: the row's layout rules key off them, and a
        // bare <span> would have to be targeted positionally in CSS.
        list.append(el('label', { class: 'org-member' }, [
          box,
          el('span', { class: 'org-member-name', text: account.name }),
          el('span', { class: 'org-member-id muted', text: '#' + account.id }),
        ]));
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

    const memberPanel = el('div', { class: 'org-member-panel' }, [memberToolbar, list]);
    paint();
    detail.append(card('节点详情：' + node.name, [info,
      el('div', { class: 'toolbar' }, [
        el('h3', { text: '成员账号', style: 'margin:0;flex:1' }),
        readonly ? el('span', { class: 'muted', text: '只读角色不能修改' }) : saveMembers]),
      memberPanel,
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
