import { api } from '../api.js';
import { el, card, pagedTable, modal, toast, confirmDialog } from '../ui.js';

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

  const view = pagedTable({
    columns: [
      { key: 'name', label: '名称' },
      { key: 'description', label: '说明' },
      { key: 'priority', label: '优先级' },
      { key: 'grants', label: '授权', render: (row) => el('code', { text: JSON.stringify(row.grants || {}) }) },
      { key: 'policy', label: '策略', render: (row) => el('code', { text: JSON.stringify(row.policy || {}) }) },
    ],
    rowActions: (row) => readonly ? [] : [
      el('button', { class: 'btn', text: '编辑', onclick: () => edit(row, () => view.refresh()) }),
      el('button', { class: 'btn btn-danger', text: '删除', onclick: () => remove(row, () => view.refresh()) }),
    ],
    load: ({ limit, offset }) => api.get('/tags', { limit, offset }),
    onError: (err) => toast(api.errorMessage(err), 'error'),
  });
  page.append(card('标签（账号与 API Key）', view.node, [
    el('span', { class: 'muted', text: '标签可绑定账号或 API Key；生效授权 = 账号标签与 Key 标签的并集，策略按优先级合并后由 Key 覆盖' })]));

  refresh.addEventListener('click', () => view.refresh());
  create.addEventListener('click', () => form(null, () => view.refresh()));
  await view.refresh();
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
  const ok = await confirmDialog('删除标签', '确认删除标签 ' + row.name + ' 吗？账号和 Key 上的同名绑定都会在刷新后失效。');
  if (!ok) return;
  await api.del('/tags/' + row.id);
  toast('已删除', 'ok');
  await reload();
}
