import { api, me, login, logout, version } from './api.js';
import { renderBrand } from './brand.js';
import { el, toast, clear } from './ui.js';
import { startRouter, renderNav, loadPage, navigate, currentRoute } from './router.js';
import { initCurrency, currencies, displayCurrency, setDisplayCurrency, missingRates } from './money.js';

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
  // A page may return a teardown function (see showRoute); it is declared here so the logout
  // handler above can reach it.
  let teardown = null;
  const nav = el('nav');
  const title = el('h1');
  const actions = el('div', { class: 'actions' });
  const page = el('section', { class: 'page' });
  const currencyBox = el('div', { class: 'currency-picker' });
  const whoami = el('span', { text: session.username + ' · ' + session.role });
  const logoutBtn = el('button', { class: 'btn btn-ghost', text: '退出' });
  logoutBtn.addEventListener('click', async () => {
    // Logging out while an answer is streaming must abort it: the stream carries the
    // previous session's data and would otherwise keep writing into a dead page.
    if (typeof teardown === 'function') {
      try { teardown(); } catch (err) { /* ignore */ }
      teardown = null;
    }
    try { await logout(); } catch (err) { /* the cookie is cleared client-side anyway */ }
    session = null;
    renderLogin('已退出');
  });
  app.append(
    el('div', { class: 'app' }, [
      el('aside', { class: 'sidebar' }, [renderBrand(version), nav,
        el('div', { class: 'sidebar-foot' }, [whoami, logoutBtn])]),
      el('main', { class: 'main' }, [el('header', { class: 'topbar' }, [title, currencyBox, actions]), page]),
    ]));

  // The chat page returns a teardown function: its answer stream and its timers must stop
  // when the operator navigates away, otherwise a stream from the previous page keeps
  // appending into a detached DOM node and keeps a request alive.
  const showRoute = async (route) => {
    if (typeof teardown === 'function') {
      try { teardown(); } catch (err) { /* a broken teardown must not block navigation */ }
      teardown = null;
    }
    renderNav(nav);
    title.textContent = route.title;
    clear(actions);
    clear(page);
    page.append(el('div', { class: 'empty', text: '加载中…' }));
    try {
      const mod = await loadPage(route);
      clear(page);
      const result = await mod.render({ page, actions, session, route, navigate });
      if (typeof result === 'function') teardown = result;
    } catch (err) {
      clear(page);
      page.append(el('div', { class: 'empty', text: '页面加载失败：' + api.errorMessage(err) }));
      toast(api.errorMessage(err), 'error');
    }
  };

  startRouter(showRoute);

  // The display currency is a view preference: it lives in this browser and only
  // changes how ledger amounts are rendered, so switching it simply re-renders the
  // page that is already open.
  renderCurrencyPicker(currencyBox, () => showRoute(currentRoute()));
}

// renderCurrencyPicker draws the display-currency selector. It stays quiet when
// there is nothing to choose (a single-currency deployment), and it warns about
// currencies some model uses but the rate table cannot convert.
function renderCurrencyPicker(host, rerender) {
  clear(host);
  initCurrency().then(() => {
    const options = currencies();
    const selected = displayCurrency();
    if (options.length > 1) {
      const select = el('select', { title: '账本类金额按所选币种换算显示（带 ≈）；计价类金额始终按模型自己的币种显示' },
        options.map((entry) => el('option', {
          value: entry.code, text: entry.code, selected: entry.code === selected,
        })));
      select.addEventListener('change', () => {
        setDisplayCurrency(select.value);
        rerender();
      });
      host.append(el('span', { class: 'muted', text: '显示币种' }), select);
    }
    const missing = missingRates();
    if (missing.length) {
      host.append(el('span', {
        class: 'badge danger',
        text: missing.join(' / ') + ' 缺汇率',
        title: '这些币种被某个模型的定价规则使用，但 billing.fx_rates 里没有汇率；该模型目前无法计价（成本与收费记 0）。请在「设置 → 汇率表」补齐。',
      }));
    }
  }).catch(() => {
    // The banner is chrome, not content: a failure here must not break the page.
  });
}

boot();