// Inline declarative forms (M35).
//
// The model asks for information by emitting a ```form block: a JSON spec of a title, some
// fields and a submit button. The console *builds* that form out of `createElement` calls in
// the transcript bubble itself — no iframe, no preview, no sandbox, no injected script, no
// channel. Everything here is a pure function of the spec text plus a root element, so the
// same call renders the form on arrival and re-renders it from the stored transcript after a
// reload: the form is not state that has to be remembered, it is a rendering of the message.
//
// Why a spec rather than model-authored HTML: allowing arbitrary HTML would mean allowing the
// model's markup into the console's own document, and owning a sanitizer at the one place in
// this console that has never once written markup (see the innerHTML assertion in
// internal/webui/embed_test.go). A spec the console renders keeps that property intact: nothing
// in this file can turn a string into markup, and the model's text is only ever assigned to
// textContent.
//
// What this cannot do, and does not pretend to: no free-form layout, no page scripts, no
// canvas. A model that needs those still emits an ```html block, which keeps using the
// sandboxed preview. This module freezes neither path for the other.

import { el } from '../ui.js';
import { applyUIOps } from './chat_ui.js';

// Limits. A form is a rendering of a message, so it must not be able to turn one answer into
// an unbounded amount of DOM or an unbounded submission.
export const FORM_LIMITS = {
  maxFields: 40,
  maxOptions: 100,
  maxLabelChars: 200,
  maxNoteChars: 4000,
  maxValueChars: 2000,
  maxEventBytes: 8 * 1024,
};

// Field types the console can build honestly. Anything else is a parse error rather than a
// silent downgrade: a form that renders as something other than what the model asked for is
// worse than one that says why it could not be built.
//
// `password` and `file` are absent on purpose, and saying so beats letting them fall through to
// "unsupported type". The credential rule is older than this module (docs/chat.md §4: a form
// value becomes a normal question in the transcript, so a form is never the place for a
// secret), and a file input cannot be serialized at all — the page-side bridge drops it for the
// same reason.
const FIELD_TYPES = [
  'text', 'textarea', 'number', 'select', 'radio', 'checkbox', 'date', 'note',
];
const REFUSED_TYPES = {
  // The credential rule is older than this module (docs/chat.md §4: a form value becomes a
  // normal question in the transcript, so a form is never the place for a secret). A password
  // field would invite exactly that, so the model is told to use the confirmation flow instead
  // and a spec that asks for one is rejected with that reason rather than silently downgraded
  // to a visible text input.
  password: '表单值会进入会话转录，不要用表单收集凭据',
  file: '文件无法随提交传送，请让用户先上传到别处再填路径',
};

// Names that must never become object keys on the way back to the model. Submissions are
// aggregated into a plain object, so these are dropped exactly like the page-side bridge drops
// them, for the same reason.
const DANGEROUS = /^(__proto__|constructor|prototype)$/;

function isPlainObject(value) {
  return !!value && typeof value === 'object' && !Array.isArray(value);
}

// clip bounds one string. Every piece of model-authored text passes through here: the value is
// never interpreted, only shortened, and the caller always assigns it to textContent.
function clip(value, limit) {
  const text = value === undefined || value === null ? '' : String(value);
  return text.length > limit ? text.slice(0, limit) + '…' : text;
}

function asText(value, limit) {
  return clip(value, limit);
}

// parseFormSpec reads one ```form block.
//
// It returns { spec, error }. On error the spec is null and the caller shows the raw block plus
// the reason, which is the same contract the chart renderer and the `ui` directive follow: a
// spec the console cannot honour is reported, never half-drawn.
//
// `key` names this form's root element (`#form_<key>`), which is what makes the `ui` directive
// able to target it. The caller passes something stable for the message the block came from —
// the block index, for instance — so the same message always renders the same id.
export function parseFormSpec(text, key) {
  const raw = String(text || '').trim();
  if (!raw) return { spec: null, error: '表单规格是空的' };
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    return { spec: null, error: '不是合法 JSON：' + String(err && err.message ? err.message : err) };
  }
  if (!isPlainObject(parsed)) return { spec: null, error: '规格必须是一个 JSON 对象' };

  const form = {
    id: String(key === undefined || key === null ? 'f' : key),
    title: asText(parsed.title, FORM_LIMITS.maxLabelChars),
    description: asText(parsed.description, FORM_LIMITS.maxNoteChars),
    fields: [],
    submit: { name: 'submit', label: '提交' },
    actions: [],
  };

  const rawFields = Array.isArray(parsed.fields) ? parsed.fields : null;
  if (!rawFields) return { spec: null, error: '规格里缺少 fields 数组' };
  if (!rawFields.length) return { spec: null, error: 'fields 是空的，没有任何字段' };
  if (rawFields.length > FORM_LIMITS.maxFields) {
    return { spec: null, error: `字段数超过上限 ${FORM_LIMITS.maxFields}` };
  }

  const seen = new Set();
  for (const [index, rawField] of rawFields.entries()) {
    const where = `第 ${index + 1} 个字段`;
    if (!isPlainObject(rawField)) return { spec: null, error: where + '不是一个对象' };
    const type = String(rawField.type || 'text').toLowerCase();
    if (REFUSED_TYPES[type]) {
      return { spec: null, error: `${where}的类型 ${type} 被拒绝：${REFUSED_TYPES[type]}` };
    }
    if (FIELD_TYPES.indexOf(type) < 0) {
      return { spec: null, error: `${where}的类型 ${type || '(空)'} 不支持（可用：${FIELD_TYPES.join(' / ')}）` };
    }
    const field = {
      type,
      name: '',
      label: asText(rawField.label, FORM_LIMITS.maxLabelChars),
      help: asText(rawField.help, FORM_LIMITS.maxNoteChars),
      required: rawField.required === true,
      value: '',
      options: [],
      placeholder: asText(rawField.placeholder, FORM_LIMITS.maxLabelChars),
      min: null,
      max: null,
      step: null,
      rows: 4,
      multiple: false,
    };

    if (type === 'note') {
      // A note carries no value, so it needs no name: it is how the model explains something
      // inside the form instead of in the prose around it.
      field.label = asText(rawField.label || rawField.text, FORM_LIMITS.maxNoteChars);
      if (!field.label) return { spec: null, error: where + '（note）缺少 label' };
      form.fields.push(field);
      continue;
    }

    field.name = String(rawField.name || '').trim();
    if (!field.name) return { spec: null, error: `${where}缺少 name（没有 name 的控件不会进入提交数据）` };
    if (DANGEROUS.test(field.name)) return { spec: null, error: `${where}的 name 不允许使用 ${field.name}` };
    if (seen.has(field.name)) return { spec: null, error: `字段 name 重复：${field.name}` };
    seen.add(field.name);
    if (!field.label) return { spec: null, error: `字段 ${field.name} 缺少 label` };

    if (type === 'select' || type === 'radio') {
      const rawOptions = Array.isArray(rawField.options) ? rawField.options : null;
      if (!rawOptions || !rawOptions.length) {
        return { spec: null, error: `字段 ${field.name}（${type}）缺少 options` };
      }
      if (rawOptions.length > FORM_LIMITS.maxOptions) {
        return { spec: null, error: `字段 ${field.name} 的选项超过上限 ${FORM_LIMITS.maxOptions}` };
      }
      const values = new Set();
      for (const rawOption of rawOptions) {
        let value;
        let label;
        if (isPlainObject(rawOption)) {
          value = String(rawOption.value === undefined ? '' : rawOption.value);
          label = asText(rawOption.label === undefined ? value : rawOption.label, FORM_LIMITS.maxLabelChars);
        } else {
          value = String(rawOption);
          label = asText(rawOption, FORM_LIMITS.maxLabelChars);
        }
        if (!value) return { spec: null, error: `字段 ${field.name} 有选项缺少 value` };
        if (values.has(value)) return { spec: null, error: `字段 ${field.name} 的选项 value 重复：${value}` };
        values.add(value);
        field.options.push({ value, label: label || value });
      }
      if (type === 'select') {
        field.multiple = rawField.multiple === true;
        field.value = rawField.value === undefined || rawField.value === null ? '' : String(rawField.value);
        if (field.value && !values.has(field.value)) {
          return { spec: null, error: `字段 ${field.name} 的默认值 ${field.value} 不在 options 里` };
        }
      } else {
        field.value = rawField.value === undefined || rawField.value === null ? '' : String(rawField.value);
        if (field.value && !values.has(field.value)) {
          return { spec: null, error: `字段 ${field.name} 的默认值 ${field.value} 不在 options 里` };
        }
      }
      form.fields.push(field);
      continue;
    }

    if (type === 'number') {
      const min = rawField.min === undefined || rawField.min === null ? null : Number(rawField.min);
      const max = rawField.max === undefined || rawField.max === null ? null : Number(rawField.max);
      const step = rawField.step === undefined || rawField.step === null ? null : Number(rawField.step);
      if (min !== null && !Number.isFinite(min)) return { spec: null, error: `字段 ${field.name} 的 min 不是数字` };
      if (max !== null && !Number.isFinite(max)) return { spec: null, error: `字段 ${field.name} 的 max 不是数字` };
      if (step !== null && !Number.isFinite(step)) return { spec: null, error: `字段 ${field.name} 的 step 不是数字` };
      if (min !== null && max !== null && min > max) {
        return { spec: null, error: `字段 ${field.name} 的 min 大于 max` };
      }
      field.min = min;
      field.max = max;
      field.step = step;
      field.value = rawField.value === undefined || rawField.value === null ? '' : String(rawField.value);
      form.fields.push(field);
      continue;
    }

    if (type === 'checkbox') {
      field.checked = rawField.value === true || rawField.checked === true;
      form.fields.push(field);
      continue;
    }

    if (type === 'textarea') {
      const rows = Number(rawField.rows);
      field.rows = Number.isFinite(rows) && rows >= 2 && rows <= 20 ? Math.floor(rows) : 4;
    }

    if (type === 'date') {
      field.value = rawField.value === undefined || rawField.value === null ? '' : String(rawField.value);
      if (field.value && !/^\d{4}-\d{2}-\d{2}$/.test(field.value)) {
        return { spec: null, error: `字段 ${field.name} 的默认值不是 YYYY-MM-DD` };
      }
      form.fields.push(field);
      continue;
    }

    // text / textarea
    const value = rawField.value === undefined || rawField.value === null ? '' : String(rawField.value);
    if (value.length > FORM_LIMITS.maxValueChars) {
      return { spec: null, error: `字段 ${field.name} 的默认值超过 ${FORM_LIMITS.maxValueChars} 字符` };
    }
    field.value = value;
    form.fields.push(field);
  }

  if (isPlainObject(parsed.submit)) {
    const name = String(parsed.submit.name || '').trim();
    const label = asText(parsed.submit.label, FORM_LIMITS.maxLabelChars);
    if (name && DANGEROUS.test(name)) return { spec: null, error: 'submit.name 不允许使用 ' + name };
    form.submit = { name: name || 'submit', label: label || '提交' };
  }

  const rawActions = Array.isArray(parsed.actions) ? parsed.actions : [];
  if (rawActions.length > 6) return { spec: null, error: 'actions 超过 6 个' };
  for (const [index, rawAction] of rawActions.entries()) {
    if (!isPlainObject(rawAction)) return { spec: null, error: `第 ${index + 1} 个 action 不是一个对象` };
    const name = String(rawAction.name || '').trim();
    if (!name) return { spec: null, error: `第 ${index + 1} 个 action 缺少 name` };
    if (DANGEROUS.test(name)) return { spec: null, error: `action name 不允许使用 ${name}` };
    if (name === form.submit.name) return { spec: null, error: `action 与 submit 同名：${name}` };
    const patch = rawAction.value;
    if (patch !== undefined && !isPlainObject(patch)) {
      return { spec: null, error: `action ${name} 的 value 必须是对象` };
    }
    form.actions.push({
      name,
      label: asText(rawAction.label, FORM_LIMITS.maxLabelChars) || name,
      value: patch || null,
    });
  }

  return { spec: form, error: '' };
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// fieldControl builds the input for one field. Every branch ends in `createElement` or the
// `el` helper: no branch ever receives HTML.
function fieldControl(field, registry) {
  let control = null;
  switch (field.type) {
    case 'textarea': {
      control = el('textarea', { class: 'form-input', id: 'f_' + field.name, rows: String(field.rows) });
      control.value = field.value;
      break;
    }
    case 'select': {
      control = el('select', { class: 'form-input', id: 'f_' + field.name });
      if (!field.required && !field.multiple) control.append(el('option', { value: '', text: '（不选）' }));
      for (const option of field.options) {
        const node = el('option', { value: option.value, text: option.label });
        if (String(option.value) === String(field.value)) node.selected = true;
        control.append(node);
      }
      if (field.multiple) control.multiple = true;
      break;
    }
    case 'radio': {
      control = el('div', { class: 'form-radio-group', id: 'f_' + field.name });
      for (const option of field.options) {
        const input = el('input', { type: 'radio', name: 'r_' + field.name, value: option.value });
        if (String(option.value) === String(field.value)) input.checked = true;
        const item = el('label', { class: 'form-radio' }, [input, el('span', { text: option.label })]);
        control.append(item);
        registry.controls.push({ field, node: input });
      }
      break;
    }
    case 'checkbox': {
      control = el('input', { class: 'form-check', type: 'checkbox', id: 'f_' + field.name });
      control.checked = field.checked === true;
      break;
    }
    case 'number': {
      control = el('input', { class: 'form-input', type: 'number', id: 'f_' + field.name });
      control.value = field.value;
      if (field.min !== null) control.min = String(field.min);
      if (field.max !== null) control.max = String(field.max);
      if (field.step !== null) control.step = String(field.step);
      break;
    }
    case 'date': {
      control = el('input', { class: 'form-input', type: 'date', id: 'f_' + field.name });
      control.value = field.value;
      break;
    }
    default: {
      control = el('input', { class: 'form-input', type: 'text', id: 'f_' + field.name });
      control.value = field.value;
      break;
    }
  }
  if (field.placeholder) control.placeholder = field.placeholder;
  registry.controls.push({ field, node: control });
  return control;
}

// renderForm builds the form into a fresh detached subtree. Returning a detached node (rather
// than appending) is what lets a caller replace it atomically — and lets the tests assert on it
// without a live document.
export function renderForm(spec, { onSubmit, onAction, onState } = {}) {
  const registry = { controls: [], buttons: [] };
  const status = el('div', { class: 'form-status muted' });
  const actions = el('div', { class: 'form-actions' });
  // The root carries the id the spec was parsed with, so the `ui` directive can target the form
  // itself (`#form_0`) as well as its fields (`#f_name`) and its buttons (`#b_submit`). Both the
  // transcript and the directive compute the same id from the same block, so no state has to be
  // carried between them.
  const root = el('div', { class: 'chat-form', id: 'form_' + spec.id }, []);
  const body = el('div', { class: 'form-body' });
  root.append(body);

  if (spec.title) body.append(el('h4', { class: 'form-title', text: spec.title }));
  if (spec.description) body.append(el('p', { class: 'form-desc', text: spec.description }));

  for (const field of spec.fields) {
    if (field.type === 'note') {
      body.append(el('p', { class: 'form-note', text: field.label }));
      continue;
    }
    const control = fieldControl(field, registry);
    const row = el('div', { class: 'form-row' + (field.type === 'checkbox' ? ' form-row-check' : '') });
    const label = el('label', { class: 'form-label', for: 'f_' + field.name });
    label.append(el('span', { class: 'form-label-text', text: field.label }));
    if (field.required) label.append(el('span', { class: 'form-req', text: '必填' }));
    if (field.type === 'checkbox') {
      // A checkbox reads better with the control first, but the label must still be the one
      // that names it, so the association stays for screen readers.
      row.append(control, label);
    } else {
      row.append(label, control);
    }
    if (field.help) row.append(el('div', { class: 'form-help muted', text: field.help }));
    body.append(row);
  }

  // Buttons carry `#b_<name>` for the same reason the root carries `#form_<key>`: the console builds
  // these elements, so the model can only address one if the console names it. Without that name a
  // directive that wants to disable a button has nothing to point at — the model reached for
  // `button[type=submit]`, which is the contract for its *own* HTML pages and never matches here
  // (these buttons are type="button"; a real `<form>` submit would reload the console). See
  // docs/chat.md §4.
  const submit = el('button', {
    class: 'btn btn-primary', type: 'button', id: 'b_' + spec.submit.name, text: spec.submit.label,
  });
  submit.addEventListener('click', () => {
    if (registry.disabled) return;
    fire(spec.submit.name, null, submit);
  });
  actions.append(submit);
  registry.buttons.push({ name: spec.submit.name, node: submit });

  for (const action of spec.actions) {
    const button = el('button', {
      class: 'btn btn-ghost', type: 'button', id: 'b_' + action.name, text: action.label,
    });
    button.addEventListener('click', () => {
      if (registry.disabled) return;
      fire(action.name, action.value, button);
    });
    actions.append(button);
    registry.buttons.push({ name: action.name, node: button });
  }

  root.append(actions, status);

  // fire is the only place a submission leaves this form. It reports `settled` so the caller
  // can decide whether to disable the form: an inline form is inside a turn, so submitting
  // usually means "a new turn starts now", and clicking twice must not spend twice.
  function fire(name, patch, button) {
    const data = collectFormValues(spec, registry);
    if (patch) {
      for (const [key, value] of Object.entries(patch)) {
        if (DANGEROUS.test(key)) continue;
        data[key] = value;
      }
    }
    const size = JSON.stringify(data).length;
    if (size > FORM_LIMITS.maxEventBytes) {
      setStatus(`这条提交有 ${size} 字节，超过 ${FORM_LIMITS.maxEventBytes} 字节上限：请减少内容`, 'error');
      return;
    }
    if (!onSubmit) return;
    onSubmit({
      name,
      label: spec.title || (name === spec.submit.name ? '表单已提交' : '表单操作：' + name),
      data,
      button,
      spec,
    });
  }

  function setStatus(text, level) {
    status.textContent = String(text || '');
    status.className = 'form-status' + (level ? ' ' + level : ' muted');
    if (onState) onState({ busy: registry.disabled, text: status.textContent });
  }

  return {
    node: root,
    spec,
    registry,
    // setBusy is the console's half of "this form is inside a turn": while a turn runs the
    // form is disabled, so a second click cannot start a second billed request.
    setBusy(busy, text) {
      registry.disabled = !!busy;
      for (const { node } of registry.buttons) node.disabled = !!busy;
      for (const { node } of registry.controls) node.disabled = !!busy;
      setStatus(text || (busy ? '模型正在处理…' : ''), busy ? '' : 'muted');
    },
    setStatus,
    collect: () => collectFormValues(spec, registry),
    // apply is the `ui` directive, applied to this form's own subtree with the console's
    // existing whitelist. The directive was written for a sandboxed page; an inline form needs
    // exactly the same operations, so it reuses the same implementation rather than growing a
    // second one that could drift.
    //
    // The resolver is the one difference, and it is a real one: the form's root is an element,
    // and `root.querySelectorAll('#form_0')` can never match it — only descendants are searched.
    // A directive that targets the form itself is the natural way to put a message on it, so the
    // root is checked first and the subtree search follows.
    apply(ops) {
      const result = applyUIOps(ops || [], {
        doc: document, root, resolve: (selector) => resolveIn(root, selector),
      });
      if (result.errors && result.errors.length) {
        setStatus('部分更新未生效：' + result.errors.join('；'), 'error');
      }
      return result;
    },
    destroy() {
      root.remove();
    },
  };
}

// resolveIn resolves one directive selector against a form: the root itself first, then its
// subtree. Exported because it is the piece of the `ui` contract that an inline form changes,
// and a test can pin it without a document full of forms.
export function resolveIn(root, selector) {
  const found = [];
  if (root.matches && root.matches(selector)) found.push(root);
  for (const node of root.querySelectorAll(selector)) {
    if (node !== root) found.push(node);
  }
  return found;
}

// collectFormValues reads the current values out of a rendered form, in the shape a native form
// would have submitted: one key per named field, numbers as numbers, checkboxes as booleans,
// radio groups and multi-selects as their chosen value(s).
export function collectFormValues(spec, registry) {
  const out = {};
  for (const { field, node } of registry.controls) {
    if (DANGEROUS.test(field.name)) continue;
    if (field.type === 'radio') {
      if (node.checked) out[field.name] = String(node.value);
      continue;
    }
    if (field.type === 'checkbox') {
      out[field.name] = !!node.checked;
      continue;
    }
    if (field.type === 'select' && field.multiple) {
      const values = [];
      for (const option of node.options) if (option.selected) values.push(String(option.value));
      out[field.name] = values;
      continue;
    }
    if (field.type === 'number') {
      const raw = String(node.value || '').trim();
      // An untouched or cleared number stays absent rather than becoming 0 or NaN: the model
      // must be able to tell "not filled in" from "zero".
      if (raw === '') continue;
      const num = Number(raw);
      out[field.name] = Number.isFinite(num) ? num : raw;
      continue;
    }
    const value = String(node.value === undefined || node.value === null ? '' : node.value);
    // An optional field the operator left alone is omitted, which is what a browser does with
    // an empty input only for checkboxes — doing it here keeps "not answered" unambiguous.
    if (value === '' && !field.required) continue;
    out[field.name] = value.length > FORM_LIMITS.maxValueChars ? value.slice(0, FORM_LIMITS.maxValueChars) : value;
  }
  return out;
}

// describeUIResult turns an apply result into the one line the transcript shows under the
// block, matching what the preview toolbar says for the same directive.
export function describeFormOps(result) {
  if (!result) return '';
  const parts = [];
  if (result.applied && result.applied.length) parts.push(`已应用 ${result.applied.length} 处更新`);
  if (result.errors && result.errors.length) parts.push('部分未生效：' + result.errors.join('；'));
  return parts.join('；');
}
