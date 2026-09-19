import { api, authMethods } from '../api.js';
import { el, card, pagedTable, modal, toast, badge, statusBadge, formatTime, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';

// 控制台管理员（M66）：多管理员 + 飞书扫码登录。
//
// 这一页与飞书是解耦的（docs/design/m66 §D8）：飞书管理员登录没开时，它就是一个普通的多管理员
// 管理页（建号 / 改角色与状态 / 重置口令 / 删除），只有「邀请链接」和「解绑飞书」两个动作需要
// 飞书——它们出不出现由公开接口 /auth/methods 的回答决定，而不是前端猜配置。
//
// route/navigate 照 shell 的契约一并接收：这一页的每个动作都停在原地（唯一离开控制台的跳转在
// 登录页的飞书入口，那里必须是整页导航）。

const ROLES = [
  { value: 'admin', label: 'admin — 管理员：可读写全部管理接口' },
  { value: 'viewer', label: 'viewer — 只读：能看控制台，不能改任何东西' },
];

const STATUS_LABELS = {
  active: 'active — 可以登录',
  pending: 'pending — 还没有可用凭据，等邀请激活',
  disabled: 'disabled — 已停用，不能登录',
};

const BOOTSTRAP_TITLE = '由 bootstrap.admin 配置在启动时创建/重建：不能删除，口令会在重启时被配置覆盖';

export async function render({ page, actions, session, route, navigate }) {
  const readonly = session.role !== 'admin';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  const create = el('button', { class: 'btn btn-primary', text: '新建管理员', disabled: readonly });
  actions.append(refresh, create);

  // 问服务端要不要显示飞书相关动作，不猜配置。问不到（老服务端、网络故障）就当没开：
  // 这一页在没有飞书的部署里必须照常可用。
  const feishuEnabled = await detectFeishu();

  const view = pagedTable({
    columns: [
      { key: 'id', label: 'ID' },
      { key: 'username', label: '用户名', render: (row) => usernameCell(row) },
      { key: 'role', label: '角色', render: (row) => roleCell(row) },
      { key: 'status', label: '状态', render: (row) => statusCell(row) },
      // 飞书身份是这个账号的第二种登录方式：谁绑了、绑的是谁，是这一页最常被问的一件事。
      { key: 'feishu', label: '飞书', render: (row) => feishuCell(row) },
      { key: 'last_login_at', label: '最近登录', render: (row) => formatTime(row.last_login_at) },
      { key: 'created_at', label: '创建时间', render: (row) => formatTime(row.created_at) },
    ],
    rowActions: (row) => readonly ? [] : [
      ...(feishuEnabled
        ? [el('button', { class: 'btn', text: '邀请链接', onclick: () => invite(row, () => view.refresh()) })]
        : []),
      el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, () => view.refresh()) }),
      el('button', { class: 'btn', text: '重置口令', onclick: () => resetPassword(row, () => view.refresh()) }),
      ...(feishuEnabled && row.feishu && row.feishu.bound
        ? [el('button', { class: 'btn', text: '解绑飞书', onclick: () => unbindFeishu(row, () => view.refresh()) })]
        : []),
      deleteButton(row, session.username, () => view.refresh()),
    ],
    // limit/offset 原样交给服务端，这一页不在浏览器里切页。
    load: async ({ limit, offset }) => {
      const payload = await api.get('/admin-users', { limit, offset });
      // 列表信封的字段名是 data（服务端的 listPayload），资源文档里写的是 items：
      // 两种都认，免得字段名对不上时页面安静地渲染成空表。
      return { ...payload, data: payload.items || payload.data || [] };
    },
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });

  const hint = readonly
    ? '只读角色（viewer）可以查看管理员名单，但不能新建、修改或删除任何账号'
    : '管理员账号用来登录这个控制台：口令只在重置时显示一次；没设口令的账号要靠邀请链接绑定飞书后才能登录';
  page.append(card('管理员', view.node, [el('span', { class: 'muted', text: hint })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', () => modal({
    title: '新建管理员',
    submitLabel: '创建',
    fields: [
      { name: 'username', label: '用户名', required: true, hint: '[A-Za-z0-9._-]，≤64 字符；全局唯一' },
      { name: 'role', label: '角色', type: 'select', options: ROLES, value: 'admin' },
      { name: 'password', label: '初始口令', type: 'password', hint: '留空则账号为 pending，需要用邀请链接激活' },
    ],
    onSubmit: async (values) => {
      const created = await api.post('/admin-users', values);
      toast(created && created.status === 'pending'
        ? '已创建 ' + created.username + '：账号待激活，生成邀请链接发给本人即可'
        : '已创建 ' + ((created && created.username) || values.username) + '：现在就能用这个口令登录', 'ok');
      await view.refresh();
      return created;
    },
  }));

  await view.refresh();
}

// detectFeishu asks the public endpoint which ways in this deployment offers. A failure is
// not something the operator can act on: it means "no Feishu entry here", and every other
// action on the page works without it.
async function detectFeishu() {
  try {
    const payload = await authMethods();
    return !!(payload && payload.feishu && payload.feishu.enabled);
  } catch (err) {
    return false;
  }
}

// 角色与状态都用徽章，绿色只留给"能改东西的管理员"和"能登录的账号"：
// viewer / pending / disabled 一眼看上去就不该是绿的。
function roleCell(row) {
  const isAdmin = row.role === 'admin';
  return withTitle(badge(row.role || '未知角色', isAdmin ? 'ok' : ''),
    isAdmin ? 'admin：可以读写全部管理接口' : 'viewer：只读，能看控制台但不能修改任何东西');
}

function statusCell(row) {
  if (row.status === 'pending') {
    return withTitle(badge('pending', 'warn'),
      'pending：还没有可用凭据（口令未设置），需要用邀请链接绑定飞书后才能登录');
  }
  if (row.status === 'disabled') {
    return withTitle(badge('disabled', 'danger'),
      'disabled：已停用，不能登录；停用时该账号已登录的会话就已经被注销');
  }
  const node = statusBadge(row.status);
  return row.status === 'active' ? withTitle(node, 'active：可以登录这个控制台') : node;
}

function usernameCell(row) {
  const parts = [row.username];
  if (row.bootstrap) {
    parts.push(' ', withTitle(badge('bootstrap'), BOOTSTRAP_TITLE));
  }
  if (row.invite_pending) {
    parts.push(' ', withTitle(badge('邀请待用', 'warn'), '已生成邀请链接但还没被使用；重新生成会让上一条立刻失效'));
  }
  return el('span', {}, parts);
}

// feishuCell 与 API Keys 页的同一列同形：单元格里放人认得出的名字，完整身份放进 title
// （比对两个账号时，open_id 才是决定性的那个）。
function feishuCell(row) {
  const feishu = row.feishu || {};
  if (!feishu.bound) return el('span', { class: 'muted', text: '未绑定' });
  const label = feishu.name || feishu.open_id || '已绑定';
  const title = [feishu.open_id, feishu.union_id,
    feishu.bound_by ? '由 ' + feishu.bound_by + ' 绑定' : '',
    feishu.bound_at ? '绑定于 ' + formatTime(feishu.bound_at) : ''].filter(Boolean).join(' · ');
  return el('span', { class: 'badge', text: label, title: title });
}

function withTitle(node, title) { node.title = title; return node; }

// deleteButton 在"自己这一行"上直接禁用：服务端会 409，一个按下去只会失败的按钮应该先把
// 原因写在 title 里，而不是让人按了才知道。
function deleteButton(row, username, reload) {
  const own = row.username === username;
  return el('button', {
    class: 'btn btn-danger', text: '删除',
    disabled: own,
    title: own
      ? '这是你当前登录的账号，不能删除自己：请让另一名管理员来操作'
      : '删除这个管理员账号；他在控制台的智能问答会话与技能库会一并删除',
    onclick: () => remove(row, reload),
  });
}

async function invite(row, reload) {
  let result;
  try {
    // 501（部署没开飞书管理员登录）与 409（账号已停用或还没启用）里服务端说得更准确，
    // 原样交给操作者，而不是在这里重写一遍。
    result = await api.post('/admin-users/' + row.id + '/invite');
  } catch (err) {
    toast(api.errorMessage(err), 'error');
    return;
  }
  await reload();
  revealDialog({
    title: '邀请链接：' + result.username,
    intro: '把链接发给本人，在对方自己的浏览器里打开：完成飞书授权后会绑定这个飞书身份，并直接以 ' +
      result.username + ' 的身份进入控制台。',
    value: result.url,
    notes: [
      '有效期约 ' + humanDuration(result.expires_in_s) + '（至 ' + formatTime(result.expires_at) + '）。' +
        '链接只能成功兑换一次：用过后再打开会显示「已失效」；重新生成会立即作废上一条。',
      result.note,
    ],
  });
}

async function edit(row, reload) {
  await modal({
    title: '编辑 ' + row.username,
    fields: [
      { name: 'role', label: '角色', type: 'select', options: ROLES, value: row.role },
      { name: 'status', label: '状态', type: 'select', options: statusOptions(row.status), value: row.status,
        hint: 'disabled 会立即停用并注销该账号已登录的会话；改回 active 要求账号已经有口令或飞书绑定' },
    ],
    onSubmit: async (values) => {
      await api.patch('/admin-users/' + row.id, { role: values.role, status: values.status });
      toast('已更新 ' + row.username, 'ok');
      await reload();
      return true;
    },
  });
}

// statusOptions 至少给出 active/disabled 两项；pending 的行要把 pending 也列出来，
// 否则下拉框会显示成第一项（active），一保存就变成"启用一个没有凭据的账号"的 409。
function statusOptions(status) {
  const values = status === 'pending' ? ['pending', 'active', 'disabled'] : ['active', 'disabled'];
  return values.map((value) => ({ value: value, label: STATUS_LABELS[value] || value }));
}

async function resetPassword(row, reload) {
  const ok = await confirmDialog('重置口令',
    '确认重置 ' + row.username + ' 的口令吗？这会立即注销该账号已登录的会话，新口令只显示这一次。');
  if (!ok) return;
  let result;
  try {
    result = await api.post('/admin-users/' + row.id + '/password');
  } catch (err) {
    toast(api.errorMessage(err), 'error');
    return;
  }
  await reload();
  revealDialog({
    title: '新口令：' + result.username,
    intro: '新口令只显示这一次：关闭后无法再次获取，请现在保存，并交给本人。',
    value: result.password,
    notes: [
      '该账号已登录的会话已被注销（sessions_revoked=true），需要用新口令重新登录。',
      result.bootstrap ? '这个账号由 bootstrap.admin 配置在启动时重建：下次启动时它的口令会被配置里的口令覆盖。' : '',
    ],
  });
}

async function unbindFeishu(row, reload) {
  const feishu = row.feishu || {};
  const who = feishu.name || feishu.open_id || '该飞书身份';
  const ok = await confirmDialog('解绑飞书',
    '确认解除 ' + row.username + ' 与 ' + who + ' 的绑定吗？解绑后该账号不能再扫码登录' +
    (row.has_password ? '（口令登录不受影响）。' : '；它没有口令，会退回 pending 并立即失去控制台会话。'));
  if (!ok) return;
  try {
    const result = await api.del('/admin-users/' + row.id + '/feishu');
    toast(result && result.unbound ? '已解绑' : '本来就没有绑定', 'ok');
  } catch (err) {
    toast(api.errorMessage(err), 'error');
  }
  await reload();
}

async function remove(row, reload) {
  const ok = await confirmDialog('删除管理员',
    '确认删除管理员 ' + row.username + ' 吗？会一并删除该管理员在控制台的智能问答会话与技能库，且无法恢复。');
  if (!ok) return;
  try {
    await api.del('/admin-users/' + row.id);
    toast('已删除 ' + row.username, 'ok');
  } catch (err) {
    // 409：自己的账号、bootstrap 配置重建的账号、最后一名可用管理员——服务端的消息就是
    // 该怎么做，原样显示。
    toast(api.errorMessage(err), 'error');
  }
  await reload();
}

// revealDialog 展示一个"只出现一次"的值（一次性口令、邀请链接）：只读字段打开即选中
// （Ctrl+C 不用鼠标），配一个复制按钮，以及关闭前必须知道的事。与 keys.js 的明文对话框
// 同一个理由：值只有这一次。
function revealDialog({ title, intro, value, notes }) {
  const box = el('input', { value: value, readonly: true });
  const copy = el('button', { class: 'btn', text: '复制' });
  const done = el('button', { class: 'btn btn-primary', text: '我已保存' });
  const body = [el('p', { class: 'muted', text: intro }), box];
  for (const note of notes || []) {
    if (note) body.push(el('p', { class: 'muted', text: note }));
  }
  const dialog = el('div', { class: 'modal' }, [
    modalHead(title, () => backdrop.remove()),
    modalBody(body),
    modalActions([copy, done]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  copy.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(value); toast('已复制', 'ok'); }
    catch (err) { box.select(); document.execCommand('copy'); }
  });
  done.addEventListener('click', () => backdrop.remove());
  document.getElementById('modal-root').append(backdrop);
  box.select();
}

// 有效期说成人读得懂的单位（服务端给的是秒）：3600 秒是「约 1 小时」。
function humanDuration(seconds) {
  const total = Number(seconds);
  if (!Number.isFinite(total) || total <= 0) return '未知';
  if (total < 3600) return Math.max(1, Math.round(total / 60)) + ' 分钟';
  if (total < 86400) return Math.round(total / 3600) + ' 小时';
  return Math.round(total / 86400) + ' 天';
}
