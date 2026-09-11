import { api } from '../api.js';
import { el, card, toast, jsonBlock } from '../ui.js';
import { initCurrency, currencies, ledgerCurrency, displayCurrency, fxSource, missingRates } from '../money.js';

// Only settings a module actually reads are suggested: any other key would be stored and
// then ignored, which looks like a configuration change that never happened. The
// recording policy and the MCP/backup knobs live in config.yaml.
const KNOWN_KEYS = [
  'billing.fx_rates',
];

export async function render({ page, session }) {
  const readonly = session.role !== 'admin';
  const keyInput = el('input', { placeholder: '设置键，例如 billing.fx_rates', list: 'known-keys' });
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
      await refreshRates();
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  });

  const ratesHost = el('div');
  page.append(card('通用设置', [
    el('div', { class: 'toolbar' }, [keyInput, datalist, load]),
    el('label', { class: 'field' }, [el('span', { text: 'JSON 值' }), valueBox]),
    el('div', { class: 'toolbar' }, [save]),
    output,
  ], [el('span', { class: 'muted', text: '设置以 JSON 存储，只有实现了读取方的设置键才会生效（当前只有汇率表）；录制口径与 MCP/备份参数在 config.yaml 里配置' })]));

  // The FX table is the one setting the console edits structurally: a mistyped rate
  // would turn into a wrong charge, so the server validates it (and reloads the live
  // table) while this editor keeps the operator honest about the micros.
  async function refreshRates() {
    ratesHost.replaceChildren(el('div', { class: 'empty', text: '加载中…' }));
    try {
      const payload = await api.get('/billing/currency');
      ratesHost.replaceChildren(ratesCard(payload, refreshRates, readonly));
    } catch (err) {
      ratesHost.replaceChildren(el('div', { class: 'empty', text: api.errorMessage(err) }));
    }
  }

  page.append(ratesHost);
  await initCurrency();
  await refreshRates();
}

function ratesCard(payload, reload, readonly) {
  const ledger = payload.ledger_currency || 'USD';
  const rows = el('div', { class: 'grid' });
  const entries = (payload.currencies || []).filter((entry) => entry.code !== ledger);

  const addRow = (code, rate) => {
    const codeInput = el('input', { value: code, placeholder: '币种，例如 CNY', style: 'max-width:120px' });
    const rateInput = el('input', { type: 'number', min: '1', step: '1', value: rate ? String(rate) : '', placeholder: '微' + ledger, style: 'max-width:160px' });
    const hint = el('span', { class: 'muted' });
    const explain = () => {
      const value = Number(rateInput.value || 0);
      hint.textContent = value > 0 ? '＝ 1 ' + (codeInput.value || '?') + ' = ' + (value / 1e6).toFixed(6) + ' ' + ledger : '';
    };
    codeInput.addEventListener('input', explain);
    rateInput.addEventListener('input', explain);
    explain();
    const remove = el('button', { class: 'btn btn-danger', text: '删除', disabled: readonly });
    const row = el('div', { class: 'toolbar' }, [
      el('span', { text: '1' }), codeInput, el('span', { text: '=' }), rateInput, hint, remove,
    ]);
    remove.addEventListener('click', () => row.remove());
    rows.append(row);
  };

  entries.forEach((entry) => addRow(entry.code, entry.rate_micros));

  const add = el('button', { class: 'btn', text: '新增币种', disabled: readonly });
  add.addEventListener('click', () => addRow('', 0));

  const save = el('button', { class: 'btn btn-primary', text: '保存汇率表', disabled: readonly });
  save.addEventListener('click', async () => {
    const value = {};
    for (const row of rows.children) {
      const [codeInput, rateInput] = row.querySelectorAll('input');
      const code = codeInput.value.trim().toUpperCase();
      if (!code) continue;
      const micros = Number(rateInput.value || 0);
      if (!Number.isInteger(micros) || micros <= 0) {
        toast(code + ' 的汇率必须是正整数微单位（1 CNY = 0.141000 USD 写作 141000）', 'error');
        return;
      }
      value[code] = micros;
    }
    try {
      await api.put('/settings/billing.fx_rates', { value });
      toast('汇率表已保存并立即生效', 'ok');
      await reload();
    } catch (err) { toast(api.errorMessage(err), 'error'); }
  });

  const missing = payload.missing_rates || [];
  return card('汇率表（多币种）', [
    el('div', { class: 'toolbar' }, [
      el('span', { text: '账本币种：' }), el('code', { text: ledger }),
      el('span', { class: 'muted', text: '所有余额/账本/发票都记在这个币种' }),
      el('span', { text: '默认显示币种：' }), el('code', { text: payload.display_currency || ledger }),
      el('span', { class: 'muted', text: '来源 ' + (payload.fx_source || 'config') }),
    ]),
    el('p', { class: 'muted', text: '每行是「1 单位该币种 = N 微' + ledger + '」；模型可以在自己的定价文档里声明币种，结算时按这里的汇率换算入账。' }),
    rows,
    el('div', { class: 'toolbar' }, [add, save]),
    missing.length
      ? el('div', { class: 'muted', text: '⚠ 以下币种被某个模型的定价使用但没有汇率，该模型目前无法计价（成本与收费记 0）：' + missing.join(' / ') })
      : null,
  ].filter(Boolean), [el('span', { class: 'muted', text: '保存写入 settings: billing.fx_rates，覆盖 config.yaml 中同币种的汇率' })]);
}
