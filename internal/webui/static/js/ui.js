// Small DOM helpers. This module knows nothing about the API or any page, so it can
// be reused by every screen without creating a dependency cycle.

export function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [key, value] of Object.entries(attrs)) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = value;
      else if (key === 'html') node.innerHTML = value;
      else if (key === 'dataset') Object.assign(node.dataset, value);
      else if (key.startsWith('on') && typeof value === 'function') node.addEventListener(key.slice(2), value);
      else if (value === true) node.setAttribute(key, '');
      else node.setAttribute(key, value);
    }
  }
  for (const child of [].concat(children || [])) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

export function card(title, children, actions) {
  const head = title ? el('div', { class: 'toolbar' }, [el('h2', { text: title, style: 'margin:0;flex:1' }), ...(actions || [])]) : null;
  return el('section', { class: 'card' }, [head, ...[].concat(children || [])]);
}

export function stat(label, value) {
  return el('div', { class: 'stat' }, [el('div', { class: 'k', text: label }), el('div', { class: 'v', text: value })]);
}

export function badge(text, kind) { return el('span', { class: 'badge ' + (kind || ''), text }); }

export function statusBadge(status) {
  const kind = status === 'active' || status === 'ok' || status === 'enabled' ? 'ok'
    : status === 'suspended' || status === 'revoked' || status === 'failed' ? 'danger' : '';
  return badge(status || 'unknown', kind);
}

export function toast(message, kind, options) {
  const opts = options || {};
  const host = document.getElementById('toasts');
  const node = el('div', { class: 'toast ' + (kind || ''), text: message });
  host.append(node);
  // sticky toasts are removed by the caller: they narrate an operation that can
  // outlive the normal auto-dismiss window.
  if (!opts.sticky) setTimeout(() => node.remove(), kind === 'error' ? 8000 : 4000);
  return node;
}

export function spinner() { return el('span', { class: 'spinner', 'aria-hidden': 'true' }); }

// withBusy runs an async action while showing progress on the button that started
// it: the button is disabled, its label becomes spinner + text, and the elapsed
// seconds tick up so a genuinely slow call (a provider probe is a real upstream
// request) never looks like a hung page. The button is always restored, and the
// action's result is passed through. Without a button, the action simply runs.
export async function withBusy(button, label, action) {
  if (!button) return action();
  const original = button.textContent;
  const wasDisabled = button.disabled;
  const elapsed = el('span', { text: '' });
  const started = Date.now();
  const tick = () => { elapsed.textContent = ' ' + Math.round((Date.now() - started) / 1000) + 's'; };
  clear(button);
  button.append(spinner(), document.createTextNode(label + '…'), elapsed);
  button.disabled = true;
  tick();
  const timer = setInterval(tick, 1000);
  try {
    return await action();
  } finally {
    clearInterval(timer);
    button.textContent = original;
    button.disabled = wasDisabled;
  }
}

export function formatTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString();
}

export function jsonBlock(value) {
  if (value === null || value === undefined || value === '') return el('span', { class: 'muted', text: '—' });
  const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  return el('pre', { class: 'mono', text });
}

// closeIcon draws the cross inside the close button. It is inline SVG rather than
// the "✕" text glyph this used to be: U+2715 is missing from plenty of operator
// font stacks, and where it is missing the button renders as an empty box — a
// dialog that looks like it has no close control at all (that is exactly what was
// reported from a browser whose fonts lacked it). A drawn cross cannot depend on
// the fonts installed on the machine reading the console. Nothing here needs the
// network or an inline style, so the strict console CSP is untouched: img-src
// governs loaded resources, not inline elements.
function closeIcon() {
  const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  for (const [key, value] of Object.entries({
    class: 'modal-close-icon', viewBox: '0 0 14 14', width: '12', height: '12',
    fill: 'none', stroke: 'currentColor', 'stroke-width': '1.6',
    'stroke-linecap': 'round', 'aria-hidden': 'true', focusable: 'false',
  })) icon.setAttribute(key, value);
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', 'M3 3 L11 11 M11 3 L3 11');
  icon.append(path);
  return icon;
}

// closeButton is the round ✕ every dialog carries in its top-right corner, so a
// popup is always dismissable the same way. Pages that hand-roll a dialog (a
// secret reveal, a detail view) call this instead of inventing their own, and
// modal()/confirmDialog() below use it too. The label is text (every reader has
// the CJK font this console is written in); the icon is drawn (see closeIcon).
export function closeButton(onClose, options) {
  const opts = options || {};
  return el('button', {
    class: 'modal-close' + (opts.danger ? ' modal-close-danger' : ''),
    type: 'button', title: '关闭', 'aria-label': '关闭', onclick: onClose,
  }, [closeIcon()]);
}

// modalHead is the dialog header: the title on the left, the round close button
// on the right. Every dialog builds its header this way so the close button can
// never be missing from one of them. It is a direct child of .modal, outside
// .modal-body, so the ✕ stays pinned to the dialog frame while the body scrolls.
export function modalHead(title, onClose, options) {
  return el('div', { class: 'modal-head' }, [el('h3', { text: title }), closeButton(onClose, options)]);
}

// modalBody is the scrolling region of a dialog: everything that can grow (a field
// list, a long log, a detail table) goes in here, so a tall dialog scrolls its
// content instead of pushing the header — and with it the round ✕ — off screen.
export function modalBody(children) {
  return el('div', { class: 'modal-body' }, children);
}

// modalActions is the footer action row, also outside the scrolling region so the
// primary buttons stay reachable in a tall dialog.
export function modalActions(children) {
  return el('div', { class: 'modal-actions' }, children);
}

// modal renders a form and resolves with the collected values, or null on cancel.
export function modal({ title, fields, submitLabel, onSubmit, wide }) {
  return new Promise((resolve) => {
    const root = document.getElementById('modal-root');
    const body = el('div', {}, fields.map(renderField));
    const error = el('div', { class: 'muted' });
    const close = (value) => { backdrop.remove(); resolve(value); };
    const submit = el('button', { class: 'btn btn-primary', text: submitLabel || '保存' });
    submit.addEventListener('click', async () => {
      const values = collect(fields, body);
      if (!values) return;
      submit.disabled = true;
      try {
        const result = onSubmit ? await onSubmit(values) : values;
        close(result === undefined ? values : result);
      } catch (err) {
        error.textContent = err && err.message ? err.message : String(err);
        error.className = 'toast error';
        submit.disabled = false;
      }
    });
    const cancel = el('button', { class: 'btn', text: '取消', onclick: () => close(null) });
    const dialog = el('div', { class: 'modal', style: wide ? 'width:min(900px,100%)' : '' }, [
      modalHead(title, () => close(null)),
      modalBody([body, error]),
      modalActions([cancel, submit]),
    ]);
    const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
    backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(null); });
    root.append(backdrop);
    const first = dialog.querySelector('input, textarea, select');
    if (first) first.focus();
  });
}

export function confirmDialog(title, message) {
  return new Promise((resolve) => {
    const root = document.getElementById('modal-root');
    const ok = el('button', { class: 'btn btn-danger', text: '确认', onclick: () => { backdrop.remove(); resolve(true); } });
    const cancel = el('button', { class: 'btn', text: '取消', onclick: () => { backdrop.remove(); resolve(false); } });
    const dialog = el('div', { class: 'modal' }, [modalHead(title, () => { backdrop.remove(); resolve(false); }, { danger: true }),
      modalBody([el('p', { text: message })]),
      modalActions([cancel, ok])]);
    const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
    root.append(backdrop);
  });
}

function renderField(field) {
  const id = 'f_' + field.name;
  let input;
  if (field.type === 'textarea') {
    input = el('textarea', { id, name: field.name, placeholder: field.placeholder || '', rows: field.rows || 6 });
    if (field.value) input.value = typeof field.value === 'string' ? field.value : JSON.stringify(field.value, null, 2);
  } else if (field.type === 'select') {
    input = el('select', { id, name: field.name }, (field.options || []).map((opt) => {
      const value = typeof opt === 'string' ? opt : opt.value;
      const label = typeof opt === 'string' ? opt : opt.label;
      return el('option', { value, text: label, selected: String(field.value) === String(value) });
    }));
  } else if (field.type === 'checkbox') {
    input = el('input', { id, name: field.name, type: 'checkbox' });
    input.checked = !!field.value;
  } else {
    input = el('input', { id, name: field.name, type: field.type || 'text', value: field.value === undefined || field.value === null ? '' : field.value, placeholder: field.placeholder || '' });
  }
  if (field.required) input.required = true;
  // A read-only field still submits its value (collect() reads node.value); it is how an
  // edit form shows the identity of the row without letting the operator rename it.
  if (field.readonly) input.readOnly = true;
  return el('label', { class: 'field' }, [el('span', { text: field.label }), input, field.hint ? el('span', { class: 'muted', text: field.hint }) : null]);
}

function collect(fields, body) {
  const values = {};
  for (const field of fields) {
    const node = body.querySelector('[name="' + field.name + '"]');
    if (!node) continue;
    let value;
    if (field.type === 'checkbox') value = node.checked;
    else if (field.json) {
      const raw = node.value.trim();
      if (raw === '') { value = undefined; }
      else {
        try { value = JSON.parse(raw); } catch (err) { toast(field.label + ' 不是合法 JSON', 'error'); return null; }
      }
    } else if (field.type === 'number') {
      value = node.value === '' ? undefined : Number(node.value);
    } else {
      value = node.value;
      if (field.required && value === '') { toast(field.label + ' 必填', 'error'); return null; }
    }
    if (value !== undefined) values[field.name] = value;
  }
  return values;
}

// table renders rows with a filter box, optional sortable headers and row actions.
//
// filterPlaceholder is for server-paged tables: there the filter can only see the
// current page, so pagedTable labels it accordingly instead of pretending to search
// the whole table (see docs/design/m24-console-pagination.md).
//
// footer is for lists that carry a summary row under their own columns (the request
// log's token/cost totals). It receives the rows currently shown — after the in-page
// filter — and returns the cells it wants to fill, keyed by column. Every column it does
// not name gets an empty cell, so the row cannot drift out of alignment when a column is
// added; colspan arithmetic at the call site is exactly what goes stale. Returning
// nothing (or having no rows to show) renders no summary row at all.
export function table({ columns, rows, filter, onFilter, empty, rowActions, filterPlaceholder, footer }) {
  const wrap = el('div');
  let query = '';
  const head = el('thead', {}, [el('tr', {}, columns.map((col) => el('th', {
    text: col.label, onclick: col.sortable ? () => toggleSort(col.key) : undefined,
  })).concat(rowActions ? [el('th', { text: '' })] : []))]);
  const tbody = el('tbody');
  // The element only exists when the list has a summary: a table without one keeps the
  // exact DOM it had before.
  const tfoot = footer ? el('tfoot') : null;
  const countLabel = el('span', { class: 'muted', id: 'count' });
  let sortKey = null;
  let sortDir = 1;

  function visible() {
    let data = rows.slice();
    if (query) {
      const needle = query.toLowerCase();
      data = data.filter((row) => JSON.stringify(row).toLowerCase().includes(needle));
    }
    if (sortKey) {
      data.sort((a, b) => {
        const av = a[sortKey], bv = b[sortKey];
        if (av === bv) return 0;
        if (av === undefined || av === null) return 1;
        if (bv === undefined || bv === null) return -1;
        return (typeof av === 'number' && typeof bv === 'number' ? av - bv : String(av).localeCompare(String(bv))) * sortDir;
      });
    }
    return data;
  }

  function toggleSort(key) {
    if (sortKey === key) sortDir = -sortDir; else { sortKey = key; sortDir = 1; }
    render();
  }

  function render() {
    clear(tbody);
    const data = visible();
    countLabel.textContent = query ? '本页 ' + data.length + ' / ' + rows.length + ' 行' : '本页 ' + rows.length + ' 行';
    renderFooter(data);
    if (!data.length) {
      tbody.append(el('tr', {}, [el('td', { colspan: columns.length + (rowActions ? 1 : 0) }, [el('div', { class: 'empty', text: empty || '暂无数据' })])]));
      return;
    }
    for (const row of data) {
      const cells = columns.map((col) => el('td', {}, [col.render ? col.render(row) : text(row[col.key])]));
      if (rowActions) cells.push(el('td', { class: 'actions' }, rowActions(row)));
      tbody.append(el('tr', {}, cells));
    }
  }

  // renderFooter places each summary cell under its own column, and an empty cell
  // everywhere else, so the summary always lines up with the header it belongs to.
  function renderFooter(data) {
    if (!tfoot) return;
    clear(tfoot);
    const cells = data.length ? footer(data) : null;
    if (!cells) return;
    tfoot.append(el('tr', {}, columns.map((col) => el('td', {}, [cells[col.key]]))
      .concat(rowActions ? [el('td')] : [])));
  }

  function text(value) {
    if (value === null || value === undefined || value === '') return el('span', { class: 'muted', text: '—' });
    return document.createTextNode(String(value));
  }

  if (filter !== false) {
    const box = el('input', { type: 'search', placeholder: filterPlaceholder || '过滤…' });
    box.addEventListener('input', () => { query = box.value; render(); });
    wrap.append(el('div', { class: 'toolbar' }, [box, countLabel]));
  }
  wrap.append(el('table', {}, [head, tbody, tfoot]));
  const api = { refresh: (next) => { rows = next || rows; render(); }, node: wrap };
  render();
  return api;
}

// ---------------------------------------------------------------------------
// server-side pagination
// ---------------------------------------------------------------------------

const PAGE_SIZES = [20, 50, 100];

// pager renders the navigation below a list: the range it shows, the total the server
// counted, the page size and prev/next/jump. It is presentation only — the caller owns the
// window (pagedTable is one such caller, the request log's statistics card is another) —
// and it lives outside the <table> element on purpose, so nothing that reads tbody rows
// (styles, other pages, the UI harness) has to know about it.
//
// unit names what is being counted, because a page does not always hold records: the
// request log's breakdown pages groups, and "共 12 条" would describe requests it never
// counted. It defaults to 条, so every existing caller keeps its wording.
export function pager({ limit, offset, total, pageSizes, unit, onChange }) {
  const sizes = pageSizes || PAGE_SIZES;
  const pages = Math.max(1, Math.ceil(total / limit));
  const current = Math.min(pages, Math.floor(offset / limit) + 1);
  const first = total === 0 ? 0 : offset + 1;
  const last = Math.min(offset + limit, total);

  const info = el('span', { class: 'muted', text: total === 0
    ? '共 0 ' + (unit || '条')
    : '共 ' + total + ' ' + (unit || '条') + ' · 本页 ' + first + '–' + last + ' · 第 ' + current + '/' + pages + ' 页' });

  const sizeSelect = el('select', { title: '每页条数' },
    sizes.map((n) => el('option', { value: n, text: n + ' 条/页', selected: n === limit })));
  // A new page size always restarts at the first page: keeping the offset would show a
  // window that no longer lines up with the pages the user was browsing.
  sizeSelect.addEventListener('change', () => onChange({ limit: Number(sizeSelect.value), offset: 0 }));

  const prev = el('button', { class: 'btn', text: '上一页', disabled: offset === 0 });
  prev.addEventListener('click', () => onChange({ limit, offset: Math.max(0, offset - limit) }));
  const next = el('button', { class: 'btn', text: '下一页', disabled: offset + limit >= total });
  next.addEventListener('click', () => onChange({ limit, offset: offset + limit }));

  const jump = el('input', { type: 'number', min: 1, max: pages, value: String(current), 'aria-label': '跳至第几页' });
  const go = el('button', { class: 'btn', text: '跳转' });
  const jumpTo = () => {
    const target = Math.min(pages, Math.max(1, Number(jump.value) || 1));
    if (target === current) return;
    onChange({ limit, offset: (target - 1) * limit });
  };
  go.addEventListener('click', jumpTo);
  jump.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') jumpTo(); });

  return el('div', { class: 'pager' }, [info,
    el('div', { class: 'pager-actions' }, [sizeSelect, prev, next, jump, go])]);
}

// pagedTable couples a server-paged list to table(). It owns {limit, offset, total}:
// load({limit, offset}) asks the page for one window and must answer with an object
// carrying data + total. The returned handle is what a page keeps:
//
//   const view = pagedTable({ columns, load: ({limit, offset}) => api.get('/keys', {limit, offset}) });
//   await view.refresh();       // (re)load the current window
//   await view.reset();         // filters changed: back to the first page
//
// Failure never locks the pager: onError reports it and the previous window stays put.
// footer is forwarded to table(): a paged list whose page carries a summary row (the
// request log's token/cost totals) hands one in, and the summary is recomputed from the
// rows of whichever page is on screen.
export function pagedTable({ columns, load, pageSize, pageSizes, rowActions, empty, filter = true, onError, footer }) {
  const state = { limit: pageSize || 20, offset: 0, total: 0, loaded: false };
  const host = el('div');
  const loading = el('div', { class: 'empty', text: '加载中…' });
  host.append(loading);
  let view = null;

  function renderPager() {
    if (!state.loaded) return;
    const node = pager({
      limit: state.limit, offset: state.offset, total: state.total, pageSizes,
      onChange: (next) => { state.limit = next.limit; state.offset = next.offset; loadWindow().catch(report); },
    });
    const existing = host.querySelector('.pager');
    if (existing) existing.replaceWith(node); else host.append(node);
  }

  function report(err) {
    if (onError) onError(err);
    else throw err;
  }

  async function loadWindow() {
    for (;;) {
      const payload = await load({ limit: state.limit, offset: state.offset });
      const rows = payload.data || [];
      // total is what the server counted after filtering; an endpoint that predates
      // pagination would omit it, and then the page can only speak for itself.
      state.total = payload.total === undefined || payload.total === null ? state.offset + rows.length : Number(payload.total);
      if (!rows.length && state.offset > 0) {
        // Deleting the last row of the last page must not leave an empty page on screen.
        state.offset = Math.max(0, state.offset - state.limit);
        continue;
      }
      if (!view) {
        view = table({ columns, rows, filter, empty, rowActions, footer, filterPlaceholder: filter === false ? undefined : '本页过滤…' });
        clear(host);
        host.append(view.node);
      } else {
        view.refresh(rows);
      }
      state.loaded = true;
      renderPager();
      return;
    }
  }

  return {
    node: host,
    refresh: () => loadWindow().catch(report),
    reset: () => { state.offset = 0; return loadWindow().catch(report); },
    state: () => ({ ...state }),
  };
}