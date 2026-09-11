import { api } from '../api.js';
import { el, card, table, toast, badge, formatTime, jsonBlock, statusBadge, modalHead } from '../ui.js';

export async function render({ page, actions }) {
  const days = el('select', {}, [1, 3, 7, 30].map((n) => el('option', { value: n, text: '最近 ' + n + ' 天' })));
  days.value = '7';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh);
  let view;

  async function load() {
    const payload = await api.get('/requests', { days: days.value, limit: 500 });
    const rows = payload.data || [];
    if (!view) {
      view = table({
        columns: [
          { key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
          { key: 'request_id', label: '请求 ID', render: (row) => el('code', { text: row.request_id }) },
          { key: 'endpoint', label: '端点' },
          { key: 'status', label: '状态', render: (row) => statusBadge(row.status) },
          { key: 'input_recorded', label: '输入', render: (row) => row.input_recorded ? badge('已录制', 'ok') : badge('未录制') },
          { key: 'output_text_recorded', label: '输出文本', render: (row) => row.output_text_recorded ? badge('已录制', 'ok') : badge('未录制') },
          { key: 'reasoning_recorded', label: '思考文本', render: (row) => row.reasoning_recorded ? badge('已录制', 'ok') : badge('未录制') },
        ],
        rows,
        rowActions: (row) => [el('button', { class: 'btn', text: '详情', onclick: () => detail(row.request_id) })],
      });
      page.append(card('请求日志', view.node, [
        days,
        el('span', { class: 'muted', text: '默认只记录输入；最终输出与思考文本需在 Key 上单独开启' })]));
    } else {
      view.refresh(rows);
    }
  }

  days.addEventListener('change', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  await load();
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
    el('div', { class: 'muted', text: row.endpoint + ' · ' + formatTime(row.created_at) + ' · HTTP ' + row.status }),
    body,
    el('div', { class: 'modal-actions' }, [el('button', { class: 'btn', text: '关闭', onclick: () => close() })]),
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