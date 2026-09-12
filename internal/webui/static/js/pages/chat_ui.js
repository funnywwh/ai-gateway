// The console's half of the interactive-preview bridge.
//
// The page half is injected by the server (internal/httpapi/chat_ui_bridge.go). This module is
// the half that lives in the console: it performs the handshake, enforces the limits that keep
// one preview from turning into an unbounded bill, and applies the `ui` directive the model
// writes back into the page.
//
// Three properties are load-bearing.
//
//  1. The channel is a MessagePort, not a message listener. The model's document shares its
//     document with our injected script, so it can replace window.postMessage or post to the
//     parent itself. Once the port is handed over, neither is possible: the port is bound to
//     the frame's window at transfer time.
//  2. The handshake is authenticated structurally, not by a secret. The port we hand over can
//     only be received by the window that loaded the preview document, the injected script
//     refuses to start when it is not that window's top document, and this side accepts a hello
//     only from that exact frame. A secret was tried twice and removed twice: it cannot be read
//     out of a sandboxed frame, and the CSP exception needed to hide it breaks the page's own
//     inline scripts.
//  3. Nothing here writes HTML into the frame. The directive is applied as element
//     construction and property assignment, exactly like the console's own Markdown and chart
//     renderers, which never touch innerHTML either.

// Limits. They are the console's side of "one preview must not spend without bound": the page
// has its own byte cap, and the server bills whatever turn a submission produces, so an
// unbounded channel would be an unbounded bill.
export const UI_LIMITS = {
  minIntervalMs: 1500, // between two submissions from one page
  maxEvents: 40, // submissions per opened preview
  maxEventBytes: 8 * 1024, // one submission payload
  handshakeTimeoutMs: 3000, // how long to wait for the page to answer
};

// The operations applyUIOps knows. Anything else is reported, not guessed at.
const OPS = {
  text: 'text',
  set: 'set',
  class: 'class',
  style: 'style',
  show: 'show',
  hide: 'hide',
  remove: 'remove',
  focus: 'focus',
  disable: 'disable',
  message: 'message',
  svg: 'svg',
};

const MAX_OPS = 64;
const MAX_TEXT = 4096;
const MAX_SVG_DEPTH = 16;
const MAX_SVG_NODES = 512;

// SVG attributes the page-side builder accepts. Kept here as the authoritative list the
// harness asserts against; the page repeats it because it runs in another document.
const SVG_ATTRS = new Set([
  'x', 'y', 'x1', 'y1', 'x2', 'y2', 'cx', 'cy', 'r', 'rx', 'ry', 'width', 'height', 'd',
  'points', 'viewBox', 'fill', 'fill-opacity', 'fill-rule', 'stroke', 'stroke-width',
  'stroke-dasharray', 'stroke-linecap', 'stroke-linejoin', 'opacity', 'transform', 'font-size',
  'font-family', 'font-weight', 'text-anchor', 'dominant-baseline', 'dx', 'dy', 'class', 'id',
  'preserveAspectRatio',
]);

// parseUISpec reads one ```ui block. The shape is {"ops":[…]} (a bare array is accepted too,
// because a model that writes the obvious thing should not be punished for it).
export function parseUISpec(text) {
  const raw = String(text || '').trim();
  if (!raw) return { ops: [], error: '指令块是空的' };
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    return { ops: [], error: '不是合法 JSON：' + String(err && err.message ? err.message : err) };
  }
  const ops = Array.isArray(parsed) ? parsed : (parsed && Array.isArray(parsed.ops) ? parsed.ops : null);
  if (!ops) return { ops: [], error: 'JSON 里没有 ops 数组（应为 {"ops":[…]}）' };
  if (!ops.length) return { ops: [], error: 'ops 是空的，没有任何操作' };
  if (ops.length > MAX_OPS) {
    return { ops: ops.slice(0, MAX_OPS), error: `操作数超过 ${MAX_OPS} 条，只应用了前 ${MAX_OPS} 条` };
  }
  return { ops, error: '' };
}

// applyUIOps applies a parsed directive. It returns what it did and what it could not do —
// never a half-applied page pretending to be complete.
//
// Two separate knobs, because conflating them was a bug: `doc` is the document that *creates*
// nodes (a fixture tree must not be asked for createElement), and `root` is the subtree the
// selectors are resolved against. In the console both are the console document; the frame is
// patched through the port instead, by the page's own copy of this logic.
export function applyUIOps(ops, { doc, root } = {}) {
  // The variable names matter here: an earlier version called the document `target` and then
  // shadowed it with `const target = String(op.target)`, so every operation that creates a node
  // was handed a CSS selector instead of a document. Distinct names are the whole fix.
  const owner = doc || (typeof document !== 'undefined' ? document : null);
  const scope = root || owner;
  const applied = [];
  const errors = [];
  if (!owner || !scope) return { applied, errors: ['当前环境没有可操作的文档'] };
  for (const [index, raw] of (ops || []).entries()) {
    const op = raw && typeof raw === 'object' ? raw : null;
    if (!op) {
      errors.push(`第 ${index + 1} 条不是对象`);
      continue;
    }
    const kind = String(op.op || '');
    if (!OPS[kind]) {
      errors.push(`第 ${index + 1} 条：不支持的操作 ${kind || '(空)'}`);
      continue;
    }
    const selector = String(op.target || '');
    if (!selector) {
      errors.push(`第 ${index + 1} 条（${kind}）：缺少 target`);
      continue;
    }
    let nodes;
    try {
      nodes = scope.querySelectorAll(selector);
    } catch (err) {
      errors.push(`第 ${index + 1} 条（${kind}）：选择器 ${selector} 不合法`);
      continue;
    }
    if (!nodes.length) {
      errors.push(`第 ${index + 1} 条（${kind}）：没有节点匹配 ${selector}`);
      continue;
    }
    for (const node of nodes) {
      try {
        applyOne(owner, node, op, kind);
        applied.push({ op: kind, target: selector });
      } catch (err) {
        // The reason goes in the message: "应用到 #x 失败" on its own sent a whole debugging
        // round after the wrong layer, because the operation did run and one line inside it
        // failed.
        const why = err && err.message ? err.message : String(err);
        errors.push(`第 ${index + 1} 条（${kind}）：应用到 ${selector} 失败（${why}）`);
      }
    }
  }
  return { applied, errors };
}

function applyOne(doc, node, op, kind) {
  switch (kind) {
    case 'text':
      node.textContent = clip(op.value);
      return;
    case 'set':
      setValue(node, op);
      return;
    case 'class': {
      for (const name of asArray(op.add)) node.classList.add(String(name));
      for (const name of asArray(op.remove)) node.classList.remove(String(name));
      return;
    }
    case 'style': {
      const style = op.style && typeof op.style === 'object' ? op.style : {};
      for (const [key, value] of Object.entries(style)) {
        // setProperty with a made-up name throws, and a CSS value like "url(javascript:…)"
        // is inert through the CSSOM but rejected by the browser anyway: nothing here writes
        // a style *attribute*, so no string can escape into markup.
        node.style.setProperty(String(key), String(value));
      }
      return;
    }
    case 'show':
      node.hidden = false;
      node.style.display = '';
      return;
    case 'hide':
      node.hidden = true;
      node.style.display = 'none';
      return;
    case 'remove':
      if (node.parentNode) node.parentNode.removeChild(node);
      return;
    case 'focus':
      if (typeof node.focus === 'function') node.focus();
      return;
    case 'disable':
      setDisabled(node, op.value !== false);
      return;
    case 'message':
      showMessage(doc, node, op);
      return;
    case 'svg':
      setSVG(doc, node, op.svg);
      return;
    default:
      throw new Error('unsupported operation ' + kind);
  }
}

function setDisabled(node, disabled) {
  node.disabled = !!disabled;
  for (const field of node.querySelectorAll('input, select, textarea, button')) {
    field.disabled = !!disabled;
  }
}

function setValue(node, op) {
  if (op.checked !== undefined) {
    node.checked = !!op.checked;
    return;
  }
  const value = op.value;
  if (node.tagName === 'SELECT') {
    const wanted = asArray(value).map(String);
    for (const option of node.options) {
      option.selected = wanted.includes(String(option.value));
    }
    return;
  }
  if (node.type === 'checkbox' || node.type === 'radio') {
    node.checked = !!value;
    return;
  }
  node.value = value === undefined || value === null ? '' : String(value);
}

function showMessage(doc, node, op) {
  const existing = node.querySelector(':scope > .aigw-msg');
  if (existing) existing.remove();
  const box = doc.createElement('div');
  box.className = 'aigw-msg aigw-' + String(op.level || 'info');
  box.setAttribute('role', 'status');
  box.textContent = clip(op.value);
  node.append(box);
}

// setSVG builds a vector graphic node by node. Nothing is parsed: the tag is checked against a
// name pattern, attributes against an allow-list that also drops anything starting with "on",
// and text is set through textContent.
function setSVG(doc, node, spec) {
  while (node.firstChild) node.removeChild(node.firstChild);
  const budget = { nodes: 0 };
  const built = buildSVG(doc, spec, 0, budget);
  if (built) node.append(built);
}

function buildSVG(doc, spec, depth, budget) {
  if (!spec || typeof spec !== 'object' || depth > MAX_SVG_DEPTH) return null;
  if (budget.nodes >= MAX_SVG_NODES) return null;
  const tag = String(spec.tag || '');
  if (!/^[a-zA-Z][a-zA-Z0-9]*$/.test(tag)) return null;
  let node;
  try {
    node = doc.createElementNS('http://www.w3.org/2000/svg', tag);
  } catch (err) {
    return null;
  }
  budget.nodes += 1;
  const attrs = spec.attrs && typeof spec.attrs === 'object' ? spec.attrs : {};
  for (const [key, value] of Object.entries(attrs)) {
    if (!SVG_ATTRS.has(key)) continue;
    if (key.slice(0, 2).toLowerCase() === 'on') continue;
    try {
      node.setAttribute(key, String(value));
    } catch (err) { /* an attribute the browser rejects is simply not set */ }
  }
  if (spec.text !== undefined && spec.text !== null) node.textContent = clip(spec.text);
  for (const child of asArray(spec.children)) {
    const built = buildSVG(doc, child, depth + 1, budget);
    if (built) node.append(built);
  }
  return node;
}

function asArray(value) {
  if (value === undefined || value === null) return [];
  return Array.isArray(value) ? value : [value];
}

function clip(value) {
  const text = value === undefined || value === null ? '' : String(value);
  return text.length > MAX_TEXT ? text.slice(0, MAX_TEXT) + '…' : text;
}

// describeUIOps turns an apply result into the one line the console shows under the block.
export function describeUIResult(result) {
  if (!result) return '';
  const parts = [];
  if (result.applied.length) parts.push(`已应用 ${result.applied.length} 处更新`);
  if (result.errors.length) parts.push('部分未生效：' + result.errors.join('；'));
  return parts.join('；');
}

// ---------------------------------------------------------------------------
// the port
// ---------------------------------------------------------------------------

// createUIPort wires one open preview to the conversation.
//
// It returns a handle rather than closing over the modal, because the modal owns the toolbar
// and the chat page owns the turn: the port's job is the channel and the limits, nothing else.
export function createUIPort({
  sessionId, frame, limits, onEvent, onApply, onState, onError, onLog,
} = {}) {
  const cap = Object.assign({}, UI_LIMITS, limits || {});
  const state = { status: 'handshaking', events: 0, pending: 0, error: '', bridge: false };
  let channel = null;
  let timer = null;
  let lastSentAt = 0;
  let closed = false;

  function publish(patch) {
    Object.assign(state, patch);
    if (onState) onState(Object.assign({}, state));
  }

  function note(message) {
    publish({ error: message });
    if (onError) onError(message);
  }

  // The handshake arrives as a window message, which is the only unframed step. Three things
  // must hold before a port is accepted:
  //   - it came from this frame's window (a sibling frame or an opener cannot send as this one);
  //   - it says it is the top document of that frame (a nested frame on the page can post to us
  //     itself, and it must not be able to speak for the preview);
  //   - it actually carries a port (a message-shaped object without one is not our script).
  function onWindowMessage(ev) {
    if (closed) return;
    if (ev.source !== frame.contentWindow) return;
    const data = ev.data;
    if (!data || data.aigw !== 'ui' || data.t !== 'hello') return;
    if (data.framed === true) {
      if (onLog) onLog('ignored a hello from a nested frame');
      return;
    }
    const port = ev.ports && ev.ports[0];
    if (!port) {
      // A message-shaped object without a port is not our bridge; ignoring it is the whole
      // point of using a port rather than this channel.
      if (onLog) onLog('ignored a hello without a MessagePort');
      return;
    }
    adoptPort(port);
  }

  function adoptPort(port) {
    channel = port;
    channel.onmessage = (msg) => handleFrame(msg && msg.data);
    if (typeof channel.start === 'function') channel.start();
    if (timer) { clearTimeout(timer); timer = null; }
    publish({ status: 'ready', bridge: true, error: '' });
    // Ready is the first event the page sends, and it is what says "the injected script ran".
    // The console does not count it as a submission.
  }

  function handleFrame(frame) {
    if (closed || !frame) return;
    switch (frame.t) {
      case 'ev': {
        const name = String(frame.name || '');
        if (name === 'ready') {
          publish({ status: 'ready' });
          return;
        }
        let payload = {};
        const raw = typeof frame.v === 'string' ? frame.v : '{}';
        if (raw.length > cap.maxEventBytes) {
          note(`提交内容超过 ${cap.maxEventBytes} 字节上限，已拒绝`);
          return;
        }
        try {
          payload = JSON.parse(raw);
        } catch (err) {
          note('页面提交的数据不是合法 JSON，已拒绝');
          return;
        }
        deliver(name, payload, String(frame.label || ''));
        return;
      }
      case 'ui':
      case 'done':
        if (onApply) onApply(frame.ops || [], frame);
        return;
      case 'err':
        note(String(frame.m || '页面报告了一个错误'));
        return;
      default:
        return;
    }
  }

  // deliver is where the limits live. A rejection is always reported: a page whose button
  // silently does nothing is worse than one that says why.
  function deliver(name, payload, label) {
    if (!onEvent) return;
    const now = Date.now();
    if (state.events >= cap.maxEvents) {
      note(`这次预览已经提交了 ${cap.maxEvents} 次，达到上限；重新打开预览可以继续`);
      return;
    }
    if (now - lastSentAt < cap.minIntervalMs) {
      note(`提交过于频繁（两次之间至少间隔 ${Math.round(cap.minIntervalMs / 1000)} 秒）`);
      return;
    }
    lastSentAt = now;
    publish({ events: state.events + 1, error: '' });
    onEvent({ name, data: payload, label: label || defaultLabel(name) });
  }

  function defaultLabel(name) {
    if (name === 'submit') return '界面表单已提交';
    return '界面事件：' + name;
  }

  function sendToFrame(frame) {
    if (!channel) return false;
    channel.postMessage(frame);
    return true;
  }

  window.addEventListener('message', onWindowMessage);

  timer = setTimeout(() => {
    timer = null;
    if (state.status === 'handshaking') {
      publish({ status: 'blocked' });
      if (onError) {
        onError('页面的脚本没有回应：它可能被沙箱或页面自己的安全策略拦住了。预览仍可查看，但无法交互。');
      }
    }
  }, cap.handshakeTimeoutMs);

  return {
    state,
    // sendToFrame carries the console's own frames: deltas while the answer streams, the
    // applied directive, the busy/idle state.
    send: sendToFrame,
    // destroy is called when the modal closes or the page is left: the frame is going away, so
    // the port is dead, and a listener left behind would accept a hello from a later frame.
    destroy() {
      closed = true;
      if (timer) { clearTimeout(timer); timer = null; }
      window.removeEventListener('message', onWindowMessage);
      if (channel) {
        channel.onmessage = null;
        try { channel.close(); } catch (err) { /* already gone with the frame */ }
        channel = null;
      }
      publish({ status: 'closed' });
    },
  };
}
