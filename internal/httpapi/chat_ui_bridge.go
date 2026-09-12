package httpapi

import (
	"strconv"
	"strings"
)

// The interactive preview bridge.
//
// M32 gave a model-authored HTML page a sandboxed iframe to run in. M34 lets that page talk
// back: the model builds a form, the operator fills it in, and the submission becomes a
// normal question in the same conversation.
//
// Three constraints shape everything below.
//
//  1. The page runs in a sandbox without allow-same-origin and without allow-forms, so it has
//     no cookie, no parent DOM, and no native form submission. Whatever channel exists has to
//     be built out of postMessage and JavaScript interception.
//  2. The page and the injected script share one document, so the page can rewrite
//     `window.postMessage` and can post to the parent itself. A bare postMessage channel is
//     therefore both hijackable and spoofable. The handshake therefore hands out a MessagePort,
//     which is bound to the receiving window at transfer time and cannot be reached through any
//     global afterwards — and the script captures `postMessage` before any page code runs.
//  3. The document's own Content-Security-Policy must keep allowing the page's inline scripts:
//     these pages are model-authored HTML whose whole point is inline CSS and JS, so a policy
//     that blocks inline script breaks the feature it was meant to protect. That rules out
//     authorizing our injected script with a nonce or a hash — either one makes browsers ignore
//     `'unsafe-inline'`, which would take the page's own scripts and event handlers down with it.
//
// So the injected script is authorized by `'unsafe-inline'` like the page's own code, and the
// channel is authenticated structurally instead of by a secret:
//
//   * the offered port reaches exactly one window — the one that loaded this document — so only
//     the page itself could ever hold it;
//   * the injected script refuses to start when it is not the top document (`window.top` check),
//     which is what stops a nested frame on the page from being the one that gets a port;
//   * the host accepts a hello only from that exact frame window.
//
// A token was tried first and removed: it cannot be read by the parent (a sandboxed document is
// an opaque origin, so `frame.contentDocument` is null) without either a second request or a
// query parameter the page itself can read, and once a nonce is needed to hide it the page's own
// inline scripts stop working. The structural check is both simpler and harder to get wrong.
//
// The script is deliberately written as ES5 (no template literals, no arrow functions, no
// const/let): it is concatenated into arbitrary documents, and the smallest possible surface
// for an injection bug is worth more than modern syntax in a file nobody reads.

// uiBridgeScriptID is the id of the injected script tag. It doubles as the idempotency marker:
// a document that already contains it is served unchanged.
const uiBridgeScriptID = "aigw-ui-bridge"

// Handshake frames. `hello` travels as a window message (there is no port yet); everything
// after the port transfer happens on the port itself.
const (
	uiBridgeHello = "aigw:hello"
	uiBridgeReady = "aigw:ready"
	uiBridgeKind  = "ui"
)

// How many times, and how often, the injected client repeats its greeting. The host may attach
// the frame before it installs the listener that receives the greeting, and a greeting that
// arrives first is gone for good; five attempts over ~3s covers that without being chatty.
const (
	uiBridgeHelloAttempts   = 6
	uiBridgeHelloIntervalMS = 600
)

// Frame-side limits, mirroring the host's. They exist so a runaway page cannot fill the
// console's memory before the host has a chance to reject anything.
const (
	// uiBridgeMaxEventBytes bounds one event payload inside the page.
	uiBridgeMaxEventBytes = 8 * 1024
	// uiBridgeMaxDepth bounds how deeply nested a submitted value may be.
	uiBridgeMaxDepth = 4
	// uiBridgeMaxFields bounds how many form controls one submission may carry.
	uiBridgeMaxFields = 64
)

// uiBridgeScript returns the client half of the bridge.
//
// It returns a string rather than a constant so tests can read exactly what is served: the
// guarantees worth pinning (no fetch, no XMLHttpRequest, no eval, no innerHTML, top document
// only) are properties of this text.
func uiBridgeScript() string {
	return `(function () {
  if (window.AIGW && window.AIGW.__aigwBridge) { return; }
  // Captured before any page code can run: the page shares this document and may replace
  // window.postMessage later.
  var post = window.parent.postMessage;
  if (typeof post !== 'function') { return; }
  // Only the top document of the frame gets the port. A nested frame on the page is a different
  // window and can post to the host itself, so without this it could be the one that receives a
  // port and then speak for the preview. This is the whole authentication: the secret-free
  // version of "prove you are the document we loaded".
  var framed = false;
  try { framed = window.top !== window; } catch (err) { framed = true; }
  if (framed) { return; }
  var port = null;
  var handlers = [];
  var byName = {};
  var directSeq = 0;
  var helloSent = 0;
  var helloTimer = 0;
  var maxBytes = ` + strconv.Itoa(uiBridgeMaxEventBytes) + `;

  // flatten turns a form into a flat object. Names are collapsed to their last segment
  // (items[0].sku -> sku) because the model is the consumer and a flat map is what it can
  // actually use; dangerous names are dropped rather than merged, so a form cannot reach
  // Object.prototype.
  function flatten(form) {
    var fd;
    try { fd = new FormData(form); } catch (err) { return {}; }
    var out = {};
    var fields = 0;
    var entries = [];
    try { entries = Array.from(fd.entries()); } catch (err) { entries = []; }
    for (var i = 0; i < entries.length; i++) {
      var name = String(entries[i][0]);
      if (!name || /^__proto__|^constructor$|^prototype$/.test(name)) { continue; }
      if (fields++ > ` + strconv.Itoa(uiBridgeMaxFields) + `) { break; }
      var key = name.split('.').pop().replace(/\[\]$/, '');
      if (!key || /^__proto__|^constructor$|^prototype$/.test(key)) { continue; }
      var value = entries[i][1];
      if (typeof value !== 'string') { continue; } // File: not serializable, dropped
      if (Object.prototype.hasOwnProperty.call(out, key)) {
        if (Object.prototype.toString.call(out[key]) === '[object Array]') { out[key].push(value); }
        else { out[key] = [out[key], value]; }
      } else {
        out[key] = value;
      }
    }
    var checkboxes = form.querySelectorAll('input[type=checkbox][name]');
    for (var j = 0; j < checkboxes.length; j++) {
      var box = checkboxes[j];
      var boxName = box.name.split('.').pop();
      if (!boxName || /^__proto__|^constructor$|^prototype$/.test(boxName)) { continue; }
      if (box.checked) { out[boxName] = true; }
      else if (!Object.prototype.hasOwnProperty.call(out, boxName)) { out[boxName] = false; }
    }
    return out;
  }

  function label(form, fallback) {
    var declared = form.getAttribute ? form.getAttribute('data-aigw-label') : '';
    if (declared) { return declared; }
    var heading = form.querySelector ? form.querySelector('h1, h2, h3, legend') : null;
    var text = heading && heading.textContent ? heading.textContent.trim() : '';
    if (text) { return text.slice(0, 40); }
    return fallback || '界面表单已提交';
  }

  function send(name, data, why) {
    if (!port) { return false; }
    var payload;
    try { payload = JSON.stringify(data === undefined ? {} : data); }
    catch (err) { payload = ''; }
    if (payload.length > maxBytes) {
      postFrame({ t: 'err', m: '提交内容超过 ' + maxBytes + ' 字节上限，请减少字段或内容长度' });
      return false;
    }
    postFrame({ t: 'ev', name: String(name || 'action'), label: String(why || ''), v: payload });
    return true;
  }

  function postFrame(frame) { port.postMessage(frame); }

  // Auto-binding: the two shapes a model actually writes. A <form> with a submit button, and
  // an element carrying data-aigw-send. Both are declarative, so the page needs no wiring and
  // a model that forgets the SDK still produces a working interface.
  function bind(root) {
    var forms = root.querySelectorAll('form');
    for (var i = 0; i < forms.length; i++) {
      (function (form) {
        form.addEventListener('submit', function (ev) {
          ev.preventDefault();
          var kind = form.getAttribute('data-aigw-event') || 'submit';
          send(kind, flatten(form), label(form, ''));
        });
      })(forms[i]);
    }
    var senders = root.querySelectorAll('[data-aigw-send]');
    for (var j = 0; j < senders.length; j++) {
      (function (node) {
        node.addEventListener('click', function (ev) {
          ev.preventDefault();
          var name = node.getAttribute('data-aigw-send');
          var raw = node.getAttribute('data-aigw-value');
          var data = {};
          if (raw) {
            try { data = JSON.parse(raw); }
            catch (err) {
              // Saying so beats sending an empty object: the model wrote JSON it thought was
              // valid, and the error must reach the console instead of vanishing.
              postFrame({ t: 'err', m: '按钮 ' + name + ' 的 data-aigw-value 不是合法 JSON：' + String(err) });
              return;
            }
          }
          send(name, data, node.getAttribute('data-aigw-label') || '');
        });
      })(senders[j]);
    }
    // Page-declared handlers: aigw-on="submit=handleSubmit" means "call window.handleSubmit
    // when the bridge receives an event named submit". This is how a page that wants to do
    // something before sending takes over, without the bridge needing a plugin API.
    var wired = root.querySelectorAll('[aigw-on]');
    for (var k = 0; k < wired.length; k++) {
      var spec = (wired[k].getAttribute('aigw-on') || '').split(',');
      for (var m = 0; m < spec.length; m++) {
        var pair = spec[m].split('=');
        if (pair.length !== 2) { continue; }
        wire(pair[0].trim(), pair[1].trim());
      }
    }
  }

  // Named handlers are kept by event name rather than as closures: AIGW.onEvent must be able
  // to register the same name twice, and a page that declares its handler late (after some
  // async work) still gets called, because the lookup happens at dispatch time.
  function wire(eventName, fnName) {
    if (!eventName || !fnName) { return; }
    if (!byName[eventName]) { byName[eventName] = []; }
    byName[eventName].push(fnName);
  }

  function callNamed(eventName, data) {
    var names = byName[eventName];
    if (!names) { return; }
    for (var i = 0; i < names.length; i++) {
      var fn = window[names[i]];
      if (typeof fn === 'function') { try { fn(data); } catch (err) { /* the page's problem */ } }
    }
  }

  // apply runs the console's DOM directive. Only the documented operations exist, and every
  // target is a selector resolved against this document. Unknown keys are ignored; this is a
  // patch channel, not a scripting channel.
  function apply(ops) {
    if (!ops || !ops.length) { return; }
    for (var i = 0; i < ops.length; i++) {
      var op = ops[i] || {};
      var nodes = [];
      try { nodes = op.target ? document.querySelectorAll(op.target) : []; } catch (err) { nodes = []; }
      for (var j = 0; j < nodes.length; j++) { applyOne(op, nodes[j]); }
    }
  }

  function applyOne(op, node) {
    var kind = op.op;
    if (kind === 'text') { node.textContent = String(op.value === undefined ? '' : op.value); }
    else if (kind === 'class') {
      var add = op.add || [];
      var remove = op.remove || [];
      for (var i = 0; i < add.length; i++) { node.classList.add(String(add[i])); }
      for (var j = 0; j < remove.length; j++) { node.classList.remove(String(remove[j])); }
    } else if (kind === 'style') {
      var style = op.style || {};
      for (var key in style) {
        if (Object.prototype.hasOwnProperty.call(style, key)) {
          try { node.style.setProperty(String(key), String(style[key])); } catch (err) { /* invalid property */ }
        }
      }
    } else if (kind === 'show') { node.hidden = false; node.style.display = ''; }
    else if (kind === 'hide') { node.hidden = true; node.style.display = 'none'; }
    else if (kind === 'remove') { if (node.parentNode) { node.parentNode.removeChild(node); } }
    else if (kind === 'focus') { if (node.focus) { node.focus(); } }
    else if (kind === 'disable') { setDisabled(node, op.value !== false); }
    else if (kind === 'set') { setValue(node, op); }
    else if (kind === 'message') { showMessage(node, op); }
    else if (kind === 'svg') { setSVG(node, op.svg); }
  }

  function setDisabled(node, disabled) {
    node.disabled = !!disabled;
    if (node.tagName === 'FIELDSET') {
      var inner = node.querySelectorAll('input, select, textarea, button');
      for (var i = 0; i < inner.length; i++) { inner[i].disabled = !!disabled; }
    }
  }

  function setValue(node, op) {
    var value = op.value;
    if (op.checked !== undefined) { node.checked = !!op.checked; return; }
    if (node.tagName === 'SELECT') {
      var wanted = Object.prototype.toString.call(value) === '[object Array]' ? value : [value];
      var options = node.options || [];
      for (var i = 0; i < options.length; i++) {
        options[i].selected = false;
        for (var j = 0; j < wanted.length; j++) {
          if (String(options[i].value) === String(wanted[j])) { options[i].selected = true; }
        }
      }
      return;
    }
    if (node.type === 'checkbox' || node.type === 'radio') { node.checked = !!value; return; }
    node.value = value === undefined || value === null ? '' : String(value);
  }

  function showMessage(node, op) {
    var host = node;
    var existing = host.querySelector(':scope > .aigw-msg');
    if (existing && existing.parentNode) { existing.parentNode.removeChild(existing); }
    var box = document.createElement('div');
    box.className = 'aigw-msg aigw-' + String(op.level || 'info');
    box.setAttribute('role', 'status');
    box.textContent = String(op.value === undefined ? '' : op.value);
    host.appendChild(box);
  }

  // setSVG builds nodes element by element. Nothing here parses markup: attributes are an
  // allow-list minus anything starting with "on", so a directive cannot smuggle a handler.
  var SVG_ALLOWED = ['x', 'y', 'x1', 'y1', 'x2', 'y2', 'cx', 'cy', 'r', 'rx', 'ry', 'width',
    'height', 'd', 'points', 'viewBox', 'fill', 'fill-opacity', 'fill-rule', 'stroke',
    'stroke-width', 'stroke-dasharray', 'stroke-linecap', 'stroke-linejoin', 'opacity',
    'transform', 'font-size', 'font-family', 'font-weight', 'text-anchor', 'dominant-baseline',
    'dx', 'dy', 'class', 'id', 'preserveAspectRatio'];
  function setSVG(node, spec) {
    while (node.firstChild) { node.removeChild(node.firstChild); }
    var built = buildSVG(spec, 0);
    if (built) { node.appendChild(built); }
  }

  function buildSVG(spec, depth) {
    if (!spec || depth > 16) { return null; }
    var tag = String(spec.tag || '');
    if (!/^[a-zA-Z][a-zA-Z0-9]*$/.test(tag)) { return null; }
    var node;
    try { node = document.createElementNS('http://www.w3.org/2000/svg', tag); }
    catch (err) { return null; }
    var attrs = spec.attrs || {};
    for (var key in attrs) {
      if (!Object.prototype.hasOwnProperty.call(attrs, key)) { continue; }
      if (SVG_ALLOWED.indexOf(key) < 0) { continue; }
      if (key.slice(0, 2).toLowerCase() === 'on') { continue; }
      try { node.setAttribute(key, String(attrs[key])); } catch (err) { /* invalid attribute */ }
    }
    if (spec.text !== undefined && spec.text !== null) { node.textContent = String(spec.text); }
    var kids = spec.children || [];
    for (var i = 0; i < kids.length; i++) {
      var child = buildSVG(kids[i], depth + 1);
      if (child) { node.appendChild(child); }
    }
    return node;
  }

  function dispatch(frame) {
    callNamed(frame.name, frame.data);
    for (var i = 0; i < handlers.length; i++) {
      try { handlers[i](frame.name, frame.data); } catch (err) { /* the page's problem */ }
    }
    if (frame.k === 'd') {
      var live = document.querySelector('[data-aigw-live]');
      if (live && frame.v) { live.textContent += String(frame.v); }
      return;
    }
    if (frame.k === 'ui' || frame.k === 'done') {
      if (frame.ops && frame.ops.length) { apply(frame.ops); }
      if (frame.k === 'done') {
        // A failed turn says so on the page as well: the operator may be looking at the form,
        // not at the transcript behind it.
        if (frame.error) { status('本轮失败：' + String(frame.error)); }
        else if (frame.status) { status(String(frame.status)); }
      }
      return;
    }
    if (frame.k === 'err') { status(String(frame.m || '')); return; }
    if (frame.k === 'state') { status(frame.v === 'busy' ? '模型正在处理…' : ''); }
  }

  // status writes into the page's own [data-aigw-status] node when it declared one. A page
  // without that node simply shows nothing: the console's toolbar carries the authoritative
  // state either way.
  function status(text) {
    var host = document.querySelector('[data-aigw-status]');
    if (host) { host.textContent = String(text || ''); }
  }

  window.addEventListener('message', function (ev) {
    var data = ev.data;
    if (!data || data.aigw !== '` + uiBridgeKind + `' || data.t !== 'port') { return; }
    if (port) { return; } // one port per document; a second offer is ignored
    port = ev.ports && ev.ports[0];
    if (!port) { return; }
    if (helloTimer) { window.clearInterval(helloTimer); helloTimer = 0; }
    port.onmessage = function (msg) {
      var frame = msg && msg.data;
      if (!frame) { return; }
      if (frame.t === 'ev' && typeof frame.v === 'string') {
        try { frame.data = JSON.parse(frame.v); } catch (err) { frame.data = {}; }
      }
      try { dispatch(frame); } catch (err) { /* a page handler must not break the channel */ }
    };
    // This first frame is the ack: the console stops waiting for the handshake when it
    // arrives, and it is where the page's own API becomes usable.
    postFrame({ t: 'ev', name: 'ready', label: '界面已就绪', v: '{}' });
    postFrame({ t: 'state', v: 'idle' });
  }, false);

  window.AIGW = {
    __aigwBridge: true,
    send: send,
    apply: apply,
    on: function (fn) { if (typeof fn === 'function') { handlers.push(fn); } },
    onEvent: function (name, fn) {
      if (typeof fn !== 'function' || !name) { return; }
      var slot = '__aigw_h_' + (++directSeq);
      window[slot] = fn;
      wire(String(name), slot);
    },
    isReady: function () { return !!port; }
  };

  // sayHello announces this document. It is sent once, and repeated a few times until the host
  // answers with a port: the host may not have installed its listener yet when this runs (it
  // attaches the frame and then listens, and the document can start loading in between), and a
  // dropped hello would otherwise leave a preview that can never connect. Repetition is
  // harmless — the host ignores every offer after the first, and each one is identical.
  function sayHello() {
    if (port || helloSent >= ` + strconv.Itoa(uiBridgeHelloAttempts) + `) { return; }
    helloSent++;
    try { post.call(window.parent, { aigw: '` + uiBridgeKind + `', t: 'hello', framed: false }, '*'); }
    catch (err) { /* a frame with no parent cannot be interactive */ }
  }

  function start() {
    bind(document);
    sayHello();
    if (!port) { helloTimer = window.setInterval(function () {
      if (port) { window.clearInterval(helloTimer); helloTimer = 0; return; }
      sayHello();
    }, ` + strconv.Itoa(uiBridgeHelloIntervalMS) + `); }
  }
  if (document.readyState === 'loading') { document.addEventListener('DOMContentLoaded', start); }
  else { start(); }
})();`
}

// injectUIBridge puts the bridge client into a document. Placement matters: the script must
// run before the page's own scripts have a chance to replace window.postMessage, so it goes as
// early as the document allows — right after <head ...>, or after <html ...>, or in front.
//
// A document that already carries the marker is returned unchanged: uploading the same page
// twice must not double-inject it.
func injectUIBridge(body string) string {
	if strings.Contains(body, uiBridgeScriptID) {
		return body
	}
	tag := `<script id="` + uiBridgeScriptID + `">` + uiBridgeScript() + `</script>`
	lower := strings.ToLower(body)
	for _, marker := range []string{"<head", "<html"} {
		index := strings.Index(lower, marker)
		if index < 0 {
			continue
		}
		end := strings.Index(lower[index:], ">")
		if end < 0 {
			continue
		}
		cut := index + end + 1
		return body[:cut] + tag + body[cut:]
	}
	return tag + body
}
