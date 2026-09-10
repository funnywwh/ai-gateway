import { api, me, login, logout } from './api.js';
import { el, toast, clear } from './ui.js';
import { startRouter, renderNav, loadPage, navigate, currentRoute } from './router.js';

const app = document.getElementById('app');
let session = null;

// A 401 from any page drops the session and returns to the login screen; the
// server is the single source of truth about who is signed in.
window.addEventListener('aigw:unauthorized', () => {
  session = null;
  renderLogin('会话已过期，请重新登录');
});

async function boot() {
  try {
    session = await me();
  } catch (err) {
    renderLogin();
    return;
  }
  renderShell();
}

function renderLogin(notice) {
  clear(app);
  const username = el('input', { type: 'text', value: 'admin', autocomplete: 'username' });
  const password = el('input', { type: 'password', autocomplete: 'current-password' });
  const submit = el('button', { class: 'btn btn-primary', text: '登录' });
  const error = el('div', { class: 'muted', text: notice || '' });
  async function attempt() {
    submit.disabled = true;
    try {
      await login(username.value, password.value);
      session = await me();
      renderShell();
    } catch (err) {
      error.textContent = api.errorMessage(err);
      error.className = 'toast error';
      submit.disabled = false;
    }
  }
  submit.addEventListener('click', attempt);
  password.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') attempt(); });
  app.append(el('div', { class: 'login' }, [el('section', { class: 'card' }, [
    el('h2', { text: 'AI Gateway 控制台' }),
    el('label', { class: 'field' }, [el('span', { text: '用户名' }), username]),
    el('label', { class: 'field' }, [el('span', { text: '口令' }), password]),
    error, submit,
  ])]));
  password.focus();
}

function renderShell() {
  clear(app);
  const nav = el('nav');
  const title = el('h1');
  const actions = el('div', { class: 'actions' });
  const page = el('section', { class: 'page' });
  const whoami = el('span', { text: session.username + ' · ' + session.role });
  const logoutBtn = el('button', { class: 'btn btn-ghost', text: '退出' });
  logoutBtn.addEventListener('click', async () => {
    try { await logout(); } catch (err) { /* the cookie is cleared client-side anyway */ }
    session = null;
    renderLogin('已退出');
  });
  app.append(
    el('div', { class: 'app' }, [
      el('aside', { class: 'sidebar' }, [el('div', { class: 'brand', text: 'AI Gateway' }), nav,
        el('div', { class: 'sidebar-foot' }, [whoami, logoutBtn])]),
      el('main', { class: 'main' }, [el('header', { class: 'topbar' }, [title, actions]), page]),
    ]));

  startRouter(async (route) => {
    renderNav(nav);
    title.textContent = route.title;
    clear(actions);
    clear(page);
    page.append(el('div', { class: 'empty', text: '加载中…' }));
    try {
      const mod = await loadPage(route);
      clear(page);
      await mod.render({ page, actions, session, route, navigate });
    } catch (err) {
      clear(page);
      page.append(el('div', { class: 'empty', text: '页面加载失败：' + api.errorMessage(err) }));
      toast(api.errorMessage(err), 'error');
    }
  });
}

boot();