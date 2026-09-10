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

export function toast(message, kind) {
  const host = document.getElementById('toasts');
  const node = el('div', { class: 'toast ' + (kind || ''), text: message });
  host.append(node);
  setTimeout(() => node.remove(), kind === 'error' ? 8000 : 4000);
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
      el('h3', { text: title }), body, error,
      el('div', { class: 'modal-actions' }, [cancel, submit]),
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
    const dialog = el('div', { class: 'modal' }, [el('h3', { text: title }), el('p', { text: message }),
      el('div', { class: 'modal-actions' }, [cancel, ok])]);
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
export function table({ columns, rows, filter, onFilter, empty, rowActions }) {
  const wrap = el('div');
  let query = '';
  const head = el('thead', {}, [el('tr', {}, columns.map((col) => el('th', {
    text: col.label, onclick: col.sortable ? () => toggleSort(col.key) : undefined,
  })).concat(rowActions ? [el('th', { text: '' })] : []))]);
  const tbody = el('tbody');
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

  function text(value) {
    if (value === null || value === undefined || value === '') return el('span', { class: 'muted', text: '—' });
    return document.createTextNode(String(value));
  }

  if (filter !== false) {
    const box = el('input', { type: 'search', placeholder: '过滤…' });
    box.addEventListener('input', () => { query = box.value; render(); });
    wrap.append(el('div', { class: 'toolbar' }, [box, el('span', { class: 'muted', id: 'count' })]));
  }
  wrap.append(el('table', {}, [head, tbody]));
  const api = { refresh: (next) => { rows = next || rows; render(); }, node: wrap };
  render();
  return api;
}