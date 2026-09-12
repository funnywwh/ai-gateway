// HTML5 / SVG previews.
//
// A preview is a model-authored document, so it never touches the console's own DOM: it is
// stored server-side, handed back a short-lived ticket, and loaded into a sandboxed iframe
// on its own URL. The frame has no allow-same-origin, so it cannot read the console's
// cookies or storage, and the server's Content-Security-Policy keeps it in an opaque origin
// even if the URL is opened in a tab.
//
// Since M34 an HTML preview can also be interactive: the server injects a bridge script into
// the served document, the operator fills in a form the model wrote, and the submission comes
// back through a MessagePort to become a normal question in the same conversation. The
// toolbar above the frame is the console's side of that channel — it reports the bridge state,
// counts the submissions, and carries the stop button, so the operator always has a way out of
// a page that keeps talking.
//
// The console cannot see inside the frame (that is the point of the sandbox), so it cannot
// read the frame's errors either. What it can do is look at the source before asking for the
// preview and warn when the page references external resources, which the default policy
// blocks — a warning is honest here, a promise that everything will render is not.

import { api } from '../api.js';
import { el, toast } from '../ui.js';
import { createUIPort, parseUISpec, UI_LIMITS } from './chat_ui.js';

const EXTERNAL = [
  /<script[^>]+src\s*=\s*["']?https?:/i,
  /<link[^>]+href\s*=\s*["']?https?:/i,
  /<img[^>]+src\s*=\s*["']?https?:/i,
  /@import\s+(url\()?["']?https?:/i,
  /url\(\s*["']?https?:/i,
  /<iframe[^>]+src\s*=\s*["']?https?:/i,
];

function referencesExternal(body) {
  return EXTERNAL.some((pattern) => pattern.test(body || ''));
}

// The interactive channel needs no secret, and that is deliberate.
//
// Two earlier designs tried to hand one over, and both failed for the same underlying reason: a
// sandboxed document is an opaque origin, so the console cannot read anything out of the frame
// (frame.contentDocument is null — that cost this feature one full outage), and a nonce that
// would have hidden a token in the URL makes browsers ignore `script-src 'unsafe-inline'`, which
// silently breaks the model's own inline scripts. What actually authenticates the channel is
// structural: the offered MessagePort can only be received by the window that loaded this
// document, the injected script refuses to run when it is not the top document, and the host
// accepts a hello only from that exact frame window.
//
// So this side only has to remember: never read into the frame, and never take a port from a
// message whose shape or source is even slightly off.

export async function openPreview({ sessionId, key, format, body, title, interactive, onSubmit, onStop, onClosed }) {
  const root = document.getElementById('modal-root');
  const status = el('div', { class: 'muted' });
  const frameHost = el('div', { class: 'preview-host' });
  const badge = el('span', { class: 'badge bridge-badge', text: '只读' });
  const counter = el('span', { class: 'muted bridge-counter' });
  let url = null;
  let port = null;
  let frame = null;
  let stopped = false;
  // newVersion is the document the model offered in its latest answer. It is only ever loaded
  // when the operator presses the button: replacing the frame throws away what they typed.
  let newVersion = null;

  const backdrop = el('div', { class: 'modal-backdrop' });
  const dialog = el('div', { class: 'modal modal-wide' }, [
    el('div', { class: 'toolbar' }, [
      el('h2', { text: title || '预览', style: 'margin:0;flex:1' }),
      badge,
      counter,
      el('span', { class: 'badge', text: format.toUpperCase() }),
    ]),
    frameHost,
    status,
  ]);
  const actions = el('div', { class: 'modal-actions' });
  const close = el('button', { class: 'btn', text: '关闭', onclick: () => teardown() });
  const copy = el('button', { class: 'btn btn-ghost', text: '复制源码' });
  copy.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(body); toast('已复制'); }
    catch (err) { toast('复制失败', 'error'); }
  });
  const openTab = el('button', { class: 'btn btn-ghost', text: '新标签打开', disabled: true });
  openTab.addEventListener('click', () => { if (url) window.open(url, '_blank', 'noopener'); });
  const stop = el('button', { class: 'btn btn-danger', text: '停止生成', disabled: true });
  stop.addEventListener('click', () => {
    stopped = true;
    status.textContent = '已请求停止：已经生成的内容与已经产生的费用照常保留。';
    if (onStop) onStop();
  });
  const reload = el('button', { class: 'btn', text: '重新加载', disabled: true });
  reload.addEventListener('click', () => { load().catch((err) => fail(err)); });
  const upgrade = el('button', { class: 'btn btn-primary', text: '加载新版本', hidden: true });
  upgrade.addEventListener('click', () => {
    if (!newVersion) return;
    // Replacing the document throws away whatever the operator typed into the old one, which
    // is why this is a button and not something the model can trigger.
    body = newVersion;
    newVersion = null;
    upgrade.hidden = true;
    load().catch((err) => fail(err));
  });
  actions.append(copy, reload, upgrade, openTab, stop, close);
  dialog.append(actions);
  backdrop.append(dialog);
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) teardown(); });
  root.append(backdrop);

  if (referencesExternal(body)) {
    dialog.insertBefore(el('div', {
      class: 'notice',
      text: '这份页面引用了外部资源（CDN、外链图片或字体）。默认策略会拦截它们，页面可能显示不全；' +
        '要允许联网需要管理员在配置里打开 chat.artifact_allow_network。',
    }), frameHost);
  }

  // -------------------------------------------------------------------------

  function teardown() {
    if (port) port.destroy();
    port = null;
    backdrop.remove();
    if (onClosed) onClosed();
  }

  function fail(err) {
    status.textContent = api.errorMessage(err);
    status.className = 'toast error';
    badge.className = 'badge bridge-badge';
    badge.textContent = '不可交互';
    stop.disabled = true;
  }

  // setState paints the toolbar from the port's state. Every transition the operator can
  // observe comes through here, so the badge and the counter cannot disagree with the channel.
  function setState(state) {
    counter.textContent = state.events ? `已提交 ${state.events}/${UI_LIMITS.maxEvents}` : '';
    if (state.status === 'handshaking') {
      badge.className = 'badge bridge-badge';
      badge.textContent = interactive ? '等待页面握手…' : '只读';
    } else if (state.status === 'ready') {
      badge.className = 'badge bridge-badge ok';
      badge.textContent = '已连接';
      stop.disabled = false;
    } else if (state.status === 'blocked') {
      badge.className = 'badge bridge-badge off';
      badge.textContent = '不可交互';
      stop.disabled = true;
    } else if (state.status === 'closed') {
      badge.className = 'badge bridge-badge';
      badge.textContent = '已断开';
      stop.disabled = true;
    }
    if (state.error) status.textContent = state.error;
  }

  async function load() {
    if (port) { port.destroy(); port = null; }
    if (frame) { frame.remove(); frame = null; }
    stopped = false;
    stop.disabled = true;
    status.className = 'muted';
    status.textContent = '正在准备预览…';
    try {
      const payload = await api.post('/chat/sessions/' + encodeURIComponent(sessionId) + '/artifacts', {
        key, format, body, title, bridge: !!interactive,
      });
      const base = payload.url + '?ticket=' + encodeURIComponent(payload.ticket);
      url = base;
      openTab.disabled = false;
      frame = el('iframe', {
        class: 'preview-frame',
        // allow-scripts only, and only for HTML. Note what is *not* here: no allow-same-origin
        // (the frame must not reach the console), no allow-forms (a native form submission is a
        // navigation, and the bridge intercepts submit anyway), no allow-popups.
        sandbox: format === 'svg' ? '' : 'allow-scripts',
        src: interactive ? base + '&bridge=1' : base,
        title: title || '预览',
        referrerpolicy: 'no-referrer',
      });
      frameHost.append(frame);
      reload.disabled = false;
      if (interactive) {
        port = createUIPort({
          sessionId, frame,
          onState: setState,
          onEvent: (event) => { if (!stopped && onSubmit) onSubmit(event); },
          onApply: (ops, frameData) => {
            // The page applies the directive itself (it owns its DOM); the console only relays
            // and reflects the outcome in the toolbar.
            if (frameData && frameData.status) status.textContent = '本轮结束：' + frameData.status;
          },
          onError: (message) => { status.textContent = message; },
        });
        setState(port.state);
        status.textContent = '预览链接会在几分钟后失效。这个页面可以把表单提交回本会话（每次提交都计费）；' +
          '已经填过的内容在模型回复后原地更新，不会重载。';
      } else {
        badge.className = 'badge bridge-badge';
        badge.textContent = format === 'svg' ? '只读' : '只读预览';
        status.textContent = '预览链接会在几分钟后失效；页面运行在独立沙箱里，无法访问控制台的登录状态。' +
          (format === 'html' && !interactive
            ? '（本会话没有绑定计费 Key 或交互预览已关闭，页面里的按钮不会产生请求。）'
            : '');
      }
    } catch (err) {
      fail(err);
    }
  }

  await load();

  // handleReply is what the chat page calls for every event of the answer this preview
  // triggered. Three kinds of frame travel back into the page:
  //   - a delta, so the answer appears where the operator is looking while it streams;
  //   - the busy/idle state, so a page that declared [data-aigw-status] can say so;
  //   - the directive, applied by the page itself at the end of the turn.
  function handleReply(event) {
    if (!port) return;
    switch (event.type) {
      case 'text':
        port.send({ k: 'd', v: event.delta || '' });
        return;
      case 'done':
        port.send({
          k: 'done',
          status: event.status || 'completed',
          error: event.error || '',
          ops: event.ops || [],
        });
        if (event.error) {
          status.textContent = '本轮失败：' + event.error;
          return;
        }
        if (event.opsError) {
          status.textContent = '本轮回答里的界面指令无法应用：' + event.opsError;
        }
        if (event.newVersion) {
          newVersion = event.newVersion;
          upgrade.hidden = false;
          status.textContent = '本轮回答给出了新的整页版本；点「加载新版本」会重新加载页面（会清空已填内容）。';
        } else {
          status.textContent = '本轮结束（' + (event.status || 'completed') + '）。继续填写或提交即可。';
        }
        return;
      case 'busy':
        port.send({ k: 'state', v: event.busy ? 'busy' : 'idle' });
        stop.disabled = !event.busy;
        return;
      default:
        return;
    }
  }

  return { destroy: teardown, reply: handleReply, isOpen: () => backdrop.isConnected };
}

// uiReplyParts turns the code blocks of one assistant answer into what an open preview needs:
// the directive to apply, and the document version to offer. It is exported because the
// decision is testable on its own — no model, no frame, no port.
//
// The directive is *parsed* here rather than read off the block: a code block is text, and the
// operations only exist once that text is read as JSON. An unusable spec comes back with an
// error and no operations, so the caller reports it instead of sending an empty patch.
export function uiReplyParts(blocks) {
  let ops = [];
  let error = '';
  let newVersion = null;
  for (const block of blocks || []) {
    if (!block) continue;
    if (block.lang === 'ui') {
      const parsed = parseUISpec(block.text || '');
      ops = parsed.ops;
      error = parsed.error;
    } else if (block.lang === 'html' && block.text) {
      newVersion = block.text;
    }
  }
  return { ops, error, newVersion };
}
