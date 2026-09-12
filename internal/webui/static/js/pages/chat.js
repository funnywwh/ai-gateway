// 智能问答：选模型 → 提问 → 模型调用网关后台工具 → 流式回答。
//
// The page owns three things the shell does not: a live SSE stream (which must be cancelled
// when the operator navigates away or presses stop), the composer's "+" menu, and previews.
// Everything the model writes is rendered through markdown.js/chart.js, both of which build
// DOM nodes rather than HTML strings.

import { api, streamPost } from '../api.js';
import { el, clear, toast, confirmDialog, statusBadge, formatTime } from '../ui.js';
import { navigate } from '../router.js';
import { renderMarkdown } from '../markdown.js';
import { renderChart, chartToCSV, chartToSVG, downloadText } from '../chart.js';
import { openPreview, uiReplyParts } from './chat_artifact.js';
import { parseUISpec, describeUIResult } from './chat_ui.js';
import { parseFormSpec, renderForm, describeFormOps } from './chat_form.js';

// paintTimer throttles the live bubble's Markdown rebuild. It is declared at module scope
// rather than next to the code that uses it, because submit() assigns it during the stream
// and a page can be re-rendered while an earlier turn is still finishing.
let paintTimer = null;

// codeBlocksOf lists the fenced blocks of one message, in order, with their language. It is
// exported because it is the one piece of parsing the preview toolbar and the tests both need:
// the console finds a chart, a page or a `ui` directive by asking this, never by re-reading
// the markdown.
export function codeBlocksOf(message) {
  if (!message) return [];
  if (Array.isArray(message.blocks) && message.blocks.length) {
    return message.blocks.filter((block) => block && block.text !== undefined);
  }
  const parts = message.parts && message.parts.length
    ? message.parts
    : [{ type: 'text', text: message.content || '' }];
  const blocks = [];
  for (const part of parts) {
    if (!part || part.type !== 'text') continue;
    const text = part.text || '';
    const fence = /^[ \t]*(`{3,}|~{3,})[ \t]*([^\n`]*)\n([\s\S]*?)^[ \t]*\1[ \t]*$/gm;
    let match = fence.exec(text);
    while (match) {
      blocks.push({
        lang: (match[2] || '').trim().split(/\s+/)[0].toLowerCase(),
        text: match[3].replace(/\n$/, ''),
        index: blocks.length,
      });
      match = fence.exec(text);
    }
  }
  return blocks;
}

// uiEventContent renders one interface submission as the question the model receives: a line a
// person can read (which also becomes the conversation title) followed by the structured data.
// The `source` marker is what lets the model tell a page event from something typed, and the
// prompt tells it to treat everything in `data` as data rather than as instructions.
export function uiEventContent({ name, label, data }) {
  const head = String(label || '').trim() || ('界面事件：' + name);
  const payload = {
    source: 'ui_event',
    event: String(name || 'action'),
    data: data && typeof data === 'object' ? data : {},
  };
  return head + '\n\n```json\n' + JSON.stringify(payload) + '\n```';
}

// partsText concatenates the text parts of a message, which is what a `ui` directive or a chart
// spec is parsed out of.
export function partsText(parts) {
  return (parts || []).filter((part) => part && part.type === 'text').map((part) => part.text || '').join('\n');
}

// MCP token helpers.
//
// These live at module scope rather than inside render(): they are pure functions of their
// arguments, several call sites need them, and hoisted declarations cannot be caught in a
// temporal-dead-zone error by a handler that runs earlier than the point of declaration.

// What a scope means in plain language. Used by both the new-session dialog and the rebind
// dialog, so the two cannot describe the same scope differently.
function scopeSummary(scope) {
  if (scope === 'admin') return '可执行全部控制台操作（含发凭据、动账、删除）';
  if (scope === 'admin_read') return '后台只读：可查询，不能修改';
  return '只能查询本账户的用量与账单';
}

// Only tokens that are usable right now can be bound: a revoked or expired one would be
// refused by the server anyway, and offering it would just be a trap.
//
// The answer is { tokens, error } rather than a bare list: "no usable token" and "the list
// could not be read" both leave a picker with nothing to offer, and a picker that conflates
// them tells the operator to go issue a token when the real problem is the request.
async function fetchUsableTokens() {
  let payload;
  try {
    payload = await api.get('/mcp-tokens', { limit: 200 });
  } catch (err) {
    return { tokens: [], error: api.errorMessage(err) };
  }
  const tokens = (payload.data || []).filter((token) => {
    if (token.status !== 'active') return false;
    if (!token.expires_at) return true;
    return new Date(token.expires_at).getTime() > Date.now();
  });
  return { tokens, error: '' };
}

// What a token picker says when it has nothing to offer. Both dialogs share it, so the same
// empty picker never reads two different ways.
function tokensUnavailableHint(error, emptyText) {
  return error ? '读取 MCP 令牌失败：' + error : emptyText;
}

function tokenOption(token) {
  return el('option', {
    value: String(token.id),
    text: (token.name || token.token_prefix) + ' · scope=' + (token.scope || 'query'),
  });
}

export async function render({ page, actions, session, route }) {
  const state = {
    sessions: [],
    session: null,
    messages: [],
    skills: [],
    models: [],
    accounts: [],
    keys: [],
    tokens: [],
    stream: null,
    controller: null,
    running: false,
    tools: new Map(),
    turnID: null,
    notice: '',
    // The open preview, when it is interactive: the console's half of the bridge and where a
    // submission goes. One console has one open preview, so one slot is enough.
    preview: null,
    // Submissions that arrived while a turn was in flight, oldest first. An inline form and an
    // interactive preview both land here; each entry remembers where it came from, because the
    // answer has to be patched back into the thing that asked.
    queue: [],
    // What the operator typed into a form but has not submitted, keyed by message and block.
    // renderMain() rebuilds the whole transcript after every turn, so without this a draft would
    // be wiped out by the act of sending a *different* question.
    formDrafts: new Map(),
    // A `ui` directive that arrived with the turn that just finished, for an inline form. It is
    // parked here until the transcript has been rebuilt from the server: see applyPendingFormOps.
    pendingFormOps: null,
  };

  const layout = el('div', { class: 'chat-layout' });
  const sidebar = el('aside', { class: 'chat-sidebar' });
  const main = el('section', { class: 'chat-main' });
  layout.append(sidebar, main);
  page.append(layout);

  const toolbar = el('div', { class: 'toolbar' });
  const newBtn = el('button', { class: 'btn btn-primary', text: '新建会话' });
  toolbar.append(el('h2', { text: '会话', style: 'margin:0;flex:1' }), newBtn);
  sidebar.append(toolbar);
  const sessionList = el('div', { class: 'chat-session-list' });
  sidebar.append(sessionList);
  newBtn.addEventListener('click', () => createSession());

  // Leaving the page tears both live channels down: the turn stream, and the bridge to any open
  // preview (whose frame is about to disappear with the page).
  const destroy = () => {
    if (state.controller) state.controller.abort();
    if (state.preview) { state.preview.destroy(); state.preview = null; }
    state.queue = [];
  };
  if (actions) {
    clear(actions);
    const refresh = el('button', { class: 'btn btn-ghost', text: '刷新' });
    refresh.addEventListener('click', () => loadSessions());
    actions.append(refresh);
  }

  await loadSkills();
  await loadSessions();

  const wanted = route && route.params ? route.params.get('session') : null;
  if (wanted) {
    const found = state.sessions.find((item) => item.id === wanted);
    await openSession(wanted);
    if (!found) toast('该会话不在你的列表里', 'error');
  } else if (state.sessions.length) {
    await openSession(state.sessions[0].id);
  } else {
    renderEmptyState();
  }

  return destroy;

  // -------------------------------------------------------------------------
  // data
  // -------------------------------------------------------------------------

  async function loadSessions() {
    try {
      const payload = await api.get('/chat/sessions', { limit: 50 });
      state.sessions = payload.data || [];
    } catch (err) {
      state.sessions = [];
      toast(api.errorMessage(err), 'error');
    }
    renderSessionList();
  }

  async function loadSkills() {
    try {
      const payload = await api.get('/chat/skills', { limit: 200 });
      state.skills = payload.data || [];
    } catch (err) {
      state.skills = [];
    }
  }

  function renderSessionList() {
    clear(sessionList);
    if (!state.sessions.length) {
      sessionList.append(el('div', { class: 'empty', text: '还没有会话' }));
      return;
    }
    for (const item of state.sessions) {
      const row = el('div', {
        class: 'chat-session' + (state.session && state.session.id === item.id ? ' active' : ''),
      }, [
        el('div', { class: 'chat-session-title', text: item.title || '未命名会话' }),
        el('div', { class: 'chat-session-meta', text: item.model + ' · ' + formatTime(item.updated_at) }),
      ]);
      row.addEventListener('click', () => openSession(item.id));
      sessionList.append(row);
    }
  }

  async function openSession(id) {
    if (state.running) {
      toast('请先停止正在生成的回答', 'error');
      return;
    }
    try {
      state.session = await api.get('/chat/sessions/' + encodeURIComponent(id));
    } catch (err) {
      toast(api.errorMessage(err), 'error');
      return;
    }
    state.messages = state.session.messages || [];
    state.tools = new Map();
    for (const call of state.session.tool_calls || []) state.tools.set(call.call_id, call);
    // state.notice is deliberately kept: a "stopped" or "truncated" note from the turn that
    // just finished must still be readable after the conversation is reloaded.
    renderSessionList();
    renderMain();
  }

  // rebindToken points an existing conversation at another token. It exists because a
  // conversation's authority lives in the token, not in the session row: when a token is
  // revoked or expires, rebinding is how the operator restores the conversation instead of
  // abandoning it (and the skills it has loaded) and starting over.
  async function rebindToken() {
    const current = state.session.mcp_token_id ? String(state.session.mcp_token_id) : '';
    const { tokens, error } = await fetchUsableTokens();
    const picker = el('select', {});
    for (const token of tokens) picker.append(tokenOption(token));
    if (current && tokens.some((token) => String(token.id) === current)) picker.value = current;
    const hint = el('p', { class: 'muted' });
    const status = el('div', { class: 'muted' });
    const describe = () => {
      const chosen = tokens.find((token) => String(token.id) === picker.value);
      hint.textContent = chosen
        ? '改绑后本会话权限：' + scopeSummary(chosen.scope || 'query')
        : tokensUnavailableHint(error, '没有可用的 MCP 令牌：请先到「MCP 令牌」页签发一个。');
    };
    picker.addEventListener('change', describe);
    describe();

    const dialog = el('div', { class: 'modal' }, [
      el('div', { class: 'toolbar' }, [el('h2', { text: '改绑 MCP 令牌' })]),
      el('div', { class: 'modal-body' }, [
        el('label', { class: 'field' }, [el('span', { text: 'MCP 令牌' }), picker]),
        hint,
        el('p', {
          class: 'muted',
          text: '本会话已加载的技能、消息与计费绑定都不受影响；只换执行时使用的身份。工具面会在下一次提问时按新令牌的 scope 重新计算。',
        }),
        status,
      ]),
      el('div', { class: 'modal-actions' }, [
        el('button', { class: 'btn', text: '取消', onclick: () => backdrop.remove() }),
        el('button', {
          class: 'btn btn-primary', text: '改绑', onclick: async () => {
            if (!picker.value) {
              status.textContent = '请先选择一个 MCP 令牌。';
              status.className = 'toast error';
              return;
            }
            try {
              const updated = await api.patch('/chat/sessions/' + encodeURIComponent(state.session.id),
                { mcp_token_id: Number(picker.value) });
              // Trust the server's derived values rather than recomputing them here: write_mode
              // is derived from the token's scope, and only the server knows the new one.
              state.session.mcp_token_id = updated.mcp_token_id;
              state.session.write_mode = updated.write_mode;
              backdrop.remove();
              renderSessionList();
              renderMain();
              toast('已改绑令牌', 'ok');
            } catch (err) {
              status.textContent = api.errorMessage(err);
              status.className = 'toast error';
            }
          },
        }),
      ]),
    ]);
    const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
    backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) backdrop.remove(); });
    document.getElementById('modal-root').append(backdrop);
  }

  async function createSession() {
    if (!state.accounts.length) {
      try {
        const payload = await api.get('/accounts', { limit: 200 });
        state.accounts = payload.data || payload.accounts || [];
      } catch (err) {
        toast(api.errorMessage(err), 'error');
        return;
      }
    }
    const accountSelect = el('select', {}, state.accounts.map((account) => el('option', {
      value: String(account.id), text: account.name + '（' + (account.billing_mode || 'postpaid') + '）',
    })));
    const keySelect = el('select', {});
    const modelSelect = el('select', {});
    // The conversation acts as an MCP token, so picking the token IS picking the permission:
    // there is no separate write switch to keep consistent with it.
    const tokenSelect = el('select', {});
    const tokenHint = el('p', { class: 'muted' });
    const status = el('div', { class: 'muted' });

    async function loadKeys() {
      clear(keySelect);
      try {
        const payload = await api.get('/keys', { account_id: accountSelect.value, limit: 200 });
        state.keys = payload.data || [];
      } catch (err) {
        state.keys = [];
      }
      for (const key of state.keys) {
        keySelect.append(el('option', { value: String(key.id), text: (key.name || key.key_prefix) + ' · ' + key.status }));
      }
      await loadModels();
    }
    async function loadModels() {
      clear(modelSelect);
      if (!keySelect.value) return;
      try {
        const payload = await api.get('/chat/models', { account_id: accountSelect.value, api_key_id: keySelect.value });
        state.models = payload.data || [];
      } catch (err) {
        state.models = [];
        status.textContent = api.errorMessage(err);
      }
      for (const model of state.models) {
        modelSelect.append(el('option', {
          value: model.id, text: model.id + (model.has_tools ? '' : '（不支持工具调用）'),
        }));
      }
    }
    accountSelect.addEventListener('change', loadKeys);
    keySelect.addEventListener('change', loadModels);

    // Only tokens that are usable right now can be bound: a revoked or expired one would be
    // refused by the server anyway, and offering it would just be a trap.
    //
    // The options are filled once, at open; a change only re-describes the choice. Refilling
    // from the change handler would throw away the very selection that fired the event:
    // removing the options resets the select, and the first option appended afterwards becomes
    // the selected one again — so the picker would snap back to the first token no matter
    // which one the operator clicked (and refetch the list on every click).
    let tokenError = '';
    async function loadTokens() {
      clear(tokenSelect);
      const loaded = await fetchUsableTokens();
      state.tokens = loaded.tokens;
      tokenError = loaded.error;
      for (const token of state.tokens) {
        tokenSelect.append(tokenOption(token));
      }
      describeToken();
    }
    function describeToken() {
      const chosen = state.tokens.find((token) => String(token.id) === tokenSelect.value);
      tokenHint.textContent = chosen
        ? '本会话权限：' + scopeSummary(chosen.scope || 'query')
        : tokensUnavailableHint(tokenError, '没有可用的 MCP 令牌：请先到「MCP 令牌」页签发一个，scope 决定本会话能做什么。');
    }
    tokenSelect.addEventListener('change', describeToken);

    const dialog = el('div', { class: 'modal' }, [
      el('div', { class: 'toolbar' }, [el('h2', { text: '新建会话' })]),
      el('div', { class: 'modal-body' }, [
        el('label', { class: 'field' }, [el('span', { text: '计费账户' }), accountSelect]),
        el('label', { class: 'field' }, [el('span', { text: 'API Key（用于计费）' }), keySelect]),
        el('label', { class: 'field' }, [el('span', { text: '模型' }), modelSelect]),
        el('label', { class: 'field' }, [el('span', { text: 'MCP 令牌（决定本会话能做什么）' }), tokenSelect]),
        tokenHint,
        el('p', { class: 'muted', text: '智能问答就是一个 MCP 客户端：工具面与可执行范围完全来自所选令牌的 scope，令牌被撤销或过期后本会话立即失效。每一步模型调用仍按所选 API Key 正常计费，并出现在「请求日志」里（客户端记为 console，正文不记录）。' }),
        status,
      ]),
      el('div', { class: 'modal-actions' }, [
        el('button', { class: 'btn', text: '取消', onclick: () => backdrop.remove() }),
        el('button', {
          class: 'btn btn-primary', text: '创建', onclick: async () => {
            if (!tokenSelect.value) {
              status.textContent = '请先选择一个 MCP 令牌。';
              status.className = 'toast error';
              return;
            }
            try {
              const created = await api.post('/chat/sessions', {
                model: modelSelect.value,
                account_id: Number(accountSelect.value),
                api_key_id: Number(keySelect.value),
                mcp_token_id: Number(tokenSelect.value),
              });
              backdrop.remove();
              await loadSessions();
              await openSession(created.id);
            } catch (err) {
              status.textContent = api.errorMessage(err);
              status.className = 'toast error';
            }
          },
        }),
      ]),
    ]);
    const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
    backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) backdrop.remove(); });
    document.getElementById('modal-root').append(backdrop);
    await Promise.all([loadKeys(), loadTokens()]);
  }

  // -------------------------------------------------------------------------
  // rendering
  // -------------------------------------------------------------------------

  function renderEmptyState() {
    clear(main);
    main.append(el('div', { class: 'empty', text: '新建一个会话开始提问：会话会绑定模型、计费 Key 和一个 MCP 令牌——令牌的 scope 决定它能做什么，admin scope 的令牌可以执行全部控制台操作。' }));
  }

  // captureFormDrafts saves what is typed into every inline form on screen. renderMain() throws
  // the whole transcript away and rebuilds it, so this has to run before `clear(main)` or a
  // half-filled form would lose its contents to an unrelated question — or to the very turn the
  // form just started, since a turn rebuilds the transcript when it finishes.
  function captureFormDrafts() {
    for (const node of main.querySelectorAll('.chat-form')) {
      const form = node.__aigwForm;
      if (!form) continue;
      const key = formKeyOf(form);
      if (!key) continue;
      state.formDrafts.set(key, form.collect());
    }
  }

  // formKeyOf rebuilds the key a form was rendered under (`<message id>:<block index>`) from the
  // form itself, so capture and restore cannot disagree about which form this is.
  function formKeyOf(form) {
    const node = form && form.node;
    if (!node) return '';
    const holder = node.closest('.chat-msg');
    // `live` is the id of the assistant bubble being streamed, and it is the one id that is not
    // on the element (the live message has no server id yet). Anything else without a holder is
    // a form the console cannot key, and returning '' makes the caller skip it rather than file
    // a draft under a name that will never be read back.
    const messageID = (holder && holder.getAttribute('data-message-id')) || '';
    if (!messageID) return '';
    return messageID + ':' + form.spec.id;
  }

  function renderMain() {
    captureFormDrafts();
    clear(main);
    if (!state.session) { renderEmptyState(); return; }
    const s = state.session;

    // The badge is the control: a conversation's authority lives in its token, so this is
    // where the operator both reads and changes it.
    const tokenBadge = el('button', {
      class: 'badge badge-button ' + (s.write_mode === 'allow_writes' ? 'danger' : ''),
      text: s.mcp_token_id
        ? 'MCP 令牌 #' + s.mcp_token_id + (s.write_mode === 'allow_writes' ? '（全部控制台操作）' : '（只读）') + ' · 改绑'
        : '未绑定 MCP 令牌 · 点击绑定',
      title: s.mcp_token_id
        ? '本会话的权限来自该令牌；撤销或让它过期后，下一次工具调用会立即失效。点击可换成别的令牌。'
        : '这个会话没有绑定令牌，因此不能调用任何工具（技能只能被读到，执行不了）。点击绑定一个令牌即可恢复。',
    });
    tokenBadge.addEventListener('click', () => {
      rebindToken().catch((err) => { toast(api.errorMessage(err), 'error'); });
    });

    const header = el('div', { class: 'chat-header' }, [
      el('div', { class: 'chat-header-title', text: s.title || '未命名会话' }),
      el('div', { class: 'chat-header-meta', text: s.model + ' · 账户 #' + s.account_id + ' · Key #' + s.api_key_id }),
      tokenBadge,
    ]);
    const chips = el('div', { class: 'chat-chips' });
    for (const skill of state.session.skills || []) {
      const chip = el('span', { class: 'chip', title: skill.description || '' }, [skill.name]);
      const remove = el('button', { class: 'chip-x', text: '×', title: '从本会话卸载' });
      remove.addEventListener('click', () => setSkills((s.skill_ids || []).filter((id) => id !== skill.id)));
      chip.append(remove);
      chips.append(chip);
    }
    if (!(state.session.skills || []).length) chips.append(el('span', { class: 'muted', text: '未加载技能' }));

    const stream = el('div', { class: 'chat-stream' });
    for (const message of state.messages) stream.append(renderMessage(message));
    const notice = el('div', { class: 'chat-notice' });
    if (state.notice) notice.append(el('div', { class: 'notice', text: state.notice }));

    const composer = renderComposer();
    main.append(header, chips, stream, notice, composer);
    stream.scrollTop = stream.scrollHeight;
  }

  function renderMessage(message) {
    const id = message.id || '';
    if (message.role === 'user') {
      return el('div', { class: 'chat-msg user', 'data-message-id': id }, [
        el('div', { class: 'chat-bubble' }, [el('div', { class: 'chat-text', text: message.content })]),
      ]);
    }
    const body = el('div', { class: 'chat-bubble assistant' });
    if (message.reasoning) {
      const details = el('details', { class: 'chat-reasoning' }, [
        el('summary', { text: '思考过程' }),
        el('div', { class: 'chat-text', text: message.reasoning }),
      ]);
      body.append(details);
    }
    const parts = message.parts || [];
    for (const part of parts) {
      if (!part || typeof part !== 'object') continue;
      if (part.type === 'text') body.append(renderRichText(part.text, message));
      else if (part.type === 'tool_call') body.append(renderToolCard(part));
    }
    // A message with no renderable part but a status is a real case — a turn interrupted before
    // it produced anything, or one rejected before it started. Showing nothing at all would
    // make it look like the question vanished.
    if (!parts.length && !message.content && message.status && message.status !== 'ok') {
      body.append(el('div', { class: 'muted', text: '这一轮没有产生内容（' + message.status + '）' })); 
    }
    if (!(message.parts || []).length && message.content) body.append(renderRichText(message.content, message));
    body.append(renderFooter(message));
    return el('div', { class: 'chat-msg assistant', 'data-message-id': id }, [body]);
  }

  function renderToolCard(part) {
    const known = state.tools.get(part.id);
    const status = part.status || (known && known.status) || 'done';
    const details = el('details', { class: 'chat-tool' + (part.is_error ? ' error' : '') }, [
      el('summary', {}, [
        el('span', { class: 'chat-tool-name', text: part.name }),
        statusBadge(status === 'done' ? 'ok' : status === 'pending' ? 'pending' : status),
      ]),
    ]);
    details.append(el('div', { class: 'chat-tool-body' }, [
      el('div', { class: 'chat-tool-label', text: '参数' }),
      el('pre', { class: 'md-pre' }, [el('code', { text: prettyJSON(part.arguments) })]),
      el('div', { class: 'chat-tool-label', text: '结果' }),
      el('pre', { class: 'md-pre' }, [el('code', { text: prettyJSON(part.result) })]),
    ]));
    return details;
  }

  function renderFooter(message) {
    const parts = [];
    if (message.tokens_in || message.tokens_out) {
      parts.push('tokens ' + (message.tokens_in || 0) + '→' + (message.tokens_out || 0));
    }
    if (message.charge_micros !== undefined) parts.push('收费 ' + micros(message.charge_micros));
    if (message.cost_micros !== undefined) parts.push('成本 ' + micros(message.cost_micros));
    if (message.resolved_model && message.resolved_model !== message.model) parts.push('路由到 ' + message.resolved_model);
    if (message.status && message.status !== 'ok') parts.push(message.status);
    if (message.error) parts.push(message.error);
    const footer = el('div', { class: 'chat-footer', text: parts.join(' · ') });
    const ids = (message.request_ids || []).slice();
    if (ids.length) {
      const copy = el('button', { class: 'btn btn-ghost btn-xs', text: '复制请求 id' });
      copy.addEventListener('click', () => copyText(ids.join('\n')));
      footer.append(' ', copy);
    }
    return footer;
  }

  function renderRichText(text, message) {
    const host = el('div', { class: 'chat-text md' });
    host.append(renderMarkdown(text));
    enhanceCodeBlocks(host, message);
    return host;
  }

  // enhanceCodeBlocks attaches the per-language toolbar: charts render in place, HTML/SVG get
  // a sandboxed preview, everything else stays source.
  function enhanceCodeBlocks(host, message) {
    host.querySelectorAll('code[data-lang]').forEach((code) => {
      const lang = (code.getAttribute('data-lang') || '').toLowerCase();
      const source = code.textContent || '';
      const wrap = code.closest('.md-code-wrap') || code.parentElement;
      const bar = el('div', { class: 'code-tools' }, [
        el('span', { class: 'code-lang', text: lang }),
      ]);
      const copy = el('button', { class: 'btn btn-ghost btn-xs', text: '复制' });
      copy.addEventListener('click', () => copyText(source));
      bar.append(copy);

      if (lang === 'form') {
        // An inline form replaces the block: the spec is not something a reader wants to read,
        // it is something they fill in. The block stays in the message, so a re-render (after
        // every turn, and after a reload from the server) rebuilds exactly the same form.
        //
        // A fence that is still open is skipped rather than reported: the answer is streaming,
        // so a half-written spec is a fact about the reader, not about the model.
        if (code.getAttribute('data-closed') === '0') return;
        renderFormBlock(host, wrap, message, code, source);
        return;
      }
      if (lang === 'ui') {
        // The directive is a patch for the *preview's* document, not for this transcript: on
        // its own it is inert here, so the block stays visible as the record of what the model
        // asked for. Parsing it eagerly is what turns a malformed directive into an error the
        // operator can see instead of a mysterious nothing.
        const parsed = parseUISpec(source);
        bar.append(el('span', {
          class: 'muted', text: parsed.error ? '规格有问题' : `${parsed.ops.length} 条更新`,
        }));
        const replay = el('button', { class: 'btn btn-ghost btn-xs', text: '重新应用', disabled: !state.preview });
        replay.addEventListener('click', () => {
          if (!state.preview || !state.preview.isOpen()) { toast('先打开这个页面（点上面的「预览」）', 'error'); return; }
          state.preview.reply({ type: 'ui', ops: parsed.ops });
          toast('已把这几条更新重新发到页面');
        });
        bar.append(replay);
        wrap.prepend(bar);
        if (parsed.error) {
          wrap.after(el('div', { class: 'notice ui-note error', text: '界面指令未生效：' + parsed.error }));
        }
        return;
      }
      if (lang === 'chart') {
        const parsed = renderChart(source, { toolCalls: toolCallNames(message) });
        if (parsed.error) {
          wrap.append(el('div', { class: 'notice', text: '图表未渲染：' + parsed.error + '（下面保留原始规格）' }));
        } else {
          const chartHost = el('div', { class: 'chart-host' }, [parsed.element]);
          const tools = el('div', { class: 'code-tools' }, [
            el('span', { class: 'code-lang', text: '图表' }),
          ]);
          const csv = el('button', { class: 'btn btn-ghost btn-xs', text: 'CSV' });
          csv.addEventListener('click', () => downloadText('chart.csv', chartToCSV(parsed.spec), 'text/csv;charset=utf-8'));
          const svg = el('button', { class: 'btn btn-ghost btn-xs', text: 'SVG' });
          svg.addEventListener('click', () => downloadText('chart.svg', chartToSVG(parsed.spec), 'image/svg+xml'));
          const copySpec = el('button', { class: 'btn btn-ghost btn-xs', text: '复制规格' });
          copySpec.addEventListener('click', () => copyText(source));
          tools.append(csv, svg, copySpec);
          wrap.before(chartHost);
          chartHost.append(tools);
          wrap.classList.add('collapsed');
        }
      } else if (lang === 'html' || lang === 'svg') {
        // Only an HTML preview with a billed conversation can be interactive: interacting means
        // submitting, and submitting means spending. Everything else stays a read-only preview.
        const canInteract = lang === 'html' && !!(state.session.account_id && state.session.api_key_id);
        const preview = el('button', {
          class: 'btn btn-ghost btn-xs',
          text: canInteract ? '预览（可交互）' : '预览',
          title: canInteract
            ? '在沙箱里打开这个页面；页面上的表单可以提交回本会话，每次提交都是一条计费的模型请求'
            : (lang === 'html'
              ? '只读预览：本会话没有绑定计费 Key，页面里的按钮不会产生请求'
              : '只读预览'),
        });
        preview.addEventListener('click', () => {
          // One interactive preview at a time: a second one would need its own channel, and the
          // submissions of both would race for the same conversation.
          if (state.preview) { state.preview.destroy(); state.preview = null; }
          // The promise is awaited here rather than left floating: openPreview runs to completion
          // before a port exists, and a throw inside it used to surface only as an unhandled
          // rejection in the console while the modal sat there saying nothing.
          const handle = openPreview({
            sessionId: state.session.id,
            key: (message.id || 'live-' + (state.turnID || 'turn')) + ':' + code.getAttribute('data-block-index'),
            format: lang,
            body: source,
            title: state.session.title || '预览',
            interactive: canInteract,
            onSubmit: onUIEvent,
            // Stopping is a control on the *open turn*, not an event from the page: it aborts
            // the stream the console owns, exactly like the composer's own stop button.
            onStop: () => { if (state.controller) state.controller.abort(); },
            // The modal can be closed without going through the page, so the slot is cleared
            // from here rather than assumed to be still valid.
            onClosed: () => { if (state.preview === handle) state.preview = null; },
          }).catch((err) => {
            // A throw here is a bug in the preview, not a model problem: say so out loud instead
            // of leaving an unhandled rejection and a modal that never explains itself.
            toast('预览打开失败：' + api.errorMessage(err), 'error');
            if (state.preview) { state.preview.destroy(); state.preview = null; }
          });
          if (canInteract) state.preview = handle;
          // 测试钩子：harness 需要驱动"帧发来问候"这一步，而帧的 window 对父窗口是跨源
          // 对象（连 dispatchEvent 都会抛 SecurityError），所以只能拿到端口句柄直接调。
          window.__aigwPreview = handle;
        });
        bar.append(preview);
      }
      wrap.prepend(bar);
    });
  }

  // renderFormBlock builds one inline form and puts it in place of its code block.
  //
  // A form is a rendering of its message, not a piece of state: the only thing carried across a
  // re-render is what the operator typed and has not submitted yet (`state.formDrafts`). That is
  // what makes a reload, a session switch, or the post-turn transcript rebuild harmless.
  function renderFormBlock(host, wrap, message, code, source) {
    const blockIndex = code.getAttribute('data-block-index') || '0';
    const formKey = (message.id || 'live') + ':' + blockIndex;
    const parsed = parseFormSpec(source, blockIndex);
    if (parsed.error) {
      // Same contract as a chart spec the renderer cannot draw: keep the raw block visible and
      // say what is wrong with it, instead of rendering half a form.
      wrap.after(el('div', { class: 'notice form-note-error', text: '表单未渲染：' + parsed.error }));
      return;
    }
    const spec = parsed.spec;
    const draft = state.formDrafts.get(formKey);
    if (draft) {
      for (const field of spec.fields) {
        if (!field || !field.name) continue;
        if (!Object.prototype.hasOwnProperty.call(draft, field.name)) continue;
        field.value = draft[field.name];
        if (field.type === 'checkbox') field.checked = draft[field.name] === true;
      }
    }
    const lines = source.split('\n');
    const head = lines.slice(0, 2).join('\n');

    const form = renderForm(spec, {
      onSubmit: (event) => onFormSubmit(form, formKey, event),
    });
    form.node.__aigwForm = form;
    wrap.replaceWith(form.node);
    // The transcript keeps the spec itself one click away. The form is a rendering; this is what
    // the model actually sent, which is what someone checks when a field is not what they meant.
    form.node.append(el('details', { class: 'form-source' }, [
      el('summary', { text: '查看表单规格' }),
      el('pre', { class: 'md-pre' }, [
        el('code', { class: 'md-code-block', text: head + (lines.length > 2 ? '\n…' : '') }),
      ]),
    ]));
  }

  // onFormSubmit is the console's side of "the operator answered the form". It becomes an
  // ordinary question in this conversation: same endpoint, same billing, same transcript. There
  // is no second path, which is why a form submission shows up in the request log exactly like
  // something typed by hand.
  function onFormSubmit(form, formKey, event) {
    if (!state.session) return;
    if (!state.session.account_id || !state.session.api_key_id) {
      form.setStatus('这个会话还没有绑定计费 Key，提交不会产生请求', 'error');
      return;
    }
    // The submission is about to become a message, so the draft has served its purpose: keeping
    // it would refill the form with values the transcript already contains.
    state.formDrafts.delete(formKey);
    const item = { event, form, formKey };
    if (state.running) {
      if (state.queue.length >= 5) {
        form.setStatus('还有 5 条提交在排队，请等模型答完', 'error');
        return;
      }
      state.queue.push(item);
      form.setStatus(`模型还在回答；已排队 ${state.queue.length} 条提交，本轮结束后自动发出`);
      return;
    }
    sendFormEvent(item);
  }

  // toolCallNames lists the management calls this answer actually made. The chart footer
  // shows them so a reader can check the numbers against the calls that produced them.
  function toolCallNames(message) {
    if (!message || !message.parts) return [];
    return message.parts.filter((part) => part.type === 'tool_call').map((part) => part.name);
  }

  function renderComposer() {
    const input = el('textarea', { class: 'chat-input', rows: '3', placeholder: '问点什么…（Enter 发送，Shift+Enter 换行）' });
    const send = el('button', { class: 'btn btn-primary', text: '发送' });
    const stop = el('button', { class: 'btn btn-danger', text: '停止', disabled: !state.running });
    const plus = el('button', { class: 'btn', text: '＋', title: '创建技能 / 选择技能' });
    const hint = el('div', { class: 'muted chat-hint', text: '每步模型调用都按所选 Key 计费' });
    const bar = el('div', { class: 'chat-composer-bar' }, [plus, hint, el('span', { class: 'spacer' }), stop, send]);

    send.addEventListener('click', () => submit(input.value));
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && !ev.shiftKey) { ev.preventDefault(); submit(input.value); }
    });
    stop.addEventListener('click', () => {
      if (state.controller) state.controller.abort();
      state.notice = '已请求停止，已生成的内容会保留';
    });
    plus.addEventListener('click', (ev) => openPlusMenu(ev.currentTarget));
    return el('div', { class: 'chat-composer' }, [input, bar]);
  }

  function openPlusMenu(anchor) {
    document.querySelectorAll('.plus-menu').forEach((node) => node.remove());
    const menu = el('div', { class: 'plus-menu' });
    const create = el('button', { class: 'plus-item', text: '创建技能（由本会话生成）' });
    create.addEventListener('click', () => { menu.remove(); createSkillFromSession(); });
    menu.append(create, el('div', { class: 'plus-sep' }));
    const list = el('div', { class: 'plus-list' });
    if (state.skills.length > 6) {
      const filter = el('input', { class: 'plus-filter', placeholder: '过滤技能…' });
      filter.addEventListener('input', () => paint(filter.value));
      list.append(filter);
    }
    const loadBtn = el('button', { class: 'plus-item', text: '管理技能…' });
    loadBtn.addEventListener('click', () => navigate('/skills'));
    function paint(keyword) {
      [...list.querySelectorAll('.plus-skill')].forEach((node) => node.remove());
      const active = new Set(state.session ? (state.session.skill_ids || []) : []);
      const needle = (keyword || '').toLowerCase();
      for (const skill of state.skills) {
        if (needle && !(skill.name + ' ' + (skill.description || '')).toLowerCase().includes(needle)) continue;
        const row = el('label', { class: 'plus-item plus-skill', title: skill.description || '' }, [
          el('input', { type: 'checkbox', checked: active.has(skill.id) }),
          el('span', { text: skill.name }),
        ]);
        row.querySelector('input').addEventListener('change', async (ev) => {
          const ids = new Set(active);
          if (ev.target.checked) ids.add(skill.id); else ids.delete(skill.id);
          await setSkills([...ids]);
        });
        list.append(row);
      }
    }
    paint('');
    menu.append(list, el('div', { class: 'plus-sep' }), loadBtn);
    anchor.parentElement.append(menu);
    const close = (ev) => {
      if (!menu.contains(ev.target)) { menu.remove(); document.removeEventListener('click', close); }
    };
    setTimeout(() => document.addEventListener('click', close), 0);
  }

  async function setSkills(ids) {
    try {
      const updated = await api.patch('/chat/sessions/' + encodeURIComponent(state.session.id), { skill_ids: ids });
      state.session.skill_ids = updated.skill_ids || ids;
      await loadSkills();
      await openSession(updated.id);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  async function createSkillFromSession() {
    if (!state.messages.length) { toast('先问一个问题，再把它沉淀成技能', 'error'); return; }
    try {
      toast('正在让模型整理技能草稿…（这次调用也会计费）');
      const draft = await api.post('/chat/sessions/' + encodeURIComponent(state.session.id) + '/skill-draft', {});
      // The draft stays in this browser: it is unsaved, and the server has no reason to
      // keep a second copy of something the operator may throw away.
      try {
        window.sessionStorage.setItem('aigw.chat.draft', JSON.stringify({
          name: draft.name, description: draft.description, instructions: draft.instructions,
          note: draft.note, session_id: state.session.id,
        }));
      } catch (err) { /* private mode: the skills page will simply open empty */ }
      navigate('/skills?new=1');
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  // -------------------------------------------------------------------------
  // one question
  // -------------------------------------------------------------------------

  // submit is the composer's entry point. Everything below it is the turn itself, which the
  // interactive preview uses too: an event from a generated page is a question in this
  // conversation, billed and stored the same way, so it must not have its own request path.
  // submit is a question typed by hand. A live inline form is *also* told what the model is
  // writing: someone who asked a follow-up while a form sat on screen should not have to guess
  // whether the answer belongs to the form or to their question.
  async function submit(content) {
    if (!state.session) { toast('先新建一个会话', 'error'); return; }
    const form = lastLiveForm();
    let streamed = '';
    await runTurn(content, form ? {
      onDelta: (frame) => {
        if (frame.type !== 'text') return;
        streamed += frame.delta || '';
        form.setStatus(streamed.length > 200 ? '…' + streamed.slice(-200) : streamed);
      },
      onFinish: (result) => {
        form.setBusy(false);
        if (result.error) { form.setStatus('本轮失败：' + result.error, 'error'); return; }
        // Parked for the same reason a form submission's directive is: the transcript is about to
        // be rebuilt, so anything applied to this node now would be discarded with it.
        state.pendingFormOps = { formId: form.spec.id, ops: result.ops || [] };
        if (!result.ops || !result.ops.length) {
          form.setStatus('本轮结束（' + result.status + '）。继续填写或提交即可。');
        }
      },
    } : undefined);
  }

  // lastLiveForm is the last inline form on screen, or nothing.
  //
  // It is read out of the DOM rather than kept in a map of handles, because the transcript is
  // rebuilt after every turn: a handle kept across that rebuild is a detached ghost, and a map of
  // ghosts has to be swept. The document is the source of truth for what is on screen, so this
  // asks it. Only one turn can be in flight, so at most one form can be "the one being answered".
  function lastLiveForm() {
    const nodes = main.querySelectorAll('.chat-form');
    const node = nodes[nodes.length - 1];
    return (node && node.__aigwForm) || null;
  }

  // runTurn posts one question and folds the stream into the transcript. When the answer
  // mentions an open interactive preview, the same events are relayed into that page so it
  // updates in place instead of being reloaded.
  async function runTurn(content, { onDelta, onFinish } = {}) {
    if (state.running) { toast('上一条还在生成', 'error'); return false; }
    const text = (content || '').trim();
    if (!text) return false;
    if (!state.session) { toast('先新建一个会话', 'error'); return false; }
    if (!state.session.account_id || !state.session.api_key_id) {
      toast('这个会话还没有绑定计费 Key', 'error');
      return false;
    }

    state.running = true;
    paintTimer = null;
    state.controller = new AbortController();
    state.turnID = 'turn_' + Math.random().toString(36).slice(2) + Date.now().toString(36);
    state.notice = '';
    // One turn id for this attempt: a retry after a transport error reuses it, and the server
    // answers with what already happened instead of charging twice.
    const turnID = state.turnID;
    const pending = { id: 'live', role: 'assistant', parts: [], status: 'ok', content: '' };
    state.messages = state.messages.concat([{ id: 'local-user', role: 'user', content: text }]);
    renderMain();
    const streamHost = main.querySelector('.chat-stream');
    let live = renderMessage(pending);
    streamHost.append(live);
    streamHost.scrollTop = streamHost.scrollHeight;
    const repaint = (immediate) => {
      const paint = () => {
        paintTimer = null;
        const fresh = renderMessage(pending);
        live.replaceWith(fresh);
        live = fresh;
        streamHost.scrollTop = streamHost.scrollHeight;
      };
      if (paintTimer) clearTimeout(paintTimer);
      if (immediate) paint();
      else paintTimer = setTimeout(paint, 50);
    };

    // The preview sees the answer as it is written. It is sent as a delta rather than a final
    // blob because a two-step investigation can take half a minute, and a page that stays
    // silent for that long reads as broken.
    const relay = (event) => {
      if (!onDelta) return;
      if (event.type === 'text') onDelta({ type: 'text', delta: event.delta || '' });
      else if (event.type === 'busy') onDelta({ type: 'busy', busy: true });
    };
    if (onDelta) onDelta({ type: 'busy', busy: true });

    let status = 'completed';
    let failure = '';
    try {
      await streamPost('/chat/sessions/' + encodeURIComponent(state.session.id) + '/turns', {
        turn_id: turnID, content: text,
      }, {
        signal: state.controller.signal,
        onEvent: (event) => {
          applyEvent(event, pending, repaint);
          if (event.type === 'error') failure = event.message || '生成失败';
          relay(event);
        },
      });
    } catch (err) {
      if (err && err.name === 'AbortError') {
        state.notice = '已停止生成';
        status = 'aborted';
      } else {
        state.notice = api.errorMessage(err);
        failure = state.notice;
        status = 'failed';
        toast(state.notice, 'error');
      }
    } finally {
      if (paintTimer) { clearTimeout(paintTimer); paintTimer = null; }
      state.running = false;
      state.controller = null;
      if (!failure && pending.status === 'failed') { failure = pending.error || '生成失败'; }
      if (failure) status = 'failed';
      // The directive and any new document version come out of the whole answer, which is
      // exactly what the live bubble accumulated.
      const reply = uiReplyParts(codeBlocksOf(pending));
      if (onFinish) {
        onFinish({
          status,
          error: failure,
          ops: reply.ops,
          opsError: reply.error,
          newVersion: reply.newVersion,
          blocks: codeBlocksOf(pending),
        });
      }
      await loadSessions();
      await openSession(state.session.id);
      // A directive for an inline form is applied *after* the refresh, and this ordering is the
      // whole reason: openSession rebuilds the transcript from the server, so a directive applied
      // inside onFinish would patch a node that is about to be thrown away — the operator would
      // see the model's update flash and vanish, which is worse than not applying it at all.
      applyPendingFormOps();
      // A submission that arrived while this turn was in flight runs now, so a form does not
      // silently lose the second click.
      drainQueue();
    }
    return true;
  }

  // -------------------------------------------------------------------------
  // events from a generated interface
  // -------------------------------------------------------------------------

  // onUIEvent is the bridge's callback: a form was submitted inside an interactive preview, or
  // an element carrying data-aigw-send was clicked. It becomes a question in this conversation.
  function onUIEvent(event) {
    if (!state.session) return;
    if (state.running) {
      if (state.queue.length >= 5) {
        toast('还有 5 条界面提交在排队，请等模型答完', 'error');
        return;
      }
      state.queue.push({ event, preview: state.preview });
      state.notice = `模型还在回答；已排队 ${state.queue.length} 条界面提交，本轮结束后自动发出。`;
      renderMain();
      return;
    }
    sendUIEvent(event, state.preview);
  }

  // drainQueue sends the submissions that arrived while a turn was in flight, oldest first.
  //
  // Each queued item remembers the thing that asked — an open preview or an inline form — and
  // an item whose target is gone is dropped rather than re-routed: the transcript already holds
  // everything that was paid for, and patching an answer into a form that did not ask for it
  // would attribute the model's words to the wrong question.
  function drainQueue() {
    if (state.running || !state.queue.length) return;
    const item = state.queue[0];
    if (item.form) {
      state.queue.shift();
      sendFormEvent(item);
      return;
    }
    if (!item.preview || !item.preview.isOpen()) {
      state.queue.shift();
      drainQueue();
      return;
    }
    state.queue.shift();
    sendUIEvent(item.event, item.preview);
  }

  // applyPendingFormOps applies the directive of the turn that just finished to the form that
  // asked for it. It runs after the transcript has been rebuilt, and it looks its target up in
  // the *live* document rather than trusting the handle the turn started with: the rebuild
  // replaced that node, so the handle is a detached ghost by now. The form's id is derived from
  // its block, so the same id names the same form before and after the rebuild.
  function applyPendingFormOps() {
    const pending = state.pendingFormOps;
    state.pendingFormOps = null;
    if (!pending || !pending.ops || !pending.ops.length) return;
    const node = main.querySelector('.chat-form#form_' + pending.formId);
    const form = node && node.__aigwForm;
    if (!form) return;
    const summary = describeFormOps(form.apply(pending.ops));
    if (summary) form.setStatus(summary);
    else form.setStatus('本轮完成');
  }

  // sendFormEvent turns one inline-form submission into a billed turn, and streams the answer
  // back into the form that asked for it: text deltas land in the form's status line while the
  // model writes, and the `ui` directive at the end of the turn patches the fields in place.
  //
  // The form is passed in rather than looked up by id afterwards, for the same reason the
  // preview is: by the time the answer arrives the transcript may have been rebuilt, and an
  // answer must not be patched into a form that did not ask for it.
  function sendFormEvent({ event, form }) {
    if (!form) return;
    form.setBusy(true, '模型正在处理…');
    let streamed = '';
    runTurn(uiEventContent(event), {
      onDelta: (frame) => {
        if (frame.type !== 'text') return;
        streamed += frame.delta || '';
        form.setStatus(streamed.length > 200 ? '…' + streamed.slice(-200) : streamed);
      },
      onFinish: (result) => {
        form.setBusy(false);
        if (result.error) {
          form.setStatus('本轮失败：' + result.error, 'error');
          return;
        }
        // The fields are patched after the refresh, not here: `result.ops` is parked for
        // applyPendingFormOps. A directive that came back with an error is reported now, because
        // it will never have a target worth retrying against.
        state.pendingFormOps = { formId: form.spec.id, ops: result.ops || [] };
        if (result.opsError) form.setStatus('回答里的界面指令无法应用：' + result.opsError, 'error');
        else if (!result.ops || !result.ops.length) {
          form.setStatus('本轮结束（' + result.status + '）。继续填写或提交即可。');
        }
      },
    });
  }

  // sendUIEvent turns one submission into a billed turn. The preview it belongs to is passed in
  // rather than read from `state`: by the time the answer arrives the operator may have opened
  // another one, and an answer must not be patched into a page that did not ask for it.
  function sendUIEvent(event, preview) {
    const content = uiEventContent(event);
    const alive = () => !!(preview && preview.isOpen());
    runTurn(content, {
      onDelta: (frame) => { if (alive()) preview.reply(frame); },
      onFinish: (result) => {
        if (!alive()) return;
        preview.reply({
          type: 'done',
          status: result.error ? 'failed' : result.status,
          error: result.error,
          ops: result.ops,
          opsError: result.opsError,
          newVersion: result.newVersion,
        });
      },
    });
  }
  // applyEvent folds one SSE event into the live bubble. Text deltas are appended to the
  // current text part so the stream reads like DSH's chat rather than a series of blobs.
  function applyEvent(event, pending, repaint) {
    switch (event.type) {
      case 'text': {
        const last = pending.parts[pending.parts.length - 1];
        if (last && last.type === 'text') last.text += event.delta || '';
        else pending.parts.push({ type: 'text', text: event.delta || '' });
        repaint(false);
        break;
      }
      case 'reasoning':
        pending.reasoning = (pending.reasoning || '') + (event.delta || '');
        repaint(false);
        break;
      case 'tool_call': {
        let call = pending.parts.find((part) => part.type === 'tool_call' && part.id === event.call_id);
        if (!call) {
          call = { type: 'tool_call', id: event.call_id, name: event.tool ? event.tool.name : '', arguments: '', status: 'pending' };
          pending.parts.push(call);
        }
        if (event.delta) call.arguments += event.delta;
        if (event.tool && event.tool.name) call.name = event.tool.name;
        repaint(true);
        break;
      }
      case 'tool_result': {
        const call = pending.parts.find((part) => part.type === 'tool_call' && part.id === event.call_id);
        if (call && event.tool) {
          call.result = event.tool.result;
          call.is_error = event.tool.is_error;
          call.status = event.tool.status;
          call.name = event.tool.name || call.name;
        }
        repaint(true);
        break;
      }
      case 'usage':
        if (event.usage) {
          pending.tokens_in = (pending.tokens_in || 0) + (event.usage.input_tokens || 0);
          pending.tokens_out = (pending.tokens_out || 0) + (event.usage.output_tokens || 0);
        }
        break;
      case 'notice':
        state.notice = event.notice || '';
        if (event.level === 'error') toast(event.notice, 'error');
        break;
      case 'error':
        pending.status = 'failed';
        pending.error = event.message || '生成失败';
        state.notice = pending.error;
        break;
      case 'message':
        if (event.message) state.messages[state.messages.length - 1] = event.message;
        break;
      default:
        break;
    }
  }

  // The live bubble is re-rendered from the accumulated parts on a 50ms timer: a fast model
  // emits hundreds of deltas a second, and rebuilding Markdown for each one would make the
  // page crawl. Tool events repaint immediately because they are rare and structural.


  function prettyJSON(value) {
    if (!value) return '';
    try { return JSON.stringify(JSON.parse(value), null, 2); } catch (err) { return String(value); }
  }

  function micros(value) {
    if (value === undefined || value === null) return '';
    return (value / 1e6).toFixed(6);
  }

  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      toast('已复制');
    } catch (err) {
      toast('复制失败，请手动选择', 'error');
    }
  }
}
