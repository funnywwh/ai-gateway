// 为账号选择飞书人员（M72）：绑定入口从「Key + 扫码」改成「账号 + 选人」。
//
// 数据直接来自组织页那份飞书通讯录（GET /org/feishu/directory，服务端 60 秒缓存）：它已经
// 带着每个人当前的归属（`account`），所以「这个人是不是已经绑到别的账号」不需要再问一次接口，
// 也就不会出现「弹窗里能选、点下去 409」的错位。写绑定走账号优先的
// `PUT /accounts/{id}/feishu`：只写账号级身份，不要求这个人仍在通讯录里（离职者也要能清理），
// 也不顺带改动账号的组织归属。
import { api } from '../api.js';
import { el, toast, badge, modalHead, modalBody, modalActions, withBusy, confirmDialog } from '../ui.js';
import { matchesQuery } from '../pinyin.js';

// 人员列表上限：与组织页的账号列表同口径，一次拉全量比做一套分页更简单。
const DIRECTORY_LIMIT = 2000;

// openFeishuPersonPicker 让管理员为某个账号选一个飞书人员。返回 true 表示绑定已写入。
export async function openFeishuPersonPicker({ account, onBound } = {}) {
  let payload = null;
  try {
    payload = await api.get('/org/feishu/directory');
  } catch (err) {
    toast(api.errorMessage(err), 'error');
    return false;
  }
  const people = (payload && payload.users) || [];
  if (!people.length) {
    toast('飞书通讯录里没有人员：请先确认应用有「获取用户基本信息」权限', 'error');
    return false;
  }
  // 谁已经绑到哪个账号：通讯录预览的 `account` 就是服务端的合并结果。
  const boundAccount = new Map();
  for (const person of people) {
    if (person.account && person.account.id) boundAccount.set(person.open_id, person.account);
  }

  let chosen = null;
  const namesUnavailable = payload.names_available === false;

  if (namesUnavailable) {
    toast('飞书应用缺少用户信息权限，人员没有姓名，只能按 ou_… 辨认', 'error');
  }

  const filter = el('input', { type: 'search', placeholder: '按姓名过滤（支持拼音，如 lizhichao）…' });
  const list = el('div', { class: 'feishu-picker-list' });
  const current = el('span', { class: 'muted' });

  function paint() {
    list.replaceChildren();
    const wanted = people
      .filter((person) => matchesQuery(person.name || person.open_id, filter.value))
      .sort((left, right) => Number(isUsable(right)) - Number(isUsable(left))
        || (left.name || left.open_id).localeCompare(right.name || right.open_id, 'zh-Hans-CN'))
      .slice(0, DIRECTORY_LIMIT);
    current.textContent = chosen ? '已选：' + (chosen.name || chosen.open_id) : '尚未选择';
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

  // 已被别的账号占用的人不可选：绑定接口会 409，弹窗里先说清楚比让人点一次再报错好。
  function isUsable(person) {
    const bound = boundAccount.get(person.open_id);
    return !bound || bound.id === account.id;
  }

  filter.addEventListener('input', paint);
  paint();

  return await new Promise((resolve) => {
    const close = (value) => { picker.remove(); resolve(value); };
    const picker = el('div', { class: 'modal-backdrop' }, [
      el('div', { class: 'modal feishu-picker-dialog' }, [
        modalHead('绑定飞书人员：' + account.name, () => close(false)),
        modalBody([
          el('div', { class: 'muted', text: '选择这个账号对应的飞书人员。绑定后，该人就能用飞书登录这个账号的 DSH 租户（账号 DSH 有效时）。' +
            '不需要扫码，也不改动账号的组织归属。' }),
          filter,
          list,
          current,
        ]),
        modalActions([
          el('button', { class: 'btn', text: '取消', onclick: () => close(false) }),
          el('button', {
            class: 'btn btn-primary', text: '绑定',
            onclick: (ev) => {
              if (!chosen) { toast('请先选择一个人', 'error'); return; }
              const button = ev.currentTarget;
              withBusy(button, '绑定中', () => api.put('/accounts/' + account.id + '/feishu', {
                open_id: chosen.open_id, union_id: chosen.union_id || '', name: chosen.name || '',
              })).then((result) => {
                toast(result && result.result === 'replaced' ? '已改绑（原身份已解除）' : '已绑定飞书身份', 'ok');
                if (onBound) onBound(chosen);
                close(true);
              }).catch((err) => {
                toast(api.errorMessage(err), 'error');
              });
            },
          }),
        ]),
      ]),
    ]);
    picker.addEventListener('click', (ev) => { if (ev.target === picker) close(false); });
    document.getElementById('modal-root').append(picker);
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

