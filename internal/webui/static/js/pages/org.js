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
// 账号的创建/编辑与组织归属选择器：与账户页共用同一份实现（fields/权限/落库路径只有一处）。
import { createAccount, editAccount } from './account_actions.js';
import { openOrgPicker } from './org_assign.js';

// 成员勾选列表最多拉这么多账号。组织页需要展示"这个部门有哪些账号"，一次拉全量比做一套
// 分页多选更简单；账号数量超过这个上限时列表会截断，并明确提示去账户页按组织筛选。
const MEMBER_PICK_LIMIT = 1000;

// 人员（账号）列表的列。表头、详情行的 colspan 与两种模式（可勾选 / 未归属那份只读列表）都从
// 这一处推出：加一列不会漏掉其中一处，而未归属列表去掉 pick 列是"整列不渲染"而不是用 CSS 藏。
const MEMBER_COLUMNS = [
  { key: 'pick', label: '', cls: 'c-pick', pick: true },
  { key: 'name', label: '账号', cls: 'c-name' },
  { key: 'dsh', label: 'DSH', cls: 'c-dsh' },
  { key: 'feishu', label: '飞书', cls: 'c-feishu' },
  { key: 'keys', label: 'Key', cls: 'c-keys' },
  { key: 'orgs', label: '所属组织', cls: 'c-orgs' },
  { key: 'ops', label: '操作', cls: 'c-ops' },
];

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

  const state = {
    nodes: [], selectedId: null, accounts: [], accountsTruncated: false, membersLoaded: false,
    // 展开中的账号（按 account id）。它跨整表重建活着：勾选、过滤、成员读取完成、甚至切换节点
    // 都不该把操作员正在看的详情收起来。
    open: new Set(),
  };
  // 当前那张人员表：写操作完成后按 id 就地刷新一行（见 refreshAccount）。
  let currentTable = null;

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
      currentTable = null;
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
    const table = personTable({ pickable: true, checked, filtering: () => search.trim() !== '', onToggle: () => paint() });
    const list = table.node;
    // The filter sits in its own row above the scrolling list, so it stays put while the
    // operator scrolls through candidates — a filter that scrolls away is unusable exactly
    // when the list is long enough to need filtering.
    const memberToolbar = el('div', { class: 'org-member-toolbar' }, [searchBox, selectedCount]);

    let search = '';
    searchBox.addEventListener('input', () => { search = searchBox.value; paint(); });

    // paint lays the person rows out. The checkbox means "is a member of THIS node", so it is only
    // editable while the whole account list is shown: with a search filter on, "保存成员" would
    // replace the node's membership with whatever subset happens to be visible.
    function paint() {
      const filtering = search.trim() !== '';
      const wanted = state.accounts
        .filter((account) => matchesPerson(account, search))
        // Already-checked accounts come first, so the members of this node stay visible at the
        // top of a long account list; a freshly ticked account jumps there immediately.
        .sort((left, right) => Number(checked.has(right.id)) - Number(checked.has(left.id))
          || left.name.localeCompare(right.name, 'zh-Hans-CN'));
      selectedCount.textContent = checked.size ? '已选 ' + checked.size + ' 个' : '';
      table.render(wanted, state.accounts.length ? '无匹配账号' : '没有可分配的账号', filtering);
    }

    // 新建成员：在这个节点下直接建一个账号并把它挂上来。它是"建人"最快的路径（先有部门，再有人）。
    const createMember = el('button', {
      class: 'btn', text: '新建成员', disabled: readonly,
      title: readonly ? '只读角色不能新建成员' : '在这个节点下新建一个账号，并把它加入本节点',
    });
    createMember.addEventListener('click', () => addMember(node, checked, paint));
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
        readonly
          ? el('span', { class: 'muted', text: '只读角色不能修改' })
          : el('span', { class: 'toolbar-actions' }, [createMember, saveMembers])]),
      memberPanel,
      unassignedRow(unassigned),
      state.accountsTruncated
        ? el('div', { class: 'muted', text: '账号列表已截断（只显示前 ' + MEMBER_PICK_LIMIT + ' 个）；完整列表见账户页按组织筛选' })
        : null,
    ]));

    currentTable = table;
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
      if (open && !box.childElementCount) {
        // No pick column: there is no node to write membership to, so a checkbox would be a lie.
        const plain = personTable({ pickable: false });
        plain.render(unassigned, '没有未归属账号');
        box.append(plain.node);
      }
    });
    line.append(toggle, el('span', { class: 'muted', text: '不在任何节点下的账号：展开后可用「分配组织」把它们挂到节点上' }), box);
    return line;
  }

  // personTable renders the person (account) list as a multi-column table whose rows expand into
  // that account's detail.
  //
  // 为什么是表格：行里原本是"徽标换行排一行"，字段一多就谁也数不清哪一格是谁的；表头 + 定列把
  // 账号 / DSH / 飞书 / Key / 所属组织 / 操作 摆成一眼可扫的列，也才有了"点这一列的按钮"这种说法。
  // pickable 决定勾选列**是否存在**（而不是画出来再用 CSS 藏）：未归属视图没有节点可写。
  //
  // 行对象按 account id 缓存：重绘（勾选、过滤、成员读取完成）不能把展开中的行扔掉，也不该因此
  // 重新发一次 /keys；展开态本身存在 state.open 里，跨整表重建（load()、切换节点）也一样活着。
  function personTable({ pickable, checked = new Set(), filtering = () => false, onToggle = () => {} }) {
    const columns = MEMBER_COLUMNS.filter((col) => pickable || !col.pick);
    const tbody = el('tbody');
    const table = el('table', { class: 'org-member-table' }, [
      el('thead', {}, [el('tr', {}, columns.map((col) => el('th', { class: col.cls, text: col.label })))]),
      tbody,
    ]);
    const node = el('div', { class: 'org-members' + (pickable ? '' : ' org-members-plain') }, [table]);
    const entries = new Map();

    // paintSummary is the one place that fills a row's cells from an account object, so a row that
    // was refreshed in place (a Key was added, a Feishu identity was bound) cannot keep showing the
    // numbers it had when it was built.
    function paintSummary(entry, account) {
      entry.account = account;
      entry.cells.name.replaceChildren(
        el('span', { class: 'org-member-name', text: account.name }),
        el('span', { class: 'org-member-id muted', text: '#' + account.id }));
      entry.cells.dsh.replaceChildren(dshBadge(account));
      entry.cells.feishu.replaceChildren(feishuBadge(account));
      entry.cells.keys.replaceChildren(keyBadge(account));
      entry.cells.orgs.replaceChildren(orgsCell(account));
      entry.box.setAttribute('aria-label', '加入节点：' + account.name);
    }

    // applyBox keeps the membership checkbox in step with `checked` (the server's answer plus
    // whatever the operator ticked since) and with the filter rule.
    function applyBox(entry, isFiltering) {
      const box = entry.box;
      box.checked = checked.has(entry.account.id);
      box.disabled = readonly || (isFiltering && !box.checked);
      // A filtered list is a view, not the node's membership: saving from here would drop the rows
      // the filter hid. Un-ticking a member that is on screen stays allowed — that is a removal the
      // operator can see, and refusing it would make a filtered list read-only.
      box.title = box.disabled
        ? (readonly ? '只读角色不能修改成员' : '过滤时不能再加入成员（列表不完整）：先清空过滤框；已勾选的可以取消')
        : '加入这个节点';
    }

    function buildRow(account) {
      const entry = {
        account, cells: {}, filled: false,
        box: el('input', { type: 'checkbox', title: '加入这个节点' }),
        toggle: el('button', { class: 'btn org-member-toggle', type: 'button', text: '展开', 'aria-expanded': 'false' }),
      };
      entry.box.addEventListener('change', () => {
        if (entry.box.checked) checked.add(account.id); else checked.delete(account.id);
        onToggle();
      });
      entry.toggle.addEventListener('click', () => setOpen(entry, !state.open.has(entry.account.id)));
      entry.cells = {
        pick: el('td', { class: 'c-pick' }, pickable ? [entry.box] : []),
        name: el('td', { class: 'c-name' }),
        dsh: el('td', { class: 'c-dsh' }),
        feishu: el('td', { class: 'c-feishu' }),
        keys: el('td', { class: 'c-keys' }),
        orgs: el('td', { class: 'c-orgs' }),
        ops: el('td', { class: 'c-ops' }, [
          // 「编辑」在「展开」前面：账号级字段（状态/计费/标签/所属组织）是最常改的，
          // Key 与飞书那些动作留在展开区里（M72 口径不变）。
          el('button', {
            class: 'btn btn-small', type: 'button', text: '编辑', disabled: readonly,
            onclick: () => editPerson(entry.account),
          }),
          entry.toggle,
        ]),
      };
      // .org-member is kept on the row itself: the harness and the static regression read rows by
      // that class (and the checkbox/name layout rules key off it), and a row is still a row.
      entry.row = el('tr', {
        class: 'org-person org-member',
        dataset: { accountId: String(account.id) },
      }, columns.map((col) => entry.cells[col.key]));
      entry.body = el('div', { class: 'org-person-detail-body' });
      // 详情行自带 accountId：一张表里每个账号都有一行详情，断言与排障都要能指名道姓地点到某一行。
      entry.detailRow = el('tr', {
        class: 'org-person-detail',
        dataset: { accountId: String(account.id) },
      }, [el('td', { class: 'org-person-detail-cell', colspan: String(columns.length) }, [entry.body])]);
      paintSummary(entry, account);
      return entry;
    }

    function entryFor(account) {
      if (!entries.has(account.id)) entries.set(account.id, buildRow(account));
      return entries.get(account.id);
    }

    // setOpen is the expander. The detail row carries the `open` class and the summary row carries
    // it for the highlight; CSS hides a detail row that is not open (that rule is the fix for
    // "点击收起不会收起" — without it the details stayed visible forever).
    function setOpen(entry, open) {
      if (open) state.open.add(entry.account.id); else state.open.delete(entry.account.id);
      entry.row.classList.toggle('open', open);
      entry.detailRow.classList.toggle('open', open);
      entry.toggle.textContent = open ? '收起' : '展开';
      entry.toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (open) fillDetail(entry);
    }

    async function fillDetail(entry) {
      if (entry.filled) return;
      entry.filled = true; // 连点两次不能发两次 /keys
      entry.body.replaceChildren(el('div', { class: 'muted', text: '正在读取该账号的 Key…' }));
      const account = state.accounts.find((row) => row.id === entry.account.id) || entry.account;
      const children = await personDetail(account, () => refreshAccount(account.id));
      // 整表可能已经重绘：这次结果属于一个已经不在页面上的行，写进去只会留下看不见的垃圾。
      if (!entry.body.isConnected) return;
      entry.body.replaceChildren(...children);
    }

    // render lays out exactly the rows it is given (the caller owns filtering and ordering) and
    // re-applies the checkbox and expansion state to the rows it reuses.
    function render(accounts, emptyText, isFiltering = filtering()) {
      tbody.replaceChildren();
      if (!accounts.length) {
        tbody.append(el('tr', { class: 'org-member-empty' }, [
          el('td', { class: 'empty', colspan: String(columns.length), text: emptyText })]));
        return;
      }
      for (const account of accounts) {
        const entry = entryFor(account);
        paintSummary(entry, account);
        applyBox(entry, isFiltering);
        const open = state.open.has(account.id);
        entry.row.classList.toggle('open', open);
        entry.detailRow.classList.toggle('open', open);
        entry.toggle.textContent = open ? '收起' : '展开';
        entry.toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
        tbody.append(entry.row, entry.detailRow);
        if (open) fillDetail(entry);
      }
    }

    // refreshSummary re-reads one row in place. The actions that cannot change this node's
    // membership (Key operations, binding a Feishu identity) go through this instead of a full
    // reload, so the operator is not thrown back to the top of a long list.
    async function refreshSummary(accountID) {
      const entry = entries.get(accountID);
      const account = state.accounts.find((row) => row.id === accountID);
      if (!entry || !account) return;
      paintSummary(entry, account);
      entry.filled = false;
      if (state.open.has(accountID)) await fillDetail(entry);
    }

    return { node, render, refreshSummary };
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

  // orgsCell answers "where does this account actually live" — the other half of the checkbox
  // column, which only says "member of the node you selected".
  function orgsCell(account) {
    const refs = account.org_nodes || [];
    if (!refs.length) return el('span', { class: 'muted', text: '未归属' });
    return el('span', { class: 'org-member-orgs' }, refs.map((ref) => badge(ref.path || ref.name)));
  }

  // dshBadge states the account's DSH situation in the three values an operator has to tell apart
  // (M72): enabled, explicitly disabled by an administrator, or never enabled.
  function dshBadge(account) {
    if (account.dsh_enabled) return badge('DSH 已启用', 'ok', account.dsh_tenant || '');
    if (account.dsh_disabled_at) return badge('DSH 已停用（管理员）', 'warn', '自动启用不会撤销它');
    if (account.dsh_effective) {
      // 自动启用的前提是"该账号在当前网关有可用模型"：worker 凭据没有模型时首登会被
      // provision_failed 挡下（403），所以这句话必须写在看得见的地方，而不是等人来问。
      return el('span', { class: 'org-dsh-note' }, [
        badge('DSH 登录即可用', 'ok'),
        el('span', { class: 'muted', text: '首次登录自动建租户（需该账号有可用模型）' }),
      ]);
    }
    return el('span', { class: 'muted', text: 'DSH 未启用' });
  }

  function feishuBadge(account) {
    const feishu = account.feishu || {};
    if (!feishu.bound) return el('span', { class: 'muted', text: '未绑飞书' });
    return badge('飞书：' + (feishu.name || feishu.open_id), 'ok', feishu.open_id);
  }

  // keyBadge states how many keys the account has and, when more than one is usable, says out
  // loud that the portal will ask the person to choose: a tooltip alone would hide the one fact
  // that explains the extra login step.
  function keyBadge(account) {
    const active = account.active_key_count || 0;
    const total = account.key_count || 0;
    if (!total) return el('span', { class: 'muted', text: '0 个 Key' });
    const label = active + ' / ' + total + ' 个 Key';
    if (active <= 1) return badge(label);
    return el('span', { class: 'org-key-multi' }, [
      badge(label, 'warn'),
      el('span', { class: 'muted', text: '登录会先选一把 Key' }),
    ]);
  }

  // personDetail builds the expanded half of a person row: the account's own fields, its Key list
  // (fetched here, per account), and the operations that belong to that account. It returns the
  // nodes to put inside the row's .org-person-detail-body — it does NOT build the row itself, so a
  // refresh refills the same body instead of nesting a second panel inside the first.
  async function personDetail(account, refresh) {
    const facts = el('div', { class: 'org-person-facts' }, [
      el('span', { class: 'muted', text: '状态 ' + (account.status || 'active') }),
      el('span', { class: 'muted', text: '计费 ' + (account.billing_mode || '—') }),
      el('span', { class: 'muted', text: '标签 ' + ((account.tags || []).join(', ') || '—') }),
      el('span', { class: 'muted', text: '组织 ' + ((account.org_node_ids || []).length ? (account.org_nodes || []).map((n) => n.path || n.name).join(' / ') : '未归属') }),
    ]);
    const actions = el('div', { class: 'toolbar org-person-actions' }, [
      el('button', { class: 'btn', text: '新建 Key', disabled: readonly, onclick: () => addKey(account, refresh) }),
      el('button', {
        class: 'btn', text: account.dsh_enabled ? '停用 DSH' : '启用 DSH', disabled: readonly,
        onclick: () => toggleDSH(account),
      }),
      el('button', {
        class: 'btn', text: (account.feishu && account.feishu.bound) ? '解绑飞书' : '绑定飞书', disabled: readonly,
        onclick: () => account.feishu && account.feishu.bound ? unbindFeishu(account, refresh) : bindFeishu(account, refresh),
      }),
      el('button', { class: 'btn', text: '分配组织', disabled: readonly, onclick: () => assignOrgs(account) }),
    ]);
    const keysBox = el('div', { class: 'org-person-keys' }, [el('div', { class: 'muted', text: '正在读取 Key…' })]);

    try {
      const payload = await api.get('/keys', { account_id: account.id, limit: 100 });
      const keys = payload.data || [];
      keysBox.replaceChildren();
      if (!keys.length) {
        keysBox.append(el('div', { class: 'muted', text: '这个账号还没有 Key：没有 Key 就无法登录门户（可用「新建 Key」）' }));
        return [facts, actions, keysBox];
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
            onclick: () => editKey(key, refresh),
          }),
          isWorker ? null : el('button', {
            class: 'btn btn-small', text: key.status === 'active' ? '停用' : '启用', disabled: readonly,
            onclick: () => toggleKey(key, refresh),
          }),
        ].filter(Boolean)));
      }
    } catch (err) {
      keysBox.replaceChildren(el('div', { class: 'muted', text: api.errorMessage(err) }));
    }
    return [facts, actions, keysBox];
  }

  // --- 逐账号操作（M72）：人员行与展开行里可用的动作 --------------------------------

  // addMember creates an account and puts it in this node in one step: the membership travels with
  // the POST (`org_node_ids`), so there is no window where the person exists but belongs nowhere.
  //
  // 建完之后**不重新读成员**：那会把操作员还没保存的勾选冲掉（`loadMembers` 会清空并重填 checked），
  // 而且新账号会一瞬间显示成未勾选——此时按「保存成员」反而把它移出节点。这里只把新账号并进
  // checked 再重绘：它按「勾选置顶」规则立刻出现在第一行且是勾上的，其余未保存的勾选原样保留。
  async function addMember(node, checked, repaint) {
    const created = await createAccount({
      title: '新建成员 — ' + node.name,
      submitLabel: '创建',
      // 所属组织预置当前节点（节点是页面已有的对象，直接给出引用，弹窗里就能显示路径）。
      orgRefs: [{ id: node.id, name: node.name, path: node.path || node.name }],
    });
    if (!created) return;
    toast('已新建成员 ' + created.name, 'ok');
    await loadAccounts();
    if (node.id !== state.selectedId) {
      // 节点已经被切走了：整页重载，落到新节点的成员表上。
      await load();
      return;
    }
    checked.add(created.id);
    repaint();
  }

  // editPerson edits the account's own fields from the person row. Unlike the Key/Feishu actions it
  // can change the account's organizations, so it reloads everything and re-reads the memberships:
  // this row's checkbox claims "member of this node", and that claim has to come from the server.
  async function editPerson(account) {
    const updated = await editAccount(account, { title: '编辑账户 ' + account.name });
    if (!updated) return;
    toast('已更新', 'ok');
    await loadAccounts();
    await load();
  }

  // addKey creates a Key for this account. The plaintext is shown once, which is why the shared
  // creator owns the whole sequence rather than this page rebuilding it.
  async function addKey(account, refresh) {
    const created = await createKeyForAccount(account, { name: '' });
    if (created) await refresh();
  }

  // toggleDSH flips the account's DSH switch from the person row. It says the same things as the
  // accounts page, because the consequence is the same: disabling stops the worker and records an
  // explicit disable that dshgw.auto_enable will not undo.
  async function toggleDSH(account) {
    if (account.dsh_enabled) {
      const ok = await confirmDialog('停用 DSH',
        '停用账号 ' + account.name + ' 的 dsh？将停止其 worker 并吊销 worker 专用 Key；新登录被拒绝，' +
        '既有会话按网关 dsh_enforce 档位失效。工作区与 dsh 数据保留，重新启用即恢复。\n' +
        '这是「显式停用」：即使本部署开启了自动启用，也不会在下次登录时自动重新启用它。');
      if (!ok) return;
      try {
        await api.post('/accounts/' + account.id + '/dsh', { enabled: false });
        toast('已停用 DSH', 'ok');
        await reloadAll();
      } catch (err) {
        toast(api.errorMessage(err), 'error');
      }
      return;
    }
    // 预填的两个来源：已有映射优先（`dsh_tenant`，让人一眼看到这次启用不会改名），否则用服务端下发的
    // 规则名（M74：`dsh-<账号拼音>-<账号ID>`）。页面不自己拼名字——规则只有服务端那一份实现。
    const suggested = account.dsh_tenant || account.dsh_tenant_suggested || '';
    const result = await modal({
      title: '启用 DSH — ' + account.name,
      submitLabel: '启用',
      fields: [{ name: 'tenant', label: 'dsh 租户名', value: suggested,
        hint: '小写字母/数字/连字符；留空则沿用既有映射，或按账号名自动生成（' +
          '例：陈景峰 / 10 → dsh-chenjingfeng-10）。已存在的租户名不会被改动' }],
      onSubmit: (values) => api.post('/accounts/' + account.id + '/dsh', {
        enabled: true, ...(values.tenant ? { tenant: values.tenant } : {}),
      }),
    });
    if (!result) return;
    toast('已启用 DSH（租户 ' + (result.tenant || suggested) + '）', 'ok');
    await reloadAll();
  }

  // bindFeishu opens the person picker (M72): the administrator chooses who this account is,
  // which is why binding needs no consent screen any more.
  async function bindFeishu(account, refresh) {
    const bound = await openFeishuPersonPicker({ account });
    // 绑定不改组织归属，所以就地刷新这一行即可（不必把操作员弹回列表顶部）。
    if (bound) await refresh();
  }

  async function unbindFeishu(account, refresh) {
    // 解绑同样不改归属：就地刷新这一行。
    if (await unbindAccountFeishu(account)) await refresh();
  }

  // assignOrgs replaces the account's organization memberships (the same field the accounts page
  // edits). It is how an unassigned account gets a home. 勾选树由 org_assign.js 提供，与账户页同源。
  async function assignOrgs(account) {
    const picked = await openOrgPicker({
      title: '分配组织 — ' + account.name,
      nodeIds: account.org_node_ids || [],
      // 落库由弹窗负责：节点可能刚被别人删掉（404），错误要留在弹窗里让人改，而不是关掉再报。
      onSubmit: (ids) => api.patch('/accounts/' + account.id, { org_node_ids: ids }),
    });
    if (!picked) return;
    toast('组织归属已更新（' + picked.ids.length + ' 个节点）', 'ok');
    // 归属变了 → 整表重载并重读成员勾选。
    await reloadAll();
  }

  // refreshAccount re-reads the accounts and repaints ONE row in place. The actions that cannot
  // change this node's membership (Key 操作、绑定/解绑飞书) go through it: they must update the
  // row's own badges without throwing the operator back to the top of a long member list.
  async function refreshAccount(accountID) {
    await loadAccounts();
    if (currentTable) await currentTable.refreshSummary(accountID);
  }

  // reloadAll re-reads everything. The actions that CAN change the membership (编辑账号、分配组织、
  // 启停 DSH) must go through it: the checkbox column answers "is a member of this node", and a
  // repaint that skipped the membership read would keep claiming a membership the server no longer
  // has — ticking 保存成员 from there would then write it back.
  async function reloadAll() {
    await loadAccounts();
    await load();
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
