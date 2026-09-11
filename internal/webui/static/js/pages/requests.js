import { api } from '../api.js';
import { el, card, pagedTable, toast, badge, formatTime, jsonBlock, statusBadge, modalHead, modalBody, modalActions } from '../ui.js';

export async function render({ page, actions }) {
  const days = el('select', {}, [1, 3, 7, 30].map((n) => el('option', { value: n, text: '最近 ' + n + ' 天' })));
  days.value = '7';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh);

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
    el('span', { class: 'muted', text: '默认只记录用户输入；系统指令、工具定义与工具输出只留计数，最终输出与思考文本需在 Key 上单独开启' })]));

  // Changing the time window restarts at page 1: the rows of the current page belong
  // to a different filter, so their offset is meaningless.
  days.addEventListener('change', () => view.reset());
  refresh.addEventListener('click', () => view.refresh());
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