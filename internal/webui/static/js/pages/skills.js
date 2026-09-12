// 技能库：当前登录管理员自己的技能。
//
// Skills are private to the administrator who wrote them, so this page never has a
// cross-account view: every request goes to an endpoint that filters by the session's
// owner, and there is no "all skills" mode to build. Two ways in: write one by hand here,
// or generate a draft from a conversation (the chat page leaves the draft in this browser's
// session storage) and edit it before saving.

import { api } from '../api.js';
import { el, clear, toast, pager, confirmDialog, formatTime } from '../ui.js';
import { navigate } from '../router.js';

// Page state lives outside render() so returning to the page keeps the window the operator
// was looking at; the size is mutable because the pager lets them change it.
let page = 1;
let pageSize = 20;

export async function render({ page: host, actions, route }) {
  const state = { total: 0, skills: [] };
  const listHost = el('div');
  const editorHost = el('div');

  const newBtn = el('button', { class: 'btn btn-primary', text: '新建技能' });
  newBtn.addEventListener('click', () => openEditor(null, null));
  if (actions) {
    clear(actions);
    actions.append(newBtn);
  }

  host.append(el('section', { class: 'card' }, [
    el('p', {
      class: 'muted',
      text: '技能只属于当前登录账号：别的管理员既看不到、也用不到它。在智能问答里用「＋ → 创建技能」可以从一段对话生成草稿，' +
        '也可以在这里手写。技能内容会随请求发送给所选的上游模型，但不会写进全局请求日志或审计日志。',
    }),
    listHost,
    editorHost,
  ]));

  await load();

  const draft = route && route.params && route.params.get('new') ? takeDraft() : null;
  if (draft) openEditor(null, draft);

  // No teardown: this page holds no stream and no timers, so leaving it needs no cleanup.
  return null;

  async function load() {
    clear(listHost);
    listHost.append(el('div', { class: 'empty', text: '加载中…' }));
    try {
      const payload = await api.get('/chat/skills', { limit: pageSize, offset: (page - 1) * pageSize });
      state.skills = payload.data || [];
      state.total = payload.total || 0;
    } catch (err) {
      clear(listHost);
      listHost.append(el('div', { class: 'empty', text: api.errorMessage(err) }));
      return;
    }
    clear(listHost);
    if (!state.skills.length) {
      listHost.append(el('div', {
        class: 'empty',
        text: '还没有技能。可以在智能问答里边聊边用「＋ → 创建技能」生成一个，或点右上角手写一个。',
      }));
      return;
    }
    const table = el('table', { class: 'table' });
    table.append(el('tr', {}, [
      el('th', { text: '名称' }), el('th', { text: '说明' }), el('th', { text: '字数' }),
      el('th', { text: '来源会话' }), el('th', { text: '更新时间' }), el('th', { text: '' }),
    ]));
    for (const skill of state.skills) {
      const source = skill.source_session_id
        ? el('a', { href: '#/chat?session=' + encodeURIComponent(skill.source_session_id), text: '打开会话' })
        : el('span', { class: 'muted', text: '手写' });
      const edit = el('button', { class: 'btn btn-ghost btn-xs', text: '编辑' });
      edit.addEventListener('click', () => openEditor(skill, null));
      const remove = el('button', { class: 'btn btn-danger btn-xs', text: '删除' });
      remove.addEventListener('click', async () => {
        const ok = await confirmDialog('删除技能', '删除「' + skill.name + '」？已经加载它的会话下次提问时会自动忽略它。');
        if (!ok) return;
        try {
          await api.del('/chat/skills/' + skill.id);
          toast('已删除');
          await load();
        } catch (err) {
          toast(api.errorMessage(err), 'error');
        }
      });
      table.append(el('tr', {}, [
        el('td', { text: skill.name }),
        el('td', { class: 'muted', text: skill.description || '—' }),
        el('td', { text: String(skill.chars || 0) }),
        el('td', {}, [source]),
        el('td', { class: 'muted', text: formatTime(skill.updated_at) }),
        el('td', { class: 'row-actions' }, [edit, remove]),
      ]));
    }
    listHost.append(el('div', { class: 'table-wrap' }, [table]));
    listHost.append(pager({
      limit: pageSize, offset: (page - 1) * pageSize, total: state.total, unit: '个技能',
      onChange: (next) => {
        pageSize = next.limit;
        page = Math.floor(next.offset / pageSize) + 1;
        load();
      },
    }));
  }

  function openEditor(skill, draft) {
    const seed = skill || draft || { name: '', description: '', instructions: '' };
    const name = el('input', { type: 'text', value: seed.name || '', placeholder: '例如：排查某模型不可用' });
    const description = el('input', { type: 'text', value: seed.description || '', placeholder: '什么时候该用它' });
    const instructions = el('textarea', {
      rows: '16', placeholder: '给模型的执行指令：适用场景、要调用哪些后台接口、怎么判断、要注意什么',
    });
    instructions.value = seed.instructions || '';
    const status = el('div', { class: 'muted', text: seed.note || '' });

    const save = el('button', {
      class: 'btn btn-primary', text: skill ? '保存' : '创建并加载',
      title: '保存后这个技能会出现在智能问答的「＋」菜单里',
    });
    save.addEventListener('click', async () => {
      save.disabled = true;
      try {
        const body = { name: name.value, description: description.value, instructions: instructions.value };
        if (skill) await api.patch('/chat/skills/' + skill.id, body);
        else await api.post('/chat/skills', Object.assign({ session_id: seed.session_id || '' }, body));
        toast('已保存');
        editorHost.replaceChildren();
        await load();
      } catch (err) {
        status.textContent = api.errorMessage(err);
        status.className = 'toast error';
      } finally {
        save.disabled = false;
      }
    });
    const cancel = el('button', {
      class: 'btn', text: '取消', onclick: () => editorHost.replaceChildren(),
    });
    const openChat = el('button', { class: 'btn btn-ghost', text: '去智能问答' });
    openChat.addEventListener('click', () => navigate('/chat'));

    clear(editorHost);
    editorHost.append(el('section', { class: 'card' }, [
      el('div', { class: 'toolbar' }, [el('h2', { text: skill ? '编辑技能：' + skill.name : '新建技能', style: 'margin:0;flex:1' })]),
      el('label', { class: 'field' }, [el('span', { text: '名称' }), name]),
      el('label', { class: 'field' }, [el('span', { text: '说明' }), description]),
      el('label', { class: 'field' }, [el('span', { text: '指令（Markdown）' }), instructions]),
      status,
      el('div', { class: 'row-actions' }, [save, cancel, openChat]),
    ]));
    name.focus();
  }
}

// takeDraft reads (and clears) the draft the chat page generated. It lives in this browser
// only: an unsaved draft has no reason to exist on the server.
function takeDraft() {
  try {
    const raw = window.sessionStorage.getItem('aigw.chat.draft');
    if (!raw) return null;
    window.sessionStorage.removeItem('aigw.chat.draft');
    return JSON.parse(raw);
  } catch (err) {
    return null;
  }
}
