// Charts for the console chat.
//
// A model that wants a chart writes a `chart` code block containing a JSON spec. This
// module validates that spec and draws it as inline SVG with DOM calls — no CDN, no
// eval, no model-authored code running in the console. That is a deliberate trade: the
// chart cannot be arbitrarily fancy, but it works offline, cannot execute anything, and can
// be exported (SVG/CSV) and asserted in tests.
//
// The two limits below are the contract with the server: internal/chat documents the same
// numbers in the prompt it gives the model, and internal/webui/embed_test.go fails if these
// constants drift from the Go ones.
export const MAX_SERIES = 8;
export const MAX_POINTS = 500;

const TYPES = ['bar', 'line', 'area', 'pie'];
const SVG_NS = 'http://www.w3.org/2000/svg';
const SERIES_COLORS = ['#4f8cff', '#41c99a', '#f2a63b', '#e4595c', '#9b6cf2', '#28b8d5', '#c9a227', '#7f8fa6'];

function svg(tag, attrs, children) {
  const node = document.createElementNS(SVG_NS, tag);
  if (attrs) {
    for (const [key, value] of Object.entries(attrs)) {
      if (value === undefined || value === null) continue;
      node.setAttribute(key, String(value));
    }
  }
  for (const child of [].concat(children || [])) {
    if (child === null || child === undefined) continue;
    node.append(child);
  }
  return node;
}

function isFiniteNumber(value) {
  return typeof value === 'number' && Number.isFinite(value);
}

// parseChart validates a spec. It returns { spec } or { error } — never a half-valid spec,
// because a chart drawn from the wrong numbers is worse than no chart at all.
export function parseChart(raw) {
  let spec;
  try {
    spec = JSON.parse(String(raw || ''));
  } catch (err) {
    return { error: '图表规格不是合法 JSON' };
  }
  if (!spec || typeof spec !== 'object') return { error: '图表规格必须是一个 JSON 对象' };
  if (!TYPES.includes(spec.type)) return { error: '图表类型必须是 bar / line / area / pie 之一' };
  const categories = Array.isArray(spec.categories) ? spec.categories.map((c) => String(c)) : [];
  if (!categories.length) return { error: '图表缺少 categories（横轴刻度）' };
  if (!Array.isArray(spec.series) || !spec.series.length) return { error: '图表缺少 series' };
  if (spec.series.length > MAX_SERIES) return { error: '最多 ' + MAX_SERIES + ' 条序列' };
  let points = 0;
  const series = [];
  for (const entry of spec.series) {
    if (!entry || typeof entry !== 'object' || !Array.isArray(entry.values)) {
      return { error: '每条序列都需要 values 数组' };
    }
    if (entry.values.length !== categories.length) {
      return { error: 'values 的长度必须与 categories 相同（' + categories.length + ' 个）' };
    }
    const values = entry.values.map((value) => (value === null ? null : Number(value)));
    for (const value of values) {
      if (value !== null && !isFiniteNumber(value)) return { error: '数值必须是有限数字或 null' };
    }
    points += values.length;
    series.push({ name: String(entry.name || ('序列 ' + (series.length + 1))), values });
  }
  if (points > MAX_POINTS) return { error: '最多 ' + MAX_POINTS + ' 个数据点，当前 ' + points + ' 个' };
  if (spec.type === 'pie') {
    // A pie chart of negatives or of an all-zero series cannot be drawn honestly.
    const values = series[0].values;
    if (values.some((value) => value !== null && value < 0)) return { error: '饼图不能包含负数' };
    const total = values.reduce((sum, value) => sum + (value || 0), 0);
    if (!(total > 0)) return { error: '饼图的所有数值都是 0，无法计算占比' };
  }
  const usable = series.some((entry) => entry.values.some((value) => isFiniteNumber(value)));
  if (!usable) return { error: '所有数据点都是 null，没有可绘制的数据' };
  return {
    spec: {
      type: spec.type,
      title: spec.title ? String(spec.title) : '',
      x_label: spec.x_label ? String(spec.x_label) : '',
      y_label: spec.y_label ? String(spec.y_label) : '',
      unit: spec.unit ? String(spec.unit) : '',
      categories,
      series,
      source: spec.source && typeof spec.source === 'object' ? spec.source : null,
    },
  };
}

// niceTicks picks 1/2/5 x 10^n steps so an axis reads in round numbers.
function niceTicks(min, max, count) {
  if (min === max) { max = min + 1; }
  const span = max - min;
  const rough = span / Math.max(count, 2);
  const magnitude = Math.pow(10, Math.floor(Math.log10(rough)));
  const normalized = rough / magnitude;
  let step = magnitude;
  if (normalized > 5) step = 10 * magnitude;
  else if (normalized > 2) step = 5 * magnitude;
  else if (normalized > 1) step = 2 * magnitude;
  const start = Math.floor(min / step) * step;
  const end = Math.ceil(max / step) * step;
  const ticks = [];
  for (let value = start; value <= end + step / 2; value += step) {
    ticks.push(Math.round(value * 1e6) / 1e6);
  }
  return ticks;
}

function formatNumber(value, unit) {
  const rounded = Math.abs(value) >= 100 ? Math.round(value) : Math.round(value * 100) / 100;
  return String(rounded) + (unit ? ' ' + unit : '');
}

// buildChartSvg lays the chart out. Width/height are viewBox units; CSS scales it.
function buildChartSvg(spec) {
  const width = 720;
  const height = 320;
  const pad = { top: 28, right: 24, bottom: 56, left: 64 };
  const plotWidth = width - pad.left - pad.right;
  const plotHeight = height - pad.top - pad.bottom;
  const root = svg('svg', {
    viewBox: '0 0 ' + width + ' ' + height, class: 'chart-svg', role: 'img',
    'aria-label': spec.title || '图表',
  });
  root.append(svg('rect', { x: 0, y: 0, width, height, fill: 'transparent' }));

  if (spec.type === 'pie') {
    const values = spec.series[0].values.map((value) => value || 0);
    const total = values.reduce((sum, value) => sum + value, 0);
    const cx = pad.left + plotWidth / 2;
    const cy = pad.top + plotHeight / 2;
    const radius = Math.min(plotWidth, plotHeight) / 2 - 8;
    let angle = -Math.PI / 2;
    values.forEach((value, index) => {
      const slice = (value / total) * Math.PI * 2;
      const end = angle + slice;
      const large = slice > Math.PI ? 1 : 0;
      const x1 = cx + radius * Math.cos(angle);
      const y1 = cy + radius * Math.sin(angle);
      const x2 = cx + radius * Math.cos(end);
      const y2 = cy + radius * Math.sin(end);
      const path = svg('path', {
        d: ['M', cx, cy, 'L', x1, y1, 'A', radius, radius, 0, large, 1, x2, y2, 'Z'].join(' '),
        fill: SERIES_COLORS[index % SERIES_COLORS.length], class: 'chart-slice',
      });
      path.append(svg('title', {}, [document.createTextNode(spec.categories[index] + '：' + formatNumber(value, spec.unit))]));
      root.append(path);
      angle = end;
    });
    return { root, legend: values.map((value, index) => ({
      name: spec.categories[index], color: SERIES_COLORS[index % SERIES_COLORS.length],
      value: formatNumber(value, spec.unit),
    })) };
  }

  const values = [];
  for (const entry of spec.series) {
    for (const value of entry.values) if (isFiniteNumber(value)) values.push(value);
  }
  let min = Math.min(...values);
  let max = Math.max(...values);
  // A bar chart is read against zero, so its baseline is always zero.
  if (spec.type === 'bar' || spec.type === 'area') min = Math.min(min, 0);
  const ticks = niceTicks(min, max, 5);
  const yMin = ticks[0];
  const yMax = ticks[ticks.length - 1];
  const scaleY = (value) => pad.top + plotHeight - ((value - yMin) / (yMax - yMin || 1)) * plotHeight;
  const stepX = plotWidth / spec.categories.length;
  const scaleX = (index) => pad.left + stepX * (index + 0.5);

  for (const tick of ticks) {
    const y = scaleY(tick);
    root.append(svg('line', { x1: pad.left, y1: y, x2: pad.left + plotWidth, y2: y, class: 'chart-grid' }));
    const label = svg('text', { x: pad.left - 8, y: y + 4, 'text-anchor': 'end', class: 'chart-axis' });
    label.append(document.createTextNode(formatNumber(tick, spec.unit)));
    root.append(label);
  }
  spec.categories.forEach((category, index) => {
    const label = svg('text', { x: scaleX(index), y: pad.top + plotHeight + 18, 'text-anchor': 'middle', class: 'chart-axis' });
    label.append(document.createTextNode(category.length > 12 ? category.slice(0, 11) + '…' : category));
    root.append(label);
    const title = svg('title', {}, [document.createTextNode(category)]);
    label.append(title);
  });

  if (spec.type === 'bar') {
    const perSeries = plotWidth / spec.categories.length / spec.series.length;
    spec.series.forEach((entry, seriesIndex) => {
      entry.values.forEach((value, index) => {
        if (!isFiniteNumber(value)) return;
        const zero = scaleY(Math.max(yMin, 0));
        const y = scaleY(value);
        const barWidth = Math.max(perSeries * 0.7, 2);
        const x = pad.left + stepX * index + perSeries * seriesIndex + (stepX - perSeries * spec.series.length) / 2;
        const rect = svg('rect', {
          x, y: Math.min(y, zero), width: barWidth, height: Math.max(Math.abs(zero - y), 1),
          fill: SERIES_COLORS[seriesIndex % SERIES_COLORS.length], rx: 2,
        });
        rect.append(svg('title', {}, [document.createTextNode(entry.name + ' · ' + spec.categories[index] + '：' + formatNumber(value, spec.unit))]));
        root.append(rect);
      });
    });
  } else {
    spec.series.forEach((entry, seriesIndex) => {
      const color = SERIES_COLORS[seriesIndex % SERIES_COLORS.length];
      let path = '';
      let previous = null;
      entry.values.forEach((value, index) => {
        if (!isFiniteNumber(value)) { previous = null; return; }
        const x = scaleX(index);
        const y = scaleY(value);
        if (previous === null) path += 'M ' + x + ' ' + y;
        else path += ' L ' + x + ' ' + y;
        previous = { x, y };
      });
      if (spec.type === 'area' && path) {
        const firstX = scaleX(0);
        const lastX = scaleX(entry.values.length - 1);
        const baseline = scaleY(Math.max(yMin, 0));
        root.append(svg('path', {
          d: path + ' L ' + lastX + ' ' + baseline + ' L ' + firstX + ' ' + baseline + ' Z',
          fill: color, opacity: 0.18,
        }));
      }
      if (path) root.append(svg('path', { d: path, fill: 'none', stroke: color, 'stroke-width': 2 }));
      entry.values.forEach((value, index) => {
        if (!isFiniteNumber(value)) return;
        const dot = svg('circle', { cx: scaleX(index), cy: scaleY(value), r: 2.5, fill: color });
        dot.append(svg('title', {}, [document.createTextNode(entry.name + ' · ' + spec.categories[index] + '：' + formatNumber(value, spec.unit))]));
        root.append(dot);
      });
    });
  }

  const legend = spec.series.map((entry, index) => ({
    name: entry.name, color: SERIES_COLORS[index % SERIES_COLORS.length], value: '',
  }));
  return { root, legend };
}

// renderChart returns a rendered chart element, or null when the spec cannot be drawn
// honestly (the caller then keeps showing the source, with the reason).
export function renderChart(raw, options) {
  const parsed = parseChart(raw);
  if (parsed.error) return { error: parsed.error };
  const spec = parsed.spec;
  const opts = options || {};
  let svgRoot;
  let legend;
  try {
    const built = buildChartSvg(spec);
    svgRoot = built.root;
    legend = built.legend;
  } catch (err) {
    return { error: '图表绘制失败：' + (err && err.message ? err.message : String(err)) };
  }

  const host = document.createElement('div');
  host.className = 'chart';
  if (spec.title) {
    const title = document.createElement('div');
    title.className = 'chart-title';
    title.textContent = spec.title;
    host.append(title);
  }
  host.append(svgRoot);

  if (legend && legend.length > 1) {
    const list = document.createElement('div');
    list.className = 'chart-legend';
    for (const entry of legend) {
      const item = document.createElement('span');
      item.className = 'chart-legend-item';
      const swatch = document.createElement('span');
      swatch.className = 'chart-swatch';
      swatch.style.background = entry.color;
      item.append(swatch, document.createTextNode(entry.name + (entry.value ? ' ' + entry.value : '')));
      list.append(item);
    }
    host.append(list);
  }

  // The data table is not decoration: the console cannot verify that the numbers the model
  // drew came from the tools it called, so the table (and the source note) is how a person
  // checks them.
  const details = document.createElement('details');
  details.className = 'chart-data';
  const summary = document.createElement('summary');
  summary.textContent = '数据表（共 ' + spec.categories.length + ' 项）';
  details.append(summary);
  const table = document.createElement('table');
  table.className = 'md-table';
  const head = document.createElement('tr');
  head.append(cell('th', spec.x_label || '分类'));
  for (const entry of spec.series) head.append(cell('th', entry.name));
  table.append(head);
  spec.categories.forEach((category, index) => {
    const row = document.createElement('tr');
    row.append(cell('td', category));
    for (const entry of spec.series) {
      const value = entry.values[index];
      row.append(cell('td', value === null ? '—' : formatNumber(value, spec.unit)));
    }
    table.append(row);
  });
  details.append(table);
  host.append(details);

  const source = document.createElement('div');
  source.className = 'chart-source';
  const parts = [];
  if (spec.source) {
    if (spec.source.tool) parts.push('来源工具：' + spec.source.tool);
    if (spec.source.request_id) parts.push('请求 id：' + spec.source.request_id);
    if (spec.source.note) parts.push(spec.source.note);
  }
  if (opts.toolCalls && opts.toolCalls.length) {
    parts.push('本轮实际调用：' + opts.toolCalls.join(', '));
  } else {
    parts.push('本轮没有匹配到工具调用，数值未经核对');
  }
  source.textContent = parts.join(' · ');
  host.append(source);
  return { element: host, spec };
}

function cell(tag, text) {
  const node = document.createElement(tag);
  node.textContent = text;
  return node;
}

// ---------------------------------------------------------------------------
// exports
// ---------------------------------------------------------------------------

// chartToCSV renders the series as CSV. Values that look like formulas are prefixed so a
// spreadsheet cannot execute them: the numbers come from a model's answer.
export function chartToCSV(spec) {
  if (!spec) return '';
  const escape = (value) => {
    let text = String(value === null || value === undefined ? '' : value);
    if (/^[=+\-@\t\r]/.test(text)) text = "'" + text;
    if (/[",\n]/.test(text)) text = '"' + text.replace(/"/g, '""') + '"';
    return text;
  };
  const rows = [[escape(spec.x_label || 'category')].concat(spec.series.map((entry) => escape(entry.name))).join(',')];
  spec.categories.forEach((category, index) => {
    rows.push([escape(category)].concat(spec.series.map((entry) => escape(entry.values[index]))).join(','));
  });
  return rows.join('\n') + '\n';
}

// chartToSVG serializes the drawn SVG, so what is exported is exactly what was displayed.
export function chartToSVG(spec) {
  const built = buildChartSvg(spec);
  const clone = built.root.cloneNode(true);
  clone.setAttribute('xmlns', SVG_NS);
  clone.setAttribute('width', '720');
  clone.setAttribute('height', '320');
  return '<?xml version="1.0" encoding="UTF-8"?>\n' + new XMLSerializer().serializeToString(clone);
}

// downloadText saves one export through a Blob URL. The console's CSP forbids external
// resources but a blob: navigation initiated by a click is not a resource load.
export function downloadText(filename, text, mime) {
  const blob = new Blob([text], { type: mime || 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  link.style.display = 'none';
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
