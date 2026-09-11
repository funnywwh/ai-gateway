import { api } from '../api.js';
import { el, card, pagedTable, toast, badge, formatTime, jsonBlock, statusBadge, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';

export async function render({ page, actions, session }) {
  const days = el('select', {}, [1, 3, 7, 30].map((n) => el('option', { value: n, text: '最近 ' + n + ' 天' })));
  days.value = '7';
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

  const view = pagedTable({
    columns: [
      { key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
      { key: 'request_id', label: '请求 ID', render: (row) => el('code', { text: row.request_id }) },
      { key: 'endpoint', label: '端点' },
      { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
      { key: 'input_recorded', label: '输入', render: (row) => row.input_recorded ? badge('已录制', 'ok') : badge('未录制') },
      { key: 'output_text_recorded', label: '输出文本', render: (row) => row.output_text_recorded ? badge('已录制', 'ok') : badge('未录制') },
      { key: 'reasoning_recorded', label: '思考文本', render: (row) => row.reasoning_recorded ? badge('已录制', 'ok') : badge('未录制') },
    ],
    empty: '该窗口内没有请求日志',
    rowActions: (row) => [el('button', { class: 'btn', text: '详情', onclick: () => detail(row.request_id) })],
    load: ({ limit, offset }) => api.get('/requests', { days: days.value, limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('请求日志', view.node, [
    days,
    el('span', { class: 'muted', text: '默认只记录用户输入；系统指令、工具定义与工具输出只留计数，最终输出与思考文本需在 Key 上单独开启' }),
    hint]));

  // Changing the time window restarts at page 1: the rows of the current page belong
  // to a different filter, so their offset is meaningless.
  days.addEventListener('change', () => view.reset());
  refresh.addEventListener('click', () => view.refresh());
  prune.addEventListener('click', () => pruneNow());
  await loadRetention();
  await view.refresh();
}

async function detail(requestID) {
  const row = await api.get('/requests/' + encodeURIComponent(requestID));
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

function panel(title, value) {
  return el('div', {}, [el('h4', { text: title }), jsonBlock(value)]);
}