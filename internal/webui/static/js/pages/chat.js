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
import { openPreview } from './chat_artifact.js';

// paintTimer throttles the live bubble's Markdown rebuild. It is declared at module scope
// rather than next to the code that uses it, because submit() assigns it during the stream
// and a page can be re-rendered while an earlier turn is still finishing.
let paintTimer = null;

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

  const destroy = () => { if (state.controller) state.controller.abort(); };
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

  function renderMain() {
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
    if (message.role === 'user') {
      return el('div', { class: 'chat-msg user' }, [
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
    for (const part of message.parts || []) {
      if (part.type === 'text') body.append(renderRichText(part.text, message));
      else body.append(renderToolCard(part));
    }
    if (!(message.parts || []).length && message.content) body.append(renderRichText(message.content, message));
    body.append(renderFooter(message));
    return el('div', { class: 'chat-msg assistant' }, [body]);
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
        const preview = el('button', { class: 'btn btn-ghost btn-xs', text: '预览' });
        preview.addEventListener('click', () => openPreview({
          sessionId: state.session.id,
          key: (message.id || 'live-' + (state.turnID || 'turn')) + ':' + code.getAttribute('data-block-index'),
          format: lang,
          body: source,
          title: state.session.title || '预览',
        }));
        bar.append(preview);
      }
      wrap.prepend(bar);
    });
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

  async function submit(content) {
    if (state.running) { toast('上一条还在生成', 'error'); return; }
    const text = (content || '').trim();
    if (!text) return;
    if (!state.session) { toast('先新建一个会话', 'error'); return; }
    if (!state.session.account_id || !state.session.api_key_id) { toast('这个会话还没有绑定计费 Key', 'error'); return; }

    state.running = true;
    paintTimer = null;
    state.controller = new AbortController();
    state.turnID = 'turn_' + Math.random().toString(36).slice(2) + Date.now().toString(36);
    state.notice = '';
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

    try {
      await streamPost('/chat/sessions/' + encodeURIComponent(state.session.id) + '/turns', {
        turn_id: state.turnID, content: text,
      }, {
        signal: state.controller.signal,
        onEvent: (event) => applyEvent(event, pending, repaint),
      });
    } catch (err) {
      if (err && err.name === 'AbortError') {
        state.notice = '已停止生成';
      } else {
        state.notice = api.errorMessage(err);
        toast(state.notice, 'error');
      }
    } finally {
      if (paintTimer) { clearTimeout(paintTimer); paintTimer = null; }
      state.running = false;
      state.controller = null;
      await loadSessions();
      await openSession(state.session.id);
    }
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
