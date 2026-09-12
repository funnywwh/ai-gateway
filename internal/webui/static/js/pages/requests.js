import { api } from '../api.js';
import { el, card, pagedTable, pager, toast, badge, formatTime, jsonBlock, statusBadge, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';
import { initCurrency, money } from '../money.js';

// Dimension labels for the statistics card. The keys are the API's group_by values.
const DIMENSIONS = [
  ['account', '用户（账户）'], ['api_key', 'API Key'],
  ['client', '客户端'], ['model', '请求的模型'], ['resolved_model', '路由到的模型'],
  ['workspace', '工作区'], ['session', '会话'], ['call_kind', '调用类型'],
];

// Sort keys of the statistics table. The keys are the API's sort values; each one has a
// column of its own (see the columns built in renderStats), so the table always shows the
// value it is ordered by. The default is the first entry — 最近活跃的排最前.
const STATS_SORTS = [
  ['last_seen', '按最近一次请求'],
  ['requests', '按请求数'],
  ['charge', '按成本'],
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
  // 用户（账户）与 API Key 是凭据维度：行的身份、筛选与统计都按它们成立。选项来自配置类
  // 列表（/accounts、/keys，控制台读全再切片），筛选本身仍在服务端做——否则「共 N 条」
  // 描述的会是别的行集。Key 下拉跟随账户：选中账户后只列该账户的 Key。
  const accountFilter = el('select', {}, [el('option', { value: '', text: '全部用户' })]);
  const keyFilter = el('select', {}, [el('option', { value: '', text: '全部 API Key' })]);
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
      // Deleting rows shifts every offset and can empty a whole bucket, so both tables go
      // back to page 1.
      view.reset();
      loadStats({ reset: true });
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  }

  function filterParams() {
    const params = { days: days.value, client: client.value, model: model.value };
    if (accountFilter.value) params.account_id = accountFilter.value;
    if (keyFilter.value) params.api_key_id = keyFilter.value;
    if (sessionFilter.value.trim()) params.session_id = sessionFilter.value.trim();
    if (workspaceFilter.value.trim()) params.workspace = workspaceFilter.value.trim();
    return params;
  }

  const view = pagedTable({
    columns: [
      { key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
      { key: 'request_id', label: '请求 ID', render: (row) => el('code', { text: row.request_id }) },
      { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
      { key: 'account_id', label: '用户', render: (row) => ownerCell(row) },
      { key: 'api_key_id', label: 'API Key', render: (row) => apiKeyCell(row) },
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
  // 维度统计：谁在用（用户/API Key）、用哪个模型、哪个工作区/会话、花了多少
  // ---------------------------------------------------------------------------
  const groupBy = el('select', {}, DIMENSIONS.map(([value, label]) => el('option', { value, text: label })));
  // 排序键决定第 1 页装的是哪 20 个分组：默认「最近一次请求」（最近活跃的排最前），
  // 也能回到 M27 的老问题「谁最忙」「谁最花钱」。排序是服务端做的——只排当前页会把
  // 「第 1 页里最大的」当成全局最大。
  const sortBy = el('select', { title: '排序方式（均为降序，同数按分组名升序）；切换后回到第 1 页' },
    STATS_SORTS.map(([value, label]) => el('option', { value, text: label })));
  const statsHost = el('div', { class: 'muted', text: '加载中…' });
  const statsCard = card('维度统计', statsHost, [groupBy, sortBy, el('span', {
    class: 'muted', text: '按维度汇总请求数、token 与成本；默认按最近一次请求时间降序，工具栏可切请求数/成本；分页器给的是本窗口的分组总数（不是请求总数）；用户与 API Key 是凭据维度（名字由账户/Key 表读时解析，分组按 id），成本与账单同源（计量表）；筛选条件在下方「请求日志」卡片里改',
  })]);
  // 这张表是服务端分页的（同 M24 的列表契约），但窗口由本页持有而不是 pagedTable：
  // 表体是手写的（列随分组维度变化），pager() 只负责呈现。
  const statsWindow = { limit: 20, offset: 0, total: 0 };

  async function loadStats({ reset = false } = {}) {
    if (reset) statsWindow.offset = 0;
    try {
      for (;;) {
        const payload = await api.get('/requests/dimensions', {
          ...filterParams(), group_by: groupBy.value, sort: sortBy.value,
          limit: statsWindow.limit, offset: statsWindow.offset,
        });
        const rows = payload.rows || [];
        // total is what the server counted for these filters; an endpoint that does not
        // report it can only speak for the page in hand (the rule pagedTable follows).
        statsWindow.total = payload.total === undefined || payload.total === null
          ? statsWindow.offset + rows.length
          : Number(payload.total);
        // 清理过期日志或更窄的筛选都会让分组变少，末页可能是空的：回退一页重取，而不是
        // 停在一个空页上（与 pagedTable 同一条规则）。
        if (!rows.length && statsWindow.offset > 0 && statsWindow.total > 0) {
          statsWindow.offset = Math.max(0, statsWindow.offset - statsWindow.limit);
          continue;
        }
        renderStats(payload, rows);
        return;
      }
    } catch (err) {
      // 失败时连分页器一起换掉：留一个指向没加载出来的窗口的控件，比没有控件更糟。
      statsHost.replaceChildren(el('div', { class: 'muted', text: api.errorMessage(err) }));
    }
  }

  function renderStats(payload, rows) {
    const bySession = payload.group_by === 'session';
    if (!rows.length) {
      statsHost.replaceChildren(el('div', { class: 'muted', text: '该窗口内没有可统计的请求' }));
      return;
    }
    // 当前排序列带 ↓：表格要说清「凭什么这个分组排第一」，而不是只让工具栏的下拉去暗示。
    const columns = [
      { label: '分组' },
      {
        label: '最近一次', key: 'last_seen',
        title: '本窗口内该分组最近一次请求的时间（窗口外的请求不参与）；它是默认排序键',
      },
      {
        label: '请求数', key: 'requests',
        title: '本窗口内该分组的请求数（排序键之一）；与「已计量」不同时，差额是本地拒绝或写入失败留下的行',
      },
      { label: '已计量' },
      { label: '输入 tokens' },
      { label: '输出 tokens' },
      { label: '成本', key: 'charge', title: '对客成本（charge_micros，与列表「成本」列同源）；它是排序键之一' },
    ];
    if (bySession) columns.splice(1, 0, { label: '标题' }, { label: '工作区' });
    const header = el('thead', {}, [el('tr', {}, columns.map((col) => el('th', {
      text: col.key && col.key === sortBy.value ? col.label + ' ↓' : col.label,
      title: col.title,
    })))]);
    const body = el('tbody', {}, rows.map((row) => {
      const cells = [el('td', {}, [groupKeyCell(row, payload.group_by)])];
      if (bySession) {
        cells.push(el('td', { text: row.title || '—' }), el('td', {}, [pathCell(row.workspace)]));
      }
      cells.push(
        el('td', { text: formatTime(row.last_seen) }),
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
    // 分页器说「个分组」而不是「条」：这一页装的是分组，不是请求——页面上同时还有列表的
    // 分页器，两者各自的单位就是它们口径的第一句话。
    statsHost.replaceChildren(el('table', {}, [header, body]), pager({
      limit: payload.limit || statsWindow.limit,
      offset: payload.offset || 0,
      total: statsWindow.total,
      unit: '个分组',
      onChange: (next) => {
        statsWindow.limit = next.limit;
        statsWindow.offset = next.offset;
        loadStats();
      },
    }));
  }

  // groupKeyCell renders one bucket's key. The two credential groupings bucket on ids
  // (names are mutable labels owned by another table, and api_keys.name is not unique), so
  // their cells show the name the API resolved plus the id it grouped by — the id is what
  // a filter needs, the name is what a human reads. An id of 0 is the API's unknown bucket
  // (historical rows, or rows written without a credential), which the other dimensions
  // spell the same way.
  function groupKeyCell(row, groupColumn) {
    if (groupColumn === 'account' || groupColumn === 'api_key') {
      const isAccount = groupColumn === 'account';
      const raw = isAccount ? row.account_id : row.api_key_id;
      const id = Number(raw ?? row.key ?? 0) || 0;
      if (!id) return el('span', { class: 'muted', text: '（未知）' });
      const name = isAccount ? row.account_name : row.api_key_name;
      const label = (name || '（无名字）') + ' #' + id;
      const title = !isAccount && row.api_key_prefix ? row.api_key_prefix + ' · #' + id : label;
      return el('span', { title, text: label });
    }
    return keyValueCell(row.key);
  }

  function keyValueCell(key) {
    if (!key) return el('span', { class: 'muted', text: '（未知）' });
    const text = String(key);
    return el('span', { title: text, text: text.length > 44 ? text.slice(0, 42) + '…' : text });
  }

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

  // The two credential filters list the configuration tables (bounded: the console's
  // config lists are read whole and sliced server-side). A list that cannot load must not
  // block the page — the filter simply stays at "all".
  async function loadAccountOptions() {
    try {
      const accounts = (await api.get('/accounts', { limit: 1000 })).data || [];
      const current = accountFilter.value;
      accountFilter.replaceChildren(el('option', { value: '', text: '全部用户' }),
        ...accounts.map((a) => el('option', { value: a.id, text: a.name })));
      accountFilter.value = current;
    } catch (err) { /* see above */ }
    await loadKeyOptions();
  }

  // The Key list follows the account: with an account selected it offers only the keys
  // that can actually appear in the rows. A selected key that is not in the new list (the
  // account changed) falls back to "all keys" rather than silently filtering by a key of
  // another account.
  async function loadKeyOptions() {
    const params = { limit: 1000 };
    if (accountFilter.value) params.account_id = accountFilter.value;
    try {
      const keys = (await api.get('/keys', params)).data || [];
      const current = keyFilter.value;
      keyFilter.replaceChildren(el('option', { value: '', text: '全部 API Key' }),
        ...keys.map((k) => el('option', { value: k.id, text: k.name + '（' + k.key_prefix + '）' })));
      keyFilter.value = keys.some((k) => String(k.id) === current) ? current : '';
    } catch (err) { /* see above */ }
  }

  // 统计卡在列表卡之上：它回答「谁在用、用哪个模型、花了多少」，是打开页面先看的问题；
  // 列表回答「具体是哪一条」。筛选栏留在列表卡里（它属于它筛的那张表），两张表共用。
  page.append(statsCard);
  page.append(card('请求日志', view.node, [
    days, accountFilter, keyFilter, client, model, sessionFilter, workspaceFilter,
    el('span', { class: 'muted', text: '用户（账户）/API Key 与客户端/模型/工作区/会话/标题、token 成本都是独立于正文口径记录的元数据（record_input=off 也记）；用户与 Key 的名字由账户/Key 表读时解析，分组按 id；标题来自会话的标题调用，成本来自计量表，与账单一致；列表底部的「本页汇总」只合计当前页已加载的行（含本页过滤），窗口口径看上方「维度统计」' }),
    hint]));

  // Changing any filter restarts both tables at page 1: the rows of the current page belong
  // to a different filter, so their offset is meaningless.
  days.addEventListener('change', () => { view.reset(); loadStats({ reset: true }); loadModelOptions(); });
  accountFilter.addEventListener('change', () => { view.reset(); loadStats({ reset: true }); loadKeyOptions(); });
  keyFilter.addEventListener('change', () => { view.reset(); loadStats({ reset: true }); });
  client.addEventListener('change', () => { view.reset(); loadStats({ reset: true }); });
  model.addEventListener('change', () => { view.reset(); loadStats({ reset: true }); });
  for (const input of [sessionFilter, workspaceFilter]) {
    input.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter') return;
      view.reset();
      loadStats({ reset: true });
    });
  }
  // The grouping and the sort key both re-rank the buckets, so both restart at page 1.
  groupBy.addEventListener('change', () => { loadStats({ reset: true }); });
  sortBy.addEventListener('change', () => { loadStats({ reset: true }); });
  // 「刷新」重新读当前这一页（两张表都保持位置）；清理过期日志会删掉整组，所以回第 1 页。
  refresh.addEventListener('click', () => { view.refresh(); loadStats(); loadModelOptions(); loadAccountOptions(); });
  prune.addEventListener('click', () => pruneNow());
  await loadRetention();
  await Promise.all([view.refresh(), loadStats(), loadModelOptions(), loadAccountOptions()]);
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

// ownerCell renders the account a request is billed to — 「用户」 on this page, the same
// entity the console calls 账户 elsewhere. The name is a read-time label from
// accounts.name; the tooltip carries the id the account_id filter takes. A row whose id has
// no name still shows what it has rather than a blank: the request exists and belongs to
// somebody.
function ownerCell(row) {
  if (!row.account_id && !row.account_name) return el('span', { class: 'muted', text: '—' });
  const text = row.account_name || '（无名字）';
  return el('span', { title: '账户 #' + row.account_id, text });
}

// apiKeyCell renders which key authenticated the request: name plus the stable prefix in
// the tooltip. Nothing here is a secret — the prefix is what the console already lists.
function apiKeyCell(row) {
  if (!row.api_key_id && !row.api_key_name) return el('span', { class: 'muted', text: '—' });
  const text = row.api_key_name || '（无名字）';
  const title = (row.api_key_prefix ? row.api_key_prefix + ' · ' : '') + 'Key #' + row.api_key_id;
  return el('span', { title, text });
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
// body: the row keeps this even when record_input is off. The first two fields are the
// credential dimensions (M30): the account this request is billed to — 「用户」 on this
// page, the console's 账户 elsewhere — and the API key it authenticated with. Their names
// are read-time labels; the ids are what the filters take, so both are shown.
function identityBlock(row) {
  const owner = row.account_id
    ? (row.account_name || '（无名字）') + ' #' + row.account_id
    : '—';
  const apiKey = row.api_key_id
    ? (row.api_key_name || '（无名字）') + ' #' + row.api_key_id + (row.api_key_prefix ? ' · ' + row.api_key_prefix : '')
    : '—';
  const fields = [
    ['用户（账户）', owner],
    ['API Key', apiKey],
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
