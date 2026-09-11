import { api } from '../api.js';
import { el, card, table, modal, toast, confirmDialog } from '../ui.js';

const SAMPLE_GRANTS = JSON.stringify({ models: ['gpt-*'], providers: ['*'] }, null, 2);
// The quota policy is FLAT: only top-level fields are read (rpm/tpm/concurrency are
// enforced, monthly_* are parsed but not checked yet). A nested {"rate_limit":{...}}
// document is rejected by the server, because it used to be stored and then ignored.
const SAMPLE_POLICY = JSON.stringify({ rpm: 60, concurrency: 4 }, null, 2);

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const create = el('button', { class: 'btn btn-primary', text: '新建标签', disabled: readonly });
  const refresh = el('button', { class: 'btn', text: '刷新' });
  actions.append(refresh, create);
  let view;

  async function load() {
    const payload = await api.get('/tags');
    const rows = payload.data || [];
    if (!view) {
      view = table({
        columns: [
          { key: 'name', label: '名称' },
          { key: 'description', label: '说明' },
          { key: 'priority', label: '优先级' },
          { key: 'grants', label: '授权', render: (row) => el('code', { text: JSON.stringify(row.grants || {}) }) },
          { key: 'policy', label: '策略', render: (row) => el('code', { text: JSON.stringify(row.policy || {}) }) },
        ],
        rows,
        rowActions: (row) => readonly ? [] : [
          el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, load) }),
          el('button', { class: 'btn btn-danger', text: '删除', onclick: () => remove(row, load) }),
        ],
      });
      page.append(card('标签（API Key 分组）', view.node, [
        el('span', { class: 'muted', text: 'Key 的可用模型/供应商 = 全部标签授权的并集；策略按 tag 覆盖 key' })]));
    } else {
      view.refresh(rows);
    }
  }

  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  create.addEventListener('click', () => form(null, load));
  await load();
}

function form(row, reload) {
  return modal({
    title: row ? '编辑标签 ' + row.name : '新建标签',
    wide: true,
    fields: [
      { name: 'name', label: '名称', required: !row, value: row ? row.name : '' },
      { name: 'description', label: '说明', value: row ? row.description : '' },
      { name: 'priority', label: '优先级', type: 'number', value: row ? row.priority : 100 },
      { name: 'grants', label: '授权（JSON）', type: 'textarea', json: true, rows: 8,
        value: row && row.grants ? JSON.stringify(row.grants, null, 2) : SAMPLE_GRANTS },
      { name: 'policy', label: '策略（JSON，扁平字段）', type: 'textarea', json: true, rows: 8,
        value: row && row.policy ? JSON.stringify(row.policy, null, 2) : SAMPLE_POLICY },
    ],
    onSubmit: async (values) => {
      await api.post('/tags', values);
      toast('已保存', 'ok');
      await reload();
      return true;
    },
  });
}

async function edit(row, reload) { await form(row, reload); }

async function remove(row, reload) {
  const ok = await confirmDialog('删除标签', '确认删除标签 ' + row.name + ' 吗？已签发的 Key 上的同名标签会失效。');
  if (!ok) return;
  await api.del('/tags/' + row.id);
  toast('已删除', 'ok');
  await reload();
}