import { api } from '../api.js';
import { el, card, modal, toast, badge, stat, jsonBlock, confirmDialog } from '../ui.js';

const MICRO = 1_000_000;
const money = (micros) => (Number(micros || 0) / MICRO).toFixed(6);
const bpToFactor = (bp) => (Number(bp || 0) / 10000);
const factorToBP = (factor) => Math.round(Number(factor || 0) * 10000);

const TEMPLATES = {
  'DeepSeek（标准/优惠 × 缓存命中/未命中 × 输入分档）': {
    rules: [
      { id: 'offpeak', order: 10, title: '优惠时段',
        when: { time_windows: [{ start: '16:30', end: '00:30', tz: 'UTC' }] },
        rates: { input_cache_hit: 35000, input_cache_miss: 135000, output: 550000 } },
      { id: 'long-input', order: 20, title: '输入 >= 32K',
        when: { tier: { basis: 'input', gte: 32768 } },
        rates: { input_cache_hit: 140000, input_cache_miss: 540000, output: 2200000 } },
      { id: 'standard', order: 100, title: '标准时段', when: {},
        rates: { input_cache_hit: 70000, input_cache_miss: 270000, output: 1100000 } },
    ],
  },
  'OpenAI 长上下文档（>128K 加倍）': {
    rules: [
      { id: 'long-context', order: 10, title: '长上下文',
        when: { tier: { basis: 'input', gte: 128000 } },
        rates: { input: 5000000, output: 15000000 } },
      { id: 'standard', order: 100, when: {}, rates: { input: 2500000, output: 10000000 } },
    ],
  },
  'Ollama 本地零价': {
    rules: [{ id: 'free', order: 10, when: {}, rates: { input: 0, output: 0 } }],
  },
};

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  const tabMarkup = el('button', { class: 'btn btn-primary', text: '倍数（推荐）' });
  const tabRules = el('button', { class: 'btn', text: '高级：绝对定价 / 促销规则' });
  actions.append(tabMarkup, tabRules, refresh);

  const body = el('div');
  page.append(body);
  let targets = [];
  let mode = 'markup';
  let selected = null;
  const search = el('input', { type: 'search', placeholder: '过滤模型…' });

  async function load() {
    const payload = await api.get('/pricing/targets');
    targets = payload.targets || [];
    if (!selected || !targets.some((t) => keyOf(t) === keyOf(selected))) {
      selected = targets.find((t) => t.kind === 'sale') || targets[0] || null;
    }
    renderBody();
  }

  function keyOf(target) {
    return target ? target.kind + ':' + target.model + ':' + (target.provider_id || 0) : '';
  }

  function renderBody() {
    body.replaceChildren();
    if (!targets.length) {
      body.append(card('定价', [el('div', { class: 'empty', text: '还没有模型或供应商模型：先在「模型与路由」页建好再回来定价' })]));
      return;
    }
    const picker = el('div', { class: 'card' }, [
      el('div', { class: 'toolbar' }, [search, el('span', { class: 'muted', text: targets.length + ' 个定价目标' })]),
      el('div', { class: 'grid' }, targets.filter(matches).map((target) => targetButton(target))),
    ]);
    body.append(picker);
    if (selected) {
      body.append(mode === 'markup' ? markupPanel(selected) : rulesPanel(selected));
    }
  }

  function matches(target) {
    const needle = search.value.trim().toLowerCase();
    if (!needle) return true;
    return JSON.stringify(target).toLowerCase().includes(needle);
  }

  function targetButton(target) {
    const active = selected && keyOf(selected) === keyOf(target);
    const node = el('button', {
      class: 'stat', style: 'text-align:left;cursor:pointer;' + (active ? 'border:1px solid var(--accent)' : ''),
      onclick: () => { selected = target; renderBody(); },
    }, [
      el('div', { class: 'k', text: target.kind === 'sale' ? '对客定价' : '成本价 · ' + (target.provider_name || target.provider_id) }),
      el('div', { class: 'v', text: target.model }),
      el('div', { class: 'muted', text: target.kind === 'sale'
        ? ('倍数 ×' + bpToFactor(target.markup_bp).toFixed(2) + ' · 来源 ' + ((target.effective_markup || {}).source || '—'))
        : ((target.rules || []).length + ' 条规则 · 上游 ' + (target.upstream_model || '—')) }),
      target.valid === false ? badge('JSON 错误', 'danger') : null,
      target.kind === 'sale' && target.cost_rules_configured === false ? badge('成本规则缺失', 'danger') : null,
      target.shadowed > 0 ? badge(target.shadowed + ' 条遮蔽', 'warn') : null,
    ].filter(Boolean));
    return node;
  }

  // ---- markup panel ------------------------------------------------------

  function markupPanel(target) {
    const state = {
      bp: Number(target.markup_bp || 10000),
      basis: target.basis || 'cost_follow',
      dimensions: { ...(target.dimension_markup_bp || {}) },
    };
    const factor = el('input', { type: 'number', step: '0.05', min: '0', value: bpToFactor(state.bp).toFixed(2), style: 'max-width:140px' });
    const bp = el('input', { type: 'number', step: '100', min: '0', value: String(state.bp), style: 'max-width:160px' });
    factor.addEventListener('input', () => { state.bp = factorToBP(factor.value); bp.value = String(state.bp); updatePreview(); });
    bp.addEventListener('input', () => { state.bp = Number(bp.value || 0); factor.value = bpToFactor(state.bp).toFixed(2); updatePreview(); });

    const dimensionRows = el('div', { class: 'grid' });
    const KNOWN = ['input', 'input_cache_hit', 'input_cache_miss', 'output', 'reasoning'];
    for (const dimension of KNOWN) {
      const input = el('input', { type: 'number', step: '0.05', min: '0', style: 'max-width:120px',
        value: state.dimensions[dimension] !== undefined ? bpToFactor(state.dimensions[dimension]).toFixed(2) : '',
        placeholder: '继承 ' + bpToFactor(state.bp).toFixed(2) });
      input.addEventListener('input', () => {
        if (input.value === '') delete state.dimensions[dimension];
        else state.dimensions[dimension] = factorToBP(input.value);
        updatePreview();
      });
      dimensionRows.append(el('label', { class: 'field' }, [el('span', { text: dimension }), input]));
    }

    const preview = el('div');
    const previewInput = el('input', { type: 'number', value: '1000', style: 'max-width:120px' });
    const previewOutput = el('input', { type: 'number', value: '500', style: 'max-width:120px' });
    previewInput.addEventListener('input', updatePreview);
    previewOutput.addEventListener('input', updatePreview);

    const save = el('button', { class: 'btn btn-primary', text: '保存倍数', disabled: readonly });
    save.addEventListener('click', async () => {
      try {
        await api.patch('/pricing/markup', {
          model: target.model, basis: state.basis, markup_bp: state.bp,
          dimension_markup_bp: state.dimensions,
        });
      } catch (err) { toast(api.errorMessage(err), 'error'); return; }
      toast('倍数已保存，下一次请求即生效', 'ok');
      await load();
    });

    async function updatePreview() {
      const dimensions = { input: Number(previewInput.value || 0), output: Number(previewOutput.value || 0) };
      try {
        const result = await api.post('/pricing/simulate', {
          model: target.model, dimensions,
          sale_rules: { basis: 'cost_follow', markup_bp: state.bp, dimension_markup_bp: state.dimensions },
        });
        preview.replaceChildren(el('div', { class: 'grid' }, [
          stat('成本（USD）', money(result.cost_micros)),
          stat('售价（USD）', money(result.charge_micros)),
          stat('毛利（USD）', money((result.charge_micros || 0) - (result.cost_micros || 0))),
          stat('规模', '×' + bpToFactor(result.markup_bp || state.bp).toFixed(2)),
        ]), jsonBlock({ cost_lines: result.cost_lines, sale_lines: result.sale_lines }));
      } catch (err) {
        preview.replaceChildren(el('div', { class: 'empty', text: api.errorMessage(err) }));
      }
    }

    const panels = [
      card('倍数', [
        el('div', { class: 'toolbar' }, [
          el('span', { text: '售价 = 成本 ×' }), factor,
          el('span', { class: 'muted', text: '基点' }), bp,
          badge(((target.effective_markup || {}).source) || '—'),
        ]),
        el('p', { class: 'muted', text: '倍数模式下，上游调价、优惠时段与缓存命中都会自动跟随；若要固定价，请用「高级」标签页的绝对定价。' }),
        save,
      ]),
      card('按维度覆写（留空＝继承总倍数）', [el('div', { class: 'grid' }, Array.from(dimensionRows.children))]),
      card('实时试算', [
        el('div', { class: 'toolbar' }, [el('span', { text: '输入 tokens' }), previewInput, el('span', { text: '输出 tokens' }), previewOutput]),
        preview,
      ]),
    ];
    if (target.cost_rules_configured === false) {
      panels.unshift(el('div', { class: 'card', style: 'border-color:var(--danger)' }, [
        el('strong', { text: '该模型还没有任何配置了成本规则的供应商映射：倍数为空转，售价会是 0。' }),
        el('p', { class: 'muted', text: '先到「模型供应商」页给某个供应商的该模型填上成本规则（pricing_rules），再回来设置倍数。' }),
      ]));
    }
    if (target.valid === false) {
      panels.unshift(el('div', { class: 'card', style: 'border-color:var(--danger)' }, [
        el('strong', { text: '当前售价文档无法解析：' + (target.parse_error || '') }),
        el('p', { class: 'muted', text: '请在「高级」标签页用 JSON 视图修复后再改倍数。' }),
      ]));
    }
    updatePreview();
    return el('div', {}, panels);
  }

  // ---- advanced rules panel ---------------------------------------------

  function rulesPanel(target) {
    const original = { basis: target.basis || '', markup_bp: target.markup_bp || 0, rules: target.rules || [] };
    const textarea = el('textarea', { rows: 18 });
    textarea.value = JSON.stringify(original, null, 2);
    const validation = el('div');
    const simulation = el('div');
    let timer = null;

    async function validate() {
      if (timer) clearTimeout(timer);
      timer = setTimeout(async () => {
        let parsed;
        try { parsed = JSON.parse(textarea.value); }
        catch (err) { validation.replaceChildren(el('div', { class: 'empty', text: 'JSON 解析失败：' + err.message })); return; }
        try {
          const result = await api.post('/pricing/validate', parsed);
          if (!result.valid) {
            validation.replaceChildren(el('div', { class: 'empty', text: '规则不合法：' + result.error }));
            return;
          }
          validation.replaceChildren(el('div', {}, [
            stat('规则数', String(result.rules)),
            result.shadowed && result.shadowed.length
              ? el('div', {}, [badge(result.shadowed.length + ' 条被遮蔽', 'warn'), jsonBlock(result.shadowed)])
              : el('div', { class: 'empty', text: '没有被遮蔽的规则' }),
          ]));
        } catch (err) { toast(api.errorMessage(err), 'error'); }
      }, 400);
    }
    textarea.addEventListener('input', validate);
    validate();

    const applyTemplate = el('button', { class: 'btn', text: '套用模板' });
    applyTemplate.addEventListener('click', async () => {
      const choice = await modal({
        title: '套用模板',
        fields: [{ name: 'name', label: '模板', type: 'select', options: Object.keys(TEMPLATES) }, { name: 'replace', label: '替换现有规则（否则追加）', type: 'checkbox' }],
        onSubmit: (values) => values,
      });
      if (!choice) return;
      const template = TEMPLATES[choice.name];
      let document;
      try { document = JSON.parse(textarea.value || '{}'); }
      catch (err) { document = {}; }
      document.rules = choice.replace ? template.rules : [].concat(document.rules || [], template.rules);
      textarea.value = JSON.stringify(document, null, 2);
      validate();
    });

    const simulate = el('button', { class: 'btn', text: '用当前内容试算' });
    simulate.addEventListener('click', async () => {
      let parsed;
      try { parsed = JSON.parse(textarea.value); } catch (err) { toast('JSON 解析失败', 'error'); return; }
      const isSale = target.kind === 'sale';
      try {
        const result = await api.post('/pricing/simulate', {
          model: target.model, dimensions: { input: 1000, output: 500 },
          cost_rules: isSale ? undefined : parsed,
          sale_rules: isSale ? parsed : undefined,
        });
        simulation.replaceChildren(el('div', { class: 'grid' }, [
          stat('命中规则', result.cost_rule_id || result.sale_rule_id || '（兜底）'),
          stat('成本（USD）', money(result.cost_micros)),
          stat('售价（USD）', money(result.charge_micros)),
        ]), jsonBlock({ cost_lines: result.cost_lines, sale_lines: result.sale_lines }));
      } catch (err) { toast(api.errorMessage(err), 'error'); }
    });

    const save = el('button', { class: 'btn btn-primary', text: target.kind === 'sale' ? '保存到模型售价' : '保存为成本规则', disabled: readonly });
    save.addEventListener('click', async () => {
      let parsed;
      try { parsed = JSON.parse(textarea.value); } catch (err) { toast('JSON 解析失败，未保存', 'error'); return; }
      try {
        if (target.kind === 'sale') {
          await api.patch('/models/' + encodeURIComponent(target.model), { sale_pricing: parsed });
        } else {
          await api.post('/providers/' + target.provider_id + '/models', {
            public_model: target.model, upstream_model: target.upstream_model,
            pricing_rules: parsed,
          });
        }
      } catch (err) { toast(api.errorMessage(err), 'error'); return; }
      toast('已保存', 'ok');
      await load();
    });

    const ladder = el('button', { class: 'btn', text: '阶梯预览 1K/8K/32K/128K' });
    ladder.addEventListener('click', async () => {
      let parsed;
      try { parsed = JSON.parse(textarea.value); } catch (err) { toast('JSON 解析失败', 'error'); return; }
      const rows = [];
      for (const size of [1024, 8192, 32768, 131072]) {
        const result = await api.post('/pricing/simulate', {
          model: target.model, dimensions: { input: size, output: 1024 },
          cost_rules: target.kind === 'sale' ? undefined : parsed,
          sale_rules: target.kind === 'sale' ? parsed : undefined,
        }).catch(() => null);
        if (result) {
          rows.push({ 输入: size, 成本: money(result.cost_micros), 售价: money(result.charge_micros),
            毛利: money((result.charge_micros || 0) - (result.cost_micros || 0)) });
        }
      }
      simulation.replaceChildren(el('table', {}, [
        el('thead', {}, [el('tr', {}, ['输入', '成本', '售价', '毛利'].map((h) => el('th', { text: h })))]),
        el('tbody', {}, rows.map((row) => el('tr', {}, Object.values(row).map((v) => el('td', { text: String(v) }))))),
      ]));
    });

    return el('div', {}, [
      card('规则文档（' + target.model + ' · ' + (target.kind === 'sale' ? '对客售价' : '成本价') + '）', [
        el('div', { class: 'toolbar' }, [applyTemplate, simulate, ladder, save]),
        textarea,
        el('p', { class: 'muted', text: '倍数是推荐方式；这里用于固定价与促销窗口。切换为 absolute 后倍数字段不再生效。' }),
      ]),
      card('校验', [validation]),
      card('试算', [simulation]),
    ]);
  }

  tabMarkup.addEventListener('click', () => { mode = 'markup'; tabMarkup.className = 'btn btn-primary'; tabRules.className = 'btn'; renderBody(); });
  tabRules.addEventListener('click', () => { mode = 'rules'; tabRules.className = 'btn btn-primary'; tabMarkup.className = 'btn'; renderBody(); });
  refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
  search.addEventListener('input', renderBody);
  await load();
}

export { confirmDialog };
