// 为账号选择飞书人员（M72）：绑定入口从「Key + 扫码」改成「账号 + 选人」。
//
// 数据直接来自组织页那份飞书通讯录（GET /org/feishu/directory，服务端 60 秒缓存）：它已经
// 带着每个人当前的归属（`account`），所以「这个人是不是已经绑到别的账号」不需要再问一次接口，
// 也就不会出现「弹窗里能选、点下去 409」的错位。写绑定走账号优先的
// `PUT /accounts/{id}/feishu`：只写账号级身份，不要求这个人仍在通讯录里（离职者也要能清理），
// 也不顺带改动账号的组织归属。
//
// 顺序很重要：**先开弹窗、再读通讯录**。这个读冷缓存时要走飞书接口（遍历部门 + 逐人用户信息），
// 几秒到十几秒都可能；先 await 再建弹窗的写法会让"点了绑定飞书"在整段时间里像没反应一样——
// 用户报障的原话就是"加载人员要先弹出框、显示进度"。读失败同理：框已经在了，就在框里说清楚并能重试。
import { api } from '../api.js';
import { el, toast, badge, modalHead, modalBody, modalActions, withBusy, confirmDialog, progressLine } from '../ui.js';
import { matchesQuery } from '../pinyin.js';

// 人员列表上限：与组织页的账号列表同口径，一次拉全量比做一套分页更简单。
const DIRECTORY_LIMIT = 2000;

// openFeishuPersonPicker 让管理员为某个账号选一个飞书人员。返回 true 表示绑定已写入。
export function openFeishuPersonPicker({ account, onBound } = {}) {
  let people = [];
  // 谁已经绑到哪个账号：通讯录预览的 `account` 就是服务端的合并结果。
  let boundAccount = new Map();
  let chosen = null;
  let loading = false;
  let progress = null;
  // close 由下面的 Promise 赋值：绑定按钮的处理函数写在 Promise 之外（它要能提前注册），
  // 所以不能把 close 定义在 executor 里——那样按钮里引用到的是一个不存在的名字（第一版就是这样，
  // 绑定成功后弹窗不关，并且抛出一个没人接的 ReferenceError）。
  let close = () => {};

  const status = el('div', { class: 'feishu-picker-status' });
  const notice = el('div', { class: 'feishu-picker-notice' });
  const filter = el('input', { type: 'search', placeholder: '按姓名过滤（支持拼音，如 lizhichao）…' });
  const list = el('div', { class: 'feishu-picker-list' });
  const current = el('span', { class: 'muted' });
  // 「刷新」走 refresh=true：绕过服务端那 60 秒缓存。刚在飞书里加的人要能立刻看到；读失败时
  // 它就是重试按钮。
  const refreshBtn = el('button', { class: 'btn', text: '刷新', title: '重新读取飞书通讯录（跳过 60 秒缓存）' });
  const bindBtn = el('button', { class: 'btn btn-primary', text: '绑定', disabled: true });

  refreshBtn.addEventListener('click', () => load(true));
  bindBtn.addEventListener('click', () => {
    if (!chosen) { toast('请先选择一个人', 'error'); return; }
    withBusy(bindBtn, '绑定中', () => api.put('/accounts/' + account.id + '/feishu', {
      open_id: chosen.open_id, union_id: chosen.union_id || '', name: chosen.name || '',
    })).then((result) => {
      toast(result && result.result === 'replaced' ? '已改绑（原身份已解除）' : '已绑定飞书身份', 'ok');
      if (onBound) onBound(chosen);
      close(true);
    }).catch((err) => {
      toast(api.errorMessage(err), 'error');
    });
  });
  filter.addEventListener('input', paint);

  // 已被别的账号占用的人不可选：绑定接口会 409，弹窗里先说清楚比让人点一次再报错好。
  function isUsable(person) {
    const bound = boundAccount.get(person.open_id);
    return !bound || bound.id === account.id;
  }

  function paint() {
    list.replaceChildren();
    const wanted = people
      .filter((person) => matchesQuery(person.name || person.open_id, filter.value))
      .sort((left, right) => Number(isUsable(right)) - Number(isUsable(left))
        || (left.name || left.open_id).localeCompare(right.name || right.open_id, 'zh-Hans-CN'))
      .slice(0, DIRECTORY_LIMIT);
    current.textContent = chosen ? '已选：' + (chosen.name || chosen.open_id) : '尚未选择';
    bindBtn.disabled = loading || !chosen;
    if (!people.length) {
      list.append(el('div', { class: 'empty', text: loading ? '正在读取飞书通讯录…' : '通讯录里没有可取的人员' }));
      return;
    }
    if (!wanted.length) {
      list.append(el('div', { class: 'empty', text: '无匹配人员' }));
      return;
    }
    for (const person of wanted) {
      const bound = boundAccount.get(person.open_id);
      const usable = isUsable(person);
      // 单选框先建出来再放进行里：它是这一行的状态载体，用变量持有比事后 querySelector 更直白
      // （也让这段逻辑在没有 CSS 选择器的环境里可测）。
      const box = el('input', { type: 'radio', name: 'feishu-person', disabled: !usable });
      box.checked = !!(chosen && chosen.open_id === person.open_id);
      box.addEventListener('change', () => { chosen = person; paint(); });
      list.append(el('label', { class: 'feishu-picker-row' + (usable ? '' : ' disabled') }, [
        box,
        el('span', { class: 'feishu-picker-name', text: person.name || person.open_id }),
        el('span', { class: 'feishu-picker-id muted', text: person.open_id }),
        bound
          ? el('span', { class: 'muted', text: bound.id === account.id ? '已绑定本账号' : '已绑定「' + bound.name + '」' })
          : badge('未绑定', 'ok'),
      ]));
    }
  }

  // startProgress is the visible half of "this read talks to Feishu and can take a while": a spinner,
  // the sentence, and the seconds ticking up. 这句话必须写在屏幕上——tooltip 在触屏上等于不存在。
  function startProgress() {
    stopProgress();
    progress = progressLine('正在读取飞书通讯录');
    status.replaceChildren(...progress.nodes,
      el('span', { class: 'muted', text: '（首次读取要访问飞书接口，可能几秒到十几秒；之后 60 秒内走缓存）' }));
    status.hidden = false;
    filter.disabled = true;
    refreshBtn.disabled = true;
    loading = true;
    paint();
  }

  function stopProgress() {
    if (progress) { progress.stop(); progress = null; }
    status.replaceChildren();
    status.hidden = true;
    filter.disabled = false;
    refreshBtn.disabled = false;
  }

  function setNotice(kind, text) {
    notice.className = 'feishu-picker-notice' + (kind ? ' ' + kind : '');
    notice.textContent = text;
    notice.hidden = !text;
  }

  async function load(refresh) {
    if (loading) return;
    startProgress();
    setNotice('', '');
    try {
      const payload = await api.get('/org/feishu/directory', refresh ? { refresh: 'true' } : undefined);
      people = (payload && payload.users) || [];
      boundAccount = new Map();
      for (const person of people) {
        if (person.account && person.account.id) boundAccount.set(person.open_id, person.account);
      }
      loading = false;
      stopProgress();
      if (!people.length) {
        setNotice('warn', '飞书通讯录里没有人员：请先确认应用有「获取用户基本信息」权限（见 docs/feishu.md §5c），再点「刷新」。');
      } else if (payload.names_available === false) {
        setNotice('warn', '飞书应用缺少用户信息权限，人员没有姓名，只能按 ou_… 辨认；绑定仍然可用。');
      }
    } catch (err) {
      loading = false;
      stopProgress();
      people = [];
      // 服务端的原话 + 下一步动作。框不关：读失败不是操作员的错，也不该让人从"点了没反应"重来一遍。
      setNotice('error', api.errorMessage(err) + '（点「刷新」重试）');
    }
    paint();
  }

  return new Promise((resolve) => {
    close = (value) => {
      if (progress) { progress.stop(); progress = null; }
      backdrop.remove();
      resolve(value);
    };
    const dialog = el('div', { class: 'modal feishu-picker-dialog' }, [
      modalHead('绑定飞书人员：' + account.name, () => close(false)),
      modalBody([
        el('div', { class: 'muted', text: '选择这个账号对应的飞书人员。绑定后，该人就能用飞书登录这个账号的 DSH 租户（账号 DSH 有效时）。' +
          '不需要扫码，也不改动账号的组织归属。' }),
        status,
        notice,
        el('div', { class: 'feishu-picker-toolbar' }, [filter, refreshBtn]),
        list,
        current,
      ]),
      modalActions([
        el('button', { class: 'btn', text: '取消', onclick: () => close(false) }),
        bindBtn,
      ]),
    ]);
    const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
    backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(false); });
    document.getElementById('modal-root').append(backdrop);
    // 弹窗已经在屏幕上了，现在才去读通讯录：慢，也是看得见的慢。
    paint();
    load(false);
  });
}

// unbindAccountFeishu 解除账号的飞书身份（幂等）。门户登录判定读的就是它，所以确认框必须把
// 后果说清楚：这个人将无法再用飞书登录。
export async function unbindAccountFeishu(account) {
  const feishu = account.feishu || {};
  const who = feishu.name || feishu.open_id || '该飞书身份';
  const ok = await confirmDialog('解绑飞书',
    '确认解除账号「' + account.name + '」与 ' + who + ' 的绑定吗？' +
    '解绑后该人将无法再用飞书登录这个账号的 DSH 租户；账号下的 Key、余额与数据都不受影响。');
  if (!ok) return false;
  try {
    const result = await api.del('/accounts/' + account.id + '/feishu');
    toast(result && result.unbound ? '已解绑' : '该账号本来就没有绑定', 'ok');
    return true;
  } catch (err) {
    toast(api.errorMessage(err), 'error');
    return false;
  }
}
