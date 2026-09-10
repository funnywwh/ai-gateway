import { api } from '../api.js';
import { el, card, toast, jsonBlock } from '../ui.js';

const KNOWN_KEYS = [
  'recording.default',
  'recording.max_bytes',
  'mcp.max_query_rows',
  'backup.retention',
];

export async function render({ page }) {
  const keyInput = el('input', { placeholder: '设置键，例如 recording.default', list: 'known-keys' });
  const datalist = el('datalist', { id: 'known-keys' }, KNOWN_KEYS.map((k) => el('option', { value: k })));
  const load = el('button', { class: 'btn', text: '读取' });
  const valueBox = el('textarea', { rows: 10 });
  const save = el('button', { class: 'btn btn-primary', text: '保存' });
  const output = el('div');

  load.addEventListener('click', async () => {
    if (!keyInput.value.trim()) return;
    try {
      const result = await api.get('/settings', { key: keyInput.value.trim() });
      const value = (result.data || {})[keyInput.value.trim()];
      valueBox.value = value === null || value === undefined ? '' : JSON.stringify(value, null, 2);
      output.replaceChildren(jsonBlock(value));
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  });

  save.addEventListener('click', async () => {
    const key = keyInput.value.trim();
    if (!key) { toast('请填写设置键', 'error'); return; }
    let value;
    try { value = JSON.parse(valueBox.value || 'null'); }
    catch (err) { toast('值不是合法 JSON', 'error'); return; }
    try {
      await api.put('/settings/' + encodeURIComponent(key), { value });
      toast('已保存', 'ok');
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  });

  page.append(card('设置', [
    el('div', { class: 'toolbar' }, [keyInput, datalist, load]),
    el('label', { class: 'field' }, [el('span', { text: 'JSON 值' }), valueBox]),
    el('div', { class: 'toolbar' }, [save]),
    output,
  ], [el('span', { class: 'muted', text: '设置以 JSON 存储；启动配置（config.yaml）优先级低于环境变量，设置项由各模块自行读取' })]));
}