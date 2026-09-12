import { api } from '../api.js';
import { el, card, pagedTable, toast, badge, formatTime, jsonBlock, statusBadge, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';
import { initCurrency, money } from '../money.js';

// Dimension labels for the statistics card. The keys are the API's group_by values.
const DIMENSIONS = [
  ['client', '客户端'], ['model', '请求的模型'], ['resolved_model', '路由到的模型'],
  ['workspace', '工作区'], ['session', '会话'], ['call_kind', '调用类型'],
];

export async function render({ page, actions, session }) {
  // Cost is a ledger amount, so it renders in the operator's display currency like every
  // other money column in the console.
  await initCurrency();

  const days = el('select', {}, [1, 3, 7, 30].map((n) => el('option', { value: n, text: '最近 ' + n + ' 天' })));
  days.value = '7';
  // The identity filters are server-side: filtering in the browser would only sift the
  // current page while "共 N 条" kept describing every request.
  const client = el('select', {}, [
    el('option', { value: '', text: '全部客户端' }),
    el('option', { value: 'dsh', text: 'DSH' }),
    el('option', { value: 'codex', text: 'Codex' }),
    el('option', { value: 'unknown', text: '未识别' }),
  ]);
  const model = el('select', {}, [el('option', { value: '', text: '全部模型' })]);
  const sessionFilter = el('input', { placeholder: '会话 id（回车）', style: 'min-width:220px' });
  const workspaceFilter = el('input', { placeholder: '工作区（回车）', style: 'min-width:200px' });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  // Retention is a daily policy: this button is how an operator reclaims space without
  // waiting for the tick, and the hint next to it reports the window it will cut at.
  const prune = el('button', { class: 'btn btn-danger', text: '清理过期日志', disabled: session.role !== 'admin' });
  const hint = el('span', { class: 'muted', text: '' });
  actions.append(refresh, prune);
  let retention = null;

  async function loadRetention() {
    try {
      retention = (await api.get('/stats')).request_log || null;
    } catch (err) { retention = null; }
    renderRetention();
  }

  // The hint reports the live policy and the write health: a retention window nothing
  // enforces would be worse than no window at all, which is what recording.retention_days
  // used to be. A dropped row is called out loudly because it is a hole in the audit trail.
  function renderRetention() {
    if (!retention) { hint.textContent = ''; return; }
    const parts = [retention.retention_days > 0
      ? '保留期 ' + retention.retention_days + ' 天（每日自动清理，可手动触发）'
      : '保留期已关闭（recording.retention_days=0，不自动清理）'];
    if (retention.pruned) parts.push('本进程已清理 ' + retention.pruned + ' 行');
    if (retention.dropped) parts.push('⚠ 有 ' + retention.dropped + ' 行日志写入失败且未能留下兜底行');
    else if (retention.write_failures) parts.push('写入失败 ' + retention.write_failures + ' 次（已用无正文兜底行写入）');
    if (retention.last_error) parts.push('上次清理失败：' + retention.last_error);
    hint.textContent = parts.join('；');
  }

  async function pruneNow() {
    const window = retention && retention.retention_days > 0
      ? '保留期 ' + retention.retention_days + ' 天'
      : '保留期已关闭';
    const ok = await confirmDialog('清理过期日志',
      '将永久删除' + window + '之前的请求日志与已过期的存储响应（计费与审计记录不受影响）。继续吗？');
    if (!ok) return;
    try {
      const result = await api.post('/requests/prune', {});
      if (result.disabled) {
        toast('保留期已关闭（recording.retention_days=0），未删除任何内容', 'error');
      } else {
        toast('已删除请求日志 ' + (result.request_logs || 0) + ' 行、存储响应 ' + (result.responses || 0) + ' 行'
          + (result.exhausted ? '（本轮达到批量上限，下一轮继续）' : ''), 'ok');
      }
      await loadRetention();
      // Deleting rows shifts every offset, so the pager goes back to page 1.
      view.reset();
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  }

  function filterParams() {
    const params = { days: days.value, client: client.value, model: model.value };
    if (sessionFilter.value.trim()) params.session_id = sessionFilter.value.trim();
    if (workspaceFilter.value.trim()) params.workspace = workspaceFilter.value.trim();
    return params;
  }

  const view = pagedTable({
    columns: [
      { key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
      { key: 'request_id', label: '请求 ID', render: (row) => el('code', { text: row.request_id }) },
      { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
      { key: 'client', label: '客户端', render: (row) => (row.client ? badge(row.client, row.client === 'unknown' ? '' : 'ok') : el('span', { class: 'muted', text: '—' })) },
      { key: 'model', label: '模型', render: (row) => modelCell(row) },
      { key: 'workspace', label: '工作区', render: (row) => pathCell(row.workspace) },
      { key: 'session_id', label: '会话', render: (row) => sessionCell(row.session_id) },
      { key: 'call_kind', label: '类型', render: (row) => callKindCell(row) },
      { key: 'title', label: '标题', render: (row) => (row.title ? el('span', { text: row.title }) : el('span', { class: 'muted', text: '—' })) },
      { key: 'usage', label: 'tokens（入/出）', render: (row) => tokensCell(row.usage) },
      { key: 'charge', label: '成本', render: (row) => costCell(row.usage) },
      { key: 'output_text_recorded', label: '输出文本', render: (row) => (row.output_text_recorded ? badge('已录制', 'ok') : badge('未录制')) },
    ],
    empty: '该窗口内没有请求日志',
    rowActions: (row) => [el('button', {
      class: 'btn', text: '详情',
      // The click handler owns the failure: an unawaited promise here would surface as
      // "Uncaught (in promise)" in the console instead of a message on screen.
      onclick: () => detail(row.request_id).catch((err) => toast(api.errorMessage(err), 'error')),
    })],
    load: ({ limit, offset }) => api.get('/requests', { ...filterParams(), limit, offset }),
    // The summary row under the list: the page's tokens and money, in the same cells as
    // the columns above them (summaryCells).
    footer: summaryCells,
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });

  // ---------------------------------------------------------------------------
  // 维度统计：谁在用、用哪个模型、哪个工作区/会话、花了多少
  // ---------------------------------------------------------------------------
  const groupBy = el('select', {}, DIMENSIONS.map(([value, label]) => el('option', { value, text: label })));
  const statsHost = el('div', { class: 'muted', text: '加载中…' });
  const statsCard = card('维度统计', statsHost, [groupBy, el('span', {
    class: 'muted', text: '按身份维度汇总请求数、token 与成本；成本与账单同源（计量表）',
  })]);

  async function loadStats() {
    try {
      const payload = await api.get('/requests/dimensions', { ...filterParams(), group_by: groupBy.value, limit: 20 });
      renderStats(payload);
    } catch (err) {
      statsHost.replaceChildren(el('div', { class: 'muted', text: api.errorMessage(err) }));
    }
  }

  function renderStats(payload) {
    const rows = payload.rows || [];
    const bySession = payload.group_by === 'session';
    if (!rows.length) {
      statsHost.replaceChildren(el('div', { class: 'muted', text: '该窗口内没有可统计的请求' }));
      return;
    }
    const head = ['分组', '请求数', '已计量', '输入 tokens', '输出 tokens', '成本'];
    if (bySession) head.splice(1, 0, '标题', '工作区');
    const header = el('thead', {}, [el('tr', {}, head.map((label) => el('th', { text: label })))]);
    const body = el('tbody', {}, rows.map((row) => {
      const cells = [el('td', {}, [keyCell(row.key)])];
      if (bySession) {
        cells.push(el('td', { text: row.title || '—' }), el('td', {}, [pathCell(row.workspace)]));
      }
      cells.push(
        el('td', { text: String(row.requests) }),
        // "已计量" is the count with a usage row: the difference from 请求数 is requests a
        // local rejection or a lost row left unmetered, which is worth seeing.
        el('td', { text: row.metered === row.requests ? String(row.metered) : row.metered + ' / ' + row.requests }),
        el('td', { text: formatTokens(row.input_tokens) }),
        el('td', { text: formatTokens(row.output_tokens) }),
        el('td', { text: row.metered ? money(row.charge_micros) : '未计量' }),
      );
      return el('tr', {}, cells);
    }));
    statsHost.replaceChildren(el('table', {}, [header, body]));
  }

  function keyCell(key) {
    if (!key) return el('span', { class: 'muted', text: '（未知）' });
    const text = String(key);
    return el('span', { title: text, text: text.length > 44 ? text.slice(0, 42) + '…' : text });
  }

  groupBy.addEventListener('change', () => { loadStats(); });

  // The model dropdown is filled from the same statistics endpoint, so it offers the
  // models this window actually used rather than a configuration list that may be empty.
  async function loadModelOptions() {
    try {
      const payload = await api.get('/requests/dimensions', { days: days.value, group_by: 'model', limit: 50 });
      const current = model.value;
      const options = [el('option', { value: '', text: '全部模型' })];
      for (const row of payload.rows || []) {
        if (!row.key) continue;
        options.push(el('option', { value: row.key, text: row.key + '（' + row.requests + '）' }));
      }
      model.replaceChildren(...options);
      model.value = current;
    } catch (err) { /* a filter list that cannot load must not block the page */ }
  }

  page.append(card('请求日志', view.node, [
    days, client, model, sessionFilter, workspaceFilter,
    el('span', { class: 'muted', text: '客户端/模型/工作区/会话/标题与 token 成本是独立于正文口径记录的元数据（record_input=off 也记）；标题来自会话的标题调用，成本来自计量表，与账单一致；列表底部的「本页汇总」只合计当前页已加载的行（含本页过滤），窗口口径看下方「维度统计」' }),
    hint]));
  page.append(statsCard);

  // Changing any filter restarts at page 1: the rows of the current page belong to a
  // different filter, so their offset is meaningless.
  days.addEventListener('change', () => { view.reset(); loadStats(); loadModelOptions(); });
  client.addEventListener('change', () => { view.reset(); loadStats(); });
  model.addEventListener('change', () => { view.reset(); loadStats(); });
  for (const input of [sessionFilter, workspaceFilter]) {
    input.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter') return;
      view.reset();
      loadStats();
    });
  }
  refresh.addEventListener('click', () => { view.refresh(); loadStats(); loadModelOptions(); });
  prune.addEventListener('click', () => pruneNow());
  await loadRetention();
  await Promise.all([view.refresh(), loadStats(), loadModelOptions()]);
}

// modelCell shows the model that actually served the request, with the requested name
// when they differ: an alias such as `luna -> gpt-5.6-luna` is exactly the case where the
// two columns say different things, and hiding either would be lying about one of them.
function modelCell(row) {
  const resolved = row.resolved_model || '';
  const requested = row.model || '';
  if (!resolved && !requested) return el('span', { class: 'muted', text: '—' });
  const primary = resolved || requested;
  const node = el('span', { text: primary });
  if (resolved && requested && resolved !== requested) {
    node.append(el('span', { class: 'muted', text: ' ← ' + requested }));
  }
  return node;
}

function callKindCell(row) {
  if (row.call_kind === 'title') return badge('标题调用', 'ok');
  if (row.call_kind === 'agent') return badge('会话轮次');
  return el('span', { class: 'muted', text: '—' });
}

function pathCell(value) {
  if (!value) return el('span', { class: 'muted', text: '—' });
  return el('code', { title: value, text: value });
}

function sessionCell(value) {
  if (!value) return el('span', { class: 'muted', text: '—' });
  return el('code', { title: value, text: value.length > 18 ? value.slice(0, 16) + '…' : value });
}

// A request with no usage row is "未计量", not zero: a locally rejected request never
// reached an upstream, and reporting 0 tokens would read as "it consumed nothing".
function tokensCell(usage) {
  if (!usage || !usage.metered) return el('span', { class: 'muted', text: '未计量' });
  return el('span', { text: formatTokens(usage.input_tokens) + ' / ' + formatTokens(usage.output_tokens) });
}

function costCell(usage) {
  if (!usage || !usage.metered) return el('span', { class: 'muted', text: '未计量' });
  return el('span', { text: money(usage.charge_micros) });
}

// summaryCells is the list's summary row: the tokens and the money of the rows on screen,
// keyed by the columns they belong under. The scope is the page — these rows, or what the
// in-page filter left of them — not the whole filtered window: the pager's 共 N 条 and the
// 维度统计 card describe the window, and a page-sized total dressed up as that would be a
// different claim. Rows with no usage row are counted in the label and contribute no
// number, which is the rule the cells above already follow ("未计量" ≠ "消耗为 0").
function summaryCells(rows) {
  if (!rows.length) return null;
  let metered = 0;
  let input = 0;
  let output = 0;
  let charge = 0;
  for (const row of rows) {
    const usage = row.usage;
    if (!usage || !usage.metered) continue;
    metered += 1;
    input += Number(usage.input_tokens || 0);
    output += Number(usage.output_tokens || 0);
    // The sum goes to money(), which converts through BigInt: a fraction would throw.
    charge += Math.round(Number(usage.charge_micros || 0));
  }
  const unmetered = rows.length - metered;
  // On a fully metered page "共 N 行" and "已计量 N" would say the same thing twice, so
  // the breakdown only appears when there is something to explain (the 维度统计 card
  // reports its own counts the same way).
  const counted = unmetered
    ? '共 ' + rows.length + ' 行（已计量 ' + metered + ' · 未计量 ' + unmetered + '）'
    : '共 ' + rows.length + ' 行';
  return {
    created_at: el('div', {
      title: '只合计当前页已加载的行（本页过滤生效时就是屏幕上剩下的行），不是整个筛选窗口',
    }, [el('strong', { text: '本页汇总' }), el('span', { class: 'muted', text: ' · ' + counted })]),
    usage: metered
      ? el('span', { text: formatTokens(input) + ' / ' + formatTokens(output) })
      : el('span', { class: 'muted', text: '未计量' }),
    charge: metered ? el('span', { text: money(charge) }) : el('span', { class: 'muted', text: '未计量' }),
  };
}

function formatTokens(value) {
  const n = Number(value || 0);
  return n.toLocaleString('en-US');
}

async function detail(requestID) {
  const row = await api.get('/requests/' + encodeURIComponent(requestID));
  // api.get answers with whatever the body parsed to: an empty or unparseable body is
  // null, not an object. Dereferencing that used to throw a TypeError inside the click
  // handler, which left the operator with nothing but "Cannot read properties of null"
  // in the console — so an answer that is not a log row is reported, not assumed away.
  if (!row || typeof row !== 'object') {
    throw new Error('该请求的详情为空（服务端未返回内容），日志可能刚好被保留期清理，请刷新列表');
  }
  const usage = row.usage || { metered: false };
  const body = el('div', { class: 'split' }, [
    panel('输入' + (row.input_recorded ? '' : '（未录制）'), row.input),
    panel('思考文本' + (row.reasoning_recorded ? '' : '（未录制）'), row.reasoning),
    panel('最终输出' + (row.output_text_recorded ? '' : '（未录制）'), row.output),
  ]);
  const dialog = el('div', { class: 'modal', style: 'width:min(1000px,100%)' }, [
    modalHead('请求 ' + requestID, close),
    // Everything that can grow goes into the scrolling body, so the header (and the
    // round ✕ with it) stays pinned to the dialog frame while a long log scrolls.
    modalBody([
      el('div', { class: 'muted', text: row.endpoint + ' · ' + formatTime(row.created_at) + ' · HTTP ' + row.status }),
      identityBlock(row),
      usageBlock(usage),
      // The size of the request is recorded even when its content is not, so a
      // "未录制" panel can still say how big the request was.
      el('div', { class: 'muted', text: row.request_bytes ? '请求正文 ' + row.request_bytes + ' 字节' + (row.truncated ? '（已截断）' : '') : '' }),
      body,
    ]),
    modalActions([el('button', { class: 'btn', text: '关闭', onclick: () => close() })]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  // Closing goes through one function: the round ✕ in the header and the footer
  // button are the same action, and the click-outside rule reuses it too.
  function close() { backdrop.remove(); }
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(); });
  document.getElementById('modal-root').append(backdrop);
}

// identityBlock shows what the gateway could tell about the caller without reading the
// body: the row keeps this even when record_input is off.
function identityBlock(row) {
  const fields = [
    ['客户端', row.client || '未识别'],
    ['请求的模型', row.model || '—'],
    ['路由到的模型', row.resolved_model || '—'],
    ['工作区', row.workspace || '—'],
    ['会话', row.session_id || '—'],
    ['调用类型', row.call_kind || '—'],
    ['标题', row.title || '—'],
  ];
  return el('div', { class: 'muted' }, fields.map(([label, value]) =>
    el('div', {}, [el('strong', { text: label + '：' }), el('span', { text: String(value) })])));
}

function usageBlock(usage) {
  if (!usage || !usage.metered) {
    return el('div', { class: 'muted', text: '未计量：该请求没有计量行（本地拒绝的请求按设计不写 usage），与「消耗为 0」不同' });
  }
  const fields = [
    ['输入 tokens', formatTokens(usage.input_tokens)],
    ['输出 tokens', formatTokens(usage.output_tokens)],
    ['思考 tokens', formatTokens(usage.reasoning_tokens)],
    ['成本', money(usage.cost_micros)],
    ['对客', money(usage.charge_micros)],
    ['延迟', usage.latency_ms + ' ms'],
    ['首字延迟', usage.ttft_ms + ' ms'],
    ['上游尝试', String(usage.attempts)],
  ];
  return el('div', { class: 'muted' }, fields.map(([label, value]) =>
    el('div', {}, [el('strong', { text: label + '：' }), el('span', { text: String(value) })])));
}

function panel(title, value) {
  return el('div', {}, [el('h4', { text: title }), jsonBlock(value)]);
}
