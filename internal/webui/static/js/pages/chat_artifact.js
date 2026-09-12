// HTML5 / SVG previews.
//
// A preview is a model-authored document, so it never touches the console's own DOM: it is
// stored server-side, handed back a short-lived ticket, and loaded into a sandboxed iframe
// on its own URL. The frame has no allow-same-origin, so it cannot read the console's
// cookies or storage, and the server's Content-Security-Policy keeps it in an opaque origin
// even if the URL is opened in a tab.
//
// The console cannot see inside the frame (that is the point of the sandbox), so it cannot
// read the frame's errors either. What it can do is look at the source before asking for the
// preview and warn when the page references external resources, which the default policy
// blocks — a warning is honest here, a promise that everything will render is not.

import { api } from '../api.js';
import { el, toast } from '../ui.js';

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

export async function openPreview({ sessionId, key, format, body, title }) {
  const root = document.getElementById('modal-root');
  const status = el('div', { class: 'muted' });
  const frameHost = el('div', { class: 'preview-host' });
  let url = null;

  const backdrop = el('div', { class: 'modal-backdrop' });
  const dialog = el('div', { class: 'modal modal-wide' }, [
    el('div', { class: 'toolbar' }, [
      el('h2', { text: title || '预览', style: 'margin:0;flex:1' }),
      el('span', { class: 'badge', text: format.toUpperCase() }),
    ]),
    frameHost,
    status,
  ]);
  const actions = el('div', { class: 'modal-actions' });
  const close = el('button', { class: 'btn', text: '关闭', onclick: () => backdrop.remove() });
  const copy = el('button', { class: 'btn btn-ghost', text: '复制源码' });
  copy.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(body); toast('已复制'); }
    catch (err) { toast('复制失败', 'error'); }
  });
  const openTab = el('button', { class: 'btn btn-ghost', text: '新标签打开', disabled: true });
  openTab.addEventListener('click', () => { if (url) window.open(url, '_blank', 'noopener'); });
  actions.append(copy, openTab, close);
  dialog.append(actions);
  backdrop.append(dialog);
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) backdrop.remove(); });
  root.append(backdrop);

  if (referencesExternal(body)) {
    dialog.insertBefore(el('div', {
      class: 'notice',
      text: '这份页面引用了外部资源（CDN、外链图片或字体）。默认策略会拦截它们，页面可能显示不全；' +
        '要允许联网需要管理员在配置里打开 chat.artifact_allow_network。',
    }), frameHost);
  }

  status.textContent = '正在准备预览…';
  try {
    const payload = await api.post('/chat/sessions/' + encodeURIComponent(sessionId) + '/artifacts', {
      key, format, body, title,
    });
    url = payload.url + '?ticket=' + encodeURIComponent(payload.ticket);
    // allow-scripts only for HTML; an SVG chart needs no scripting at all.
    const frame = el('iframe', {
      class: 'preview-frame',
      sandbox: format === 'svg' ? '' : 'allow-scripts',
      src: url,
      title: title || '预览',
      referrerpolicy: 'no-referrer',
    });
    frameHost.append(frame);
    openTab.disabled = false;
    status.textContent = '预览链接会在几分钟后失效；页面运行在独立沙箱里，无法访问控制台的登录状态。';
  } catch (err) {
    status.textContent = api.errorMessage(err);
    status.className = 'toast error';
  }
}
