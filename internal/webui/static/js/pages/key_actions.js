// 一个账号下的 Key 操作（M72）：组织架构页的账号行与 API Keys 页共用这份实现。
//
// 抽出来的原因很具体：创建 Key 时明文只出现一次，那段「创建 → 单独提交录制开关 → 弹明文」
// 的顺序如果抄成两份，迟早会有一份忘记第二步，于是悄悄把 Key 建成一个非预期的录制状态。
import { api } from '../api.js';
import { el, modal, toast, modalHead, modalBody, modalActions } from '../ui.js';

// 与 internal/config.RecordingInputModes 对齐；internal/webui 的测试守着这份表不漂移。
// user 档的字数上限（recording.input_max_chars，M81）是部署级配置、控制台看不到具体数值，
// 所以这里只说「长消息按上限截断」，不写死 100。
export const INPUT_MODES = [
  { value: 'inherit', label: '继承全局默认（默认：只记用户输入，长消息按上限截断）' },
  { value: 'user', label: 'user：只记用户输入（长消息按上限截断）' },
  { value: 'full', label: 'full：整份请求正文（排障用，不受上限影响）' },
  { value: 'metadata', label: 'metadata：只记元数据（不落正文）' },
  { value: 'off', label: 'off：不记录输入' },
];

// createKeyForAccount 为指定账号签发一把 Key（可预填名称），成功后展示一次明文。
// 返回新建的 Key，取消时返回 null。
export async function createKeyForAccount(account, { name = '' } = {}) {
  const result = await modal({
    title: '新建 API Key — ' + account.name,
    submitLabel: '创建',
    fields: [
      { name: 'name', label: '名称', required: true, value: name,
        hint: '同一账号下建议用能分辨用途的名字（如 laptop / phone）' },
      { name: 'tags', label: '标签（逗号分隔）', hint: '与账号标签取并集，决定可用的模型与供应商' },
      { name: 'record_input_mode', label: '输入录制模式', type: 'select', options: INPUT_MODES, value: 'inherit' },
      { name: 'record_output_text', label: '记录最终输出文本', type: 'checkbox' },
      { name: 'record_reasoning', label: '记录思考文本', type: 'checkbox' },
    ],
    onSubmit: async (values) => {
      const created = await api.post('/keys', {
        name: values.name,
        account_id: account.id,
        tags: values.tags ? values.tags.split(',').map((t) => t.trim()).filter(Boolean) : [],
      });
      // 录制开关是每把 Key 的策略，创建后立刻单独提交：对话框关掉不会留下一个意料之外的录制状态。
      await api.patch('/keys/' + created.id, {
        record_input_mode: values.record_input_mode || 'inherit',
        record_output_text: !!values.record_output_text,
        record_reasoning: !!values.record_reasoning,
      });
      return created;
    },
  });
  if (!result) return null;
  showSecret('API Key 已创建', result.key);
  return result;
}

// editKey 改一把 Key 的标签、录制开关、状态与配额策略。
export async function editKey(row, reload) {
  const result = await modal({
    title: '编辑 ' + row.name,
    fields: [
      { name: 'tags', label: 'Key 标签（逗号分隔）', hint: '与账号标签取并集；空输入清空 Key 自有标签', value: (row.tags || []).join(', ') },
      { name: 'record_input_mode', label: '输入录制模式', type: 'select', options: INPUT_MODES, value: row.record_input_mode },
      { name: 'record_output_text', label: '记录最终输出文本', type: 'checkbox', value: row.record_output_text },
      { name: 'record_reasoning', label: '记录思考文本', type: 'checkbox', value: row.record_reasoning },
      { name: 'status', label: '状态', type: 'select', options: ['active', 'suspended', 'revoked'], value: row.status },
      // 配额策略只读扁平字段（rpm/tpm/concurrency 会被执行），其它字段由服务端拒绝而不是存下来被忽略。
      { name: 'policy', label: '配额策略（扁平 JSON）', type: 'textarea', json: true, rows: 6,
        hint: '例：{"rpm":60,"concurrency":4}；留空=不改动', value: row.policy ? JSON.stringify(row.policy, null, 2) : '' },
    ],
    onSubmit: (values) => api.patch('/keys/' + row.id, {
      tags: (values.tags || '').split(',').map((tag) => tag.trim()).filter(Boolean),
      record_input_mode: values.record_input_mode,
      record_output_text: !!values.record_output_text,
      record_reasoning: !!values.record_reasoning,
      status: values.status,
      ...(values.policy === undefined ? {} : { policy: values.policy }),
    }),
  });
  if (result) { toast('已更新', 'ok'); if (reload) await reload(); }
}

// toggleKey 启用/停用一把 Key（停用后它不能登录门户，也不能调用模型）。
export async function toggleKey(row, reload) {
  const next = row.status === 'active' ? 'suspended' : 'active';
  const result = await modal({
    title: (next === 'suspended' ? '停用 ' : '启用 ') + row.name,
    submitLabel: next === 'suspended' ? '停用' : '启用',
    fields: [{ name: 'confirm', label: '确认', type: 'checkbox', required: true,
      hint: next === 'suspended' ? '停用后该 Key 立刻不能调用模型，也不能再用它登录门户' : '启用后该 Key 立刻可用' }],
    onSubmit: () => api.patch('/keys/' + row.id, { status: next }),
  });
  if (result) { toast('已更新', 'ok'); if (reload) await reload(); }
}

// showSecret 展示一次性明文。对话框关掉之后服务端也拿不回来，所以这里必须说清楚。
export function showSecret(title, secret) {
  const box = el('input', { value: secret, readonly: true });
  const copy = el('button', { class: 'btn', text: '复制' });
  copy.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(secret); toast('已复制', 'ok'); }
    catch (err) { box.select(); document.execCommand('copy'); toast('已复制', 'ok'); }
  });
  const done = el('button', { class: 'btn btn-primary', text: '我已保存' });
  const dialog = el('div', { class: 'modal' }, [
    modalHead(title, close),
    modalBody([
      el('p', { class: 'muted', text: '明文只显示这一次：关闭后无法再次查看，只能重新签发。' }),
      box,
    ]),
    modalActions([copy, done]),
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  const close = () => backdrop.remove();
  done.addEventListener('click', close);
  backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(); });
  document.getElementById('modal-root').append(backdrop);
}
