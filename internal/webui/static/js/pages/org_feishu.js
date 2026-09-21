// 「同步飞书」弹窗：飞书通讯录（部门树 + 人员）与本地组织架构/账户的合并界面。
//
// 它不是一个路由页面，而是组织架构页右上角按钮打开的对话框，所以只导出 openFeishuSync()
// 并由 pages/org.js import —— 控制台没有构建步骤，模块就是文件，多一个页面文件就多一条
// 需要维护的路由表项。
//
// 这个弹窗只做两件事：（1）把服务端的合并预览画出来（每个部门标 已存在/将创建/同名合并，
// 每个人标匹配通道或未匹配）；（2）把操作员的决定发回去（同步 / 创建用户 / 绑定账号 / 解绑）。
// 匹配规则本身全在服务端（internal/httpapi/admin_org_feishu.go 的 planFeishuOrg），前端不
// 复制一份，避免出现"界面说会合并、同步却不动"的分歧。

import { api } from '../api.js';
import { el, modalHead, modalBody, modalActions, modal, confirmDialog, toast, badge, withBusy } from '../ui.js';
import { tree } from '../tree.js';
import { matchesQuery } from '../pinyin.js';

// 账号选择列表一次拉这么多，与组织页的成员列表同一口径（超过就截断并提示去账户页）。
const ACCOUNT_PICK_LIMIT = 1000;

// 飞书把公司本身当作一个虚拟部门 "0"：它不是部门（没有名字），但它的人员要有人管，
// 所以树里给它一个合成根节点。
const ROOT_ID = '0';

// 人员列表里标记的匹配通道。界面用中文，接口用 matched_by 的英文值。
const CHANNEL_LABEL = {
  open_id: '人员id已绑定',
  api_key: 'Key 绑定转账户',
  name: '同名自动合并',
  manual: '手工绑定',
};

export function openFeishuSync({ onDone } = {}) {
  const state = {
    payload: null,
    selectedDept: ROOT_ID,
    includeChildren: true,
    query: '',
    loading: false,
    changed: false,
  };

  const subtitle = el('span', { class: 'muted feishu-sync-subtitle', text: '正在读取飞书通讯录…' });
  const notice = el('div', { class: 'feishu-sync-notice' });
  const treeHost = el('div', { class: 'feishu-sync-tree' });
  const peopleHost = el('div', { class: 'feishu-sync-people' });

  // The toolbar of the people pane is built ONCE: rebuilding it per render would replace the
  // filter box on every keystroke and drop the caret, which is exactly how a filter becomes
  // unusable on a long list.
  const peopleTitle = el('h4', { text: '人员' });
  const peopleCount = el('span', { class: 'muted' });
  const includeBox = el('input', { type: 'checkbox', id: 'feishu-include-children' });
  includeBox.checked = state.includeChildren;
  includeBox.addEventListener('change', () => { state.includeChildren = includeBox.checked; renderPeople(); });
  const peopleSearch = el('input', { type: 'search', placeholder: '按姓名过滤（支持拼音，如 ranqiliang）…' });
  peopleSearch.addEventListener('input', () => { state.query = peopleSearch.value; renderPeople(); });
  const peopleList = el('div', { class: 'feishu-user-list' });
  peopleHost.append(
    el('div', { class: 'feishu-people-head' }, [peopleTitle]),
    el('div', { class: 'feishu-people-toolbar' }, [
      el('label', { class: 'feishu-inline-check' }, [includeBox, el('span', { text: '包含子部门' })]),
      peopleSearch, peopleCount,
    ]),
    peopleList,
  );

  const refreshBtn = el('button', { class: 'btn', text: '刷新' });
  const syncBtn = el('button', { class: 'btn btn-primary', text: '同步', disabled: true });
  const close = () => { backdrop.remove(); if (onDone) onDone(state.changed); };

  const dialog = el('div', { class: 'modal feishu-sync-dialog' }, [
    modalHead('同步飞书组织架构', close),
    modalBody([
      el('div', { class: 'toolbar feishu-sync-head' }, [subtitle, refreshBtn, syncBtn]),
      notice,
      el('div', { class: 'feishu-sync-layout' }, [treeHost, peopleHost]),
    ]),
    modalActions([el('button', { class: 'btn', text: '关闭', onclick: close })]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(); });
  document.getElementById('modal-root').append(backdrop);

  const mainTree = tree({
    mode: 'workspace',
    filter: true,
    filterPlaceholder: '过滤部门名（支持拼音）…',
    matcher: matchesQuery,
    emptyText: '飞书通讯录里没有部门',
    renderLabel: (node) => node.name,
    renderMeta: (node) => departmentMeta(node),
    onSelect: (node) => { state.selectedDept = node ? node.id : ROOT_ID; renderPeople(); },
  });
  treeHost.append(mainTree.node);

  refreshBtn.addEventListener('click', () => load(true));
  syncBtn.addEventListener('click', runSync);

  load(false);

  // --- data ---------------------------------------------------------------

  async function load(refresh) {
    if (state.loading) return;
    state.loading = true;
    refreshBtn.disabled = true;
    try {
      const payload = await api.get('/org/feishu/directory', refresh ? { refresh: 'true' } : undefined);
      state.payload = payload;
      render();
    } catch (err) {
      state.payload = null;
      subtitle.textContent = '读取飞书通讯录失败';
      notice.className = 'feishu-sync-notice error';
      notice.textContent = api.errorMessage(err);
      treeHost.replaceChildren();
      peopleHost.replaceChildren();
    } finally {
      state.loading = false;
      refreshBtn.disabled = false;
    }
  }

  // render paints the whole dialog from one payload. It is called after every write too,
  // which is why it rebuilds the tree instead of patching rows.
  function render() {
    const payload = state.payload;
    if (!payload) return;
    const stats = payload.stats || {};
    const cached = payload.cached ? '（60 秒缓存）' : '';
    subtitle.textContent = '部门 ' + stats.departments + '（将创建 ' + stats.departments_to_create +
      ' · 同名打标 ' + stats.departments_to_pin + '）· 人员 ' + stats.users +
      '（可自动合并 ' + stats.users_matched + ' · 待决定 ' + stats.users_unmatched +
      ' · 已同步 ' + stats.users_already_synced + '）' + cached;

    const warnings = payload.warnings || [];
    notice.className = 'feishu-sync-notice' + (warnings.length ? ' warn' : '');
    if (warnings.includes('names_unavailable')) {
      notice.textContent = '飞书应用缺少「获取部门基础信息 / 获取用户基本信息」数据权限，' +
        '因此部门与人员没有名称（接口本身是成功的）：部门/人员只能按 od-/ou- 编号辨认，' +
        '创建账户需要手填名字，「同步」不会用编号创建部门。在飞书开放平台加上这两个权限并发布新版本后即可。';
    } else if (warnings.includes('directory_truncated')) {
      notice.textContent = '飞书通讯录超出单次读取上限，结果已截断：先处理已读到的部分，或分批同步。';
    } else if (payload.users_truncated) {
      notice.textContent = '人员列表显示已截断（仅前 ' + (payload.users || []).length + ' 人）。';
    } else {
      notice.textContent = '';
    }
    notice.hidden = notice.textContent === '';

    mainTree.refresh(treeNodes());
    mainTree.setSelected(state.selectedDept);
    syncBtn.disabled = false;
    renderPeople();
  }

  // treeNodes builds the control's flat node list: a synthetic root for the people who sit
  // directly under the Feishu company node, then every department as it came (BFS order,
  // depth+1 because the control's own depth starts at 0 for the root).
  function treeNodes() {
    const users = state.payload.users || [];
    const rootUsers = users.filter((person) => !(person.department_ids || []).length).length;
    const nodes = [{
      id: ROOT_ID,
      parent_id: null,
      name: '飞书根组织',
      depth: 0,
      user_count: rootUsers,
      local: { matched: 'root' },
    }];
    for (const department of state.payload.departments || []) {
      nodes.push({
        id: department.id,
        parent_id: department.parent_id || ROOT_ID,
        name: department.name || department.id,
        depth: (department.depth || 0) + 1,
        user_count: department.direct_user_count || 0,
        local: department.local || {},
      });
    }
    return nodes;
  }

  function departmentMeta(node) {
    const parts = [];
    if (node.user_count) parts.push(node.user_count + ' 人');
    const local = node.local || {};
    if (node.id === ROOT_ID) parts.push('公司根节点');
    else if (local.will_create) parts.push('将创建');
    else if (local.name_conflict) parts.push('同名节点已属其它部门');
    else if (local.matched === 'id') parts.push('已关联');
    else if (local.matched === 'name') parts.push('同名节点');
    else if (local.skipped) parts.push('跳过（无名称或层级过深）');
    return el('span', { class: 'org-meta', text: parts.join(' · ') });
  }

  // --- people list --------------------------------------------------------

  function peopleInScope() {
    const payload = state.payload;
    if (!payload) return [];
    const wanted = descendantDepartments(state.selectedDept);
    return (payload.users || []).filter((person) => {
      const ids = person.department_ids || [];
      // The synthetic root stands for the company itself: with 包含子部门 on it means
      // "everyone", including the people Feishu lists directly under the company (who
      // belong to no department at all). With it off, only those company-level people.
      if (state.selectedDept === ROOT_ID) return state.includeChildren ? true : ids.length === 0;
      return ids.some((id) => wanted.has(id));
    });
  }

  // descendantDepartments walks the department list the server already ordered BFS, so one
  // pass is enough to collect a subtree without a graph structure of its own.
  function descendantDepartments(id) {
    if (id === ROOT_ID) {
      return new Set((state.payload.departments || []).map((department) => department.id).concat(['']));
    }
    if (!state.includeChildren) return new Set([id]);
    const children = new Map();
    for (const department of state.payload.departments || []) {
      const parent = department.parent_id || ROOT_ID;
      if (!children.has(parent)) children.set(parent, []);
      children.get(parent).push(department.id);
    }
    const seen = new Set([id]);
    const queue = [id];
    while (queue.length) {
      const current = queue.shift();
      for (const child of children.get(current) || []) {
        if (seen.has(child)) continue;
        seen.add(child);
        queue.push(child);
      }
    }
    return seen;
  }

  function renderPeople() {
    if (!state.payload) return;
    const people = peopleInScope()
      .filter((person) => matchesQuery(person.name || person.open_id, state.query));
    peopleTitle.textContent = departmentTitle(state.selectedDept);
    peopleCount.textContent = people.length + ' 人';
    peopleList.replaceChildren();
    if (!people.length) {
      peopleList.append(el('div', { class: 'empty', text: '该部门下没有人员（或都被过滤掉了）' }));
      return;
    }
    for (const person of people) peopleList.append(personRow(person));
  }

  function departmentTitle(id) {
    if (id === ROOT_ID) return '飞书根组织下的人员';
    const department = (state.payload.departments || []).find((row) => row.id === id);
    if (!department) return '人员';
    const local = department.local || {};
    const suffix = local.will_create ? '（本地将创建）' : local.matched ? '（本地已存在）' : '';
    return (department.name || id) + suffix;
  }

  function personRow(person) {
    const name = el('span', { class: 'feishu-user-name', text: person.name || person.open_id });
    const meta = el('span', { class: 'feishu-user-meta muted', text: person.name ? person.open_id : '' });
    const cells = [el('div', { class: 'feishu-user-id' }, [name, meta])];

    const actions = el('div', { class: 'feishu-user-actions' });
    if (person.account) {
      cells.push(el('span', { class: 'feishu-user-account' }, [
        badge(CHANNEL_LABEL[person.account.matched_by] || person.account.matched_by || '已匹配', 'ok'),
        el('span', { text: '→ ' + person.account.name + ' #' + person.account.id }),
      ]));
      if (person.join_nodes && person.join_nodes.length) {
        cells.push(el('span', { class: 'feishu-user-nodes muted', text: '挂入 ' + person.join_nodes.length + ' 个节点' }));
      }
      actions.append(el('button', {
        class: 'btn btn-danger', text: '解绑',
        onclick: () => unbind(person),
      }));
    } else {
      cells.push(el('span', { class: 'feishu-user-account muted', text: '未匹配，请决定' }));
      actions.append(
        el('button', { class: 'btn', text: '创建用户', onclick: () => createUser(person) }),
        el('button', { class: 'btn btn-primary', text: '绑定账号', onclick: () => bindAccount(person) }),
      );
    }
    cells.push(actions);
    return el('div', { class: 'feishu-user' }, cells);
  }

  // --- writes -------------------------------------------------------------

  async function runSync() {
    const stats = state.payload ? state.payload.stats || {} : {};
    const ok = await confirmDialog('执行一次飞书同步',
      '将创建 ' + (stats.departments_to_create || 0) + ' 个组织节点（名称即部门名），' +
      '并把自动匹配上的 ' + (stats.users_matched || 0) + ' 人写入飞书身份、挂进其部门节点' +
      '（新增成员关系 ' + (stats.memberships_to_add || 0) + ' 条）。' +
      '组织节点的标签会被整棵子树继承，所以这会立即改变这些账号的生效授权。' +
      '已存在/已匹配的内容不会重复写入，重复点「同步」是安全的。');
    if (!ok) return;
    try {
      const result = await withBusy(syncBtn, '同步中', () => api.post('/org/feishu/sync'));
      const created = (result.created_nodes || []).length;
      const linked = (result.linked_users || []).length;
      const skipped = (result.skipped_departments || []).length + (result.skipped_users || []).length;
      toast('同步完成：新建节点 ' + created + ' 个，合并人员 ' + linked + ' 人' +
        (skipped ? '，跳过 ' + skipped + ' 项（见弹窗与审计）' : ''), 'ok');
      state.changed = true;
      for (const row of (result.skipped_departments || []).concat(result.skipped_users || [])) {
        toast('跳过 ' + (row.name || row.id) + '：' + skipReason(row.reason), 'error');
      }
      await load(true);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  async function createUser(person) {
    const created = await modal({
      title: '为飞书人员创建本地账户',
      fields: [
        { name: 'name', label: '账户名', required: true, value: person.name || '',
          hint: person.name ? '默认用飞书姓名；已存在同名账户时会被拒绝（改用「绑定账号」）'
            : '通讯录没有名称（缺数据权限），必须手填' },
        { name: 'note', label: '备注' },
      ],
      submitLabel: '创建',
      onSubmit: (values) => api.post('/org/feishu/users/' + encodeURIComponent(person.open_id) + '/account',
        { name: values.name, note: values.note }),
    });
    if (!created) return;
    toast('已创建账户「' + created.account.name + '」并绑定飞书身份', 'ok');
    state.changed = true;
    await load(true);
  }

  // bindAccount opens the account picker: a second dialog stacked on this one, because the
  // decision belongs to one person (whose row the operator clicked) and needs the account
  // list in front of it.
  async function bindAccount(person) {
    let accounts = [];
    try {
      const payload = await api.get('/accounts', { limit: ACCOUNT_PICK_LIMIT });
      accounts = payload.data || [];
    } catch (err) {
      toast(api.errorMessage(err), 'error');
      return;
    }
    // Which accounts already carry a Feishu identity: the preview knows every account a
    // directory person is matched to, which is exactly the set that must not be stolen.
    const takenBy = new Map();
    for (const row of (state.payload.users || [])) {
      if (row.account) takenBy.set(row.account.id, row.open_id);
    }

    const chosen = { id: null };
    const sameName = accounts.filter((account) => account.name === person.name);
    const preferred = sameName.find((account) => !takenBy.has(account.id));
    if (preferred) chosen.id = preferred.id;

    const list = el('div', { class: 'feishu-picker-list' });
    const filter = el('input', { type: 'search', placeholder: '按账号名过滤（支持拼音，如 ranqiliang）…' });

    function paint() {
      list.replaceChildren();
      const wanted = accounts
        .filter((account) => matchesQuery(account.name, filter.value))
        .sort((left, right) => Number(right.name === person.name) - Number(left.name === person.name)
          || Number(takenBy.has(left.id)) - Number(takenBy.has(right.id))
          || left.name.localeCompare(right.name, 'zh-Hans-CN'));
      if (!wanted.length) {
        list.append(el('div', { class: 'empty', text: '无匹配账号' }));
        return;
      }
      for (const account of wanted) {
        const boundTo = takenBy.get(account.id);
        const usable = !boundTo || boundTo === person.open_id;
        const row = el('label', { class: 'feishu-picker-row' + (usable ? '' : ' disabled') }, [
          el('input', { type: 'radio', name: 'feishu-account', disabled: !usable }),
          el('span', { class: 'feishu-picker-name', text: account.name }),
          el('span', { class: 'feishu-picker-id muted', text: '#' + account.id }),
          boundTo
            ? el('span', { class: 'muted', text: usable ? '已绑定该人员' : '已绑定其他飞书身份' })
            : (account.name === person.name ? badge('同名', 'ok') : null),
        ]);
        const box = row.querySelector('input');
        box.checked = chosen.id === account.id;
        box.addEventListener('change', () => { chosen.id = account.id; });
        list.append(row);
      }
    }
    filter.addEventListener('input', paint);
    paint();

    const ok = await new Promise((resolve) => {
      const closePicker = (value) => { picker.remove(); resolve(value); };
      const picker = el('div', { class: 'modal-backdrop' }, [
        el('div', { class: 'modal feishu-picker-dialog' }, [
          modalHead('绑定账号：' + (person.name || person.open_id), () => closePicker(false)),
          modalBody([
            el('div', { class: 'muted', text: '选择要把这个飞书人员绑定到哪个本地账户。绑定只写账户级飞书身份（同步映射），不影响 API Key 与门户登录。' }),
            filter,
            list,
          ]),
          modalActions([
            el('button', { class: 'btn', text: '取消', onclick: () => closePicker(false) }),
            el('button', {
              class: 'btn btn-primary', text: '绑定',
              onclick: (ev) => {
                if (chosen.id === null) {
                  toast('请先选择一个账户', 'error');
                  return;
                }
                const button = ev.currentTarget;
                withBusy(button, '绑定中', () => api.put(
                    '/org/feishu/users/' + encodeURIComponent(person.open_id) + '/account',
                    { account_id: chosen.id })).then(() => {
                  toast('已绑定到账户 #' + chosen.id, 'ok');
                  state.changed = true;
                  closePicker(true);
                }).catch((err) => {
                  toast(api.errorMessage(err), 'error');
                });
              },
            }),
          ]),
        ]),
      ]);
      picker.addEventListener('click', (ev) => { if (ev.target === picker) closePicker(false); });
      document.getElementById('modal-root').append(picker);
    });
    if (ok) await load(true);
  }

  async function unbind(person) {
    const ok = await confirmDialog('解除飞书绑定',
      '确认解除「' + (person.name || person.open_id) + '」与账户' +
      (person.account ? '「' + person.account.name + '」' : '') + '的绑定吗？' +
      '这只是解除账户级同步映射：账户、它的 API Key 以及门户登录都不受影响。');
    if (!ok) return;
    try {
      const result = await api.del('/org/feishu/users/' + encodeURIComponent(person.open_id) + '/account');
      toast(result.unbound ? '已解绑' : '本来就没有绑定', result.unbound ? 'ok' : '');
      state.changed = true;
      await load(true);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }
}

function skipReason(reason) {
  switch (reason) {
    case 'sibling_name_exists': return '同一父节点下已有同名节点';
    case 'node_pinned_to_other_department': return '该节点已关联另一个飞书部门';
    case 'identity_taken': return '该账户已被另一个飞书身份绑定';
    default: return reason || '未知原因';
  }
}
