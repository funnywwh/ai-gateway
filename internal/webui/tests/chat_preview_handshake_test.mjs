// Node-only guard for the interactive-preview handshake (M34) — both halves of it.
//
// This is the channel the whole feature hangs on, and every one of its failures so far looked
// identical from the outside: the toolbar said 不可交互. Three of them were structural and shipped
// that way, so they are pinned here by *running* both halves rather than by reading them:
//
//   1. the console resolved the frame's window once, when the port was created — before the frame
//      was attached, when that window does not exist yet (an iframe has no content window until
//      it is connected to a document). The greeting then never matched, and the identity check
//      that is supposed to protect the channel rejected everything;
//   2. nobody created the MessagePort: the console waited for a greeting that carried a port, and
//      the injected script waited to be handed one. No message ever crossed;
//   3. the injected script refused to start when it was not the top document — and the preview IS
//      framed, so that test was true in every preview there ever was. It returned before defining
//      `window.AIGW`, so the page could not even send.
//
// What is faked here is the DOM, and only the DOM: a window for the page half (framed, because the
// preview always is), a window for the parent, and the document's element queries. What is *not*
// faked is the code under test — the real `chat_ui.js` from the console and the real injected
// script from `scripts/ui-harness/fixtures.json`, which a Go test rewrites from `uiBridgeScript()`
// on every `go test`. The real browser is still the authority for the DOM itself, and
// scripts/ui-harness drives the same two halves in one (view `chat`, live phase).
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const chatUIURL = new URL('../static/js/pages/chat_ui.js', import.meta.url);
const chatUISource = await readFile(chatUIURL, 'utf8');
const fixtures = JSON.parse(
  await readFile(new URL('../../../scripts/ui-harness/fixtures.json', import.meta.url), 'utf8'),
);
const bridgeSource = fixtures['/js/pages/chat_ui_bridge_script.js'];
assert.ok(bridgeSource && bridgeSource.length > 1000,
  'the harness fixtures must carry the injected script (internal/httpapi dumps it on every go test)');

// ---------------------------------------------------------------------------
// the console's half
// ---------------------------------------------------------------------------

// loadConsoleHalf evaluates the real module against a window that only records message listeners:
// createUIPort registers one, and the greeting is delivered through it by hand (in a browser the
// frame's window is a cross-origin object and cannot be dispatched at, which is the whole reason
// this decision is a pure function of the message).
async function loadConsoleHalf() {
  const listeners = new Set();
  const context = vm.createContext({
    window: {
      addEventListener: (type, fn) => { if (type === 'message') listeners.add(fn); },
      removeEventListener: (type, fn) => { if (type === 'message') listeners.delete(fn); },
    },
    // The console opens the channel itself; Node's real MessageChannel gives the test real
    // transfer semantics instead of a hand-made port object.
    MessageChannel,
    // The handshake timer is the "nobody answered" path. It must not fire here, or a failing
    // assertion would be reported as a timeout instead of the rejected greeting it was.
    setTimeout: () => 1,
    clearTimeout: () => {},
  });
  const module = new vm.SourceTextModule(chatUISource, { identifier: chatUIURL.href, context });
  await module.link(() => {
    throw new Error('chat_ui.js must not import anything: this test cannot stand up a dependency');
  });
  await module.evaluate();
  return { createUIPort: module.namespace.createUIPort, listeners };
}

// deliverGreeting plays the browser: the frame's own window posts to its parent, and the console's
// listener (registered on the console's window) receives it with that window as `source`.
function deliverGreeting(listeners, frameWindow, data) {
  for (const fn of [...listeners]) fn({ source: frameWindow, data, ports: [] });
}

// ---------------------------------------------------------------------------
// the page's half
// ---------------------------------------------------------------------------

// runInjectedScript runs the real injected script in a framed window: `window.top` is a different
// object, exactly as it is for a preview inside the console's sandboxed iframe. A script that
// bases its own startup on "am I the top document?" bails out here — which is the point.
function runInjectedScript() {
  const toParent = [];
  const toFrame = [];
  const frameListeners = new Set();
  const intervals = [];
  const parentWindow = {
    postMessage: (data, targetOrigin, transfer) => toParent.push({ data, targetOrigin, transfer: transfer || [] }),
  };
  const frameWindow = {
    parent: parentWindow,
    top: {}, // NOT this window: the preview is framed, and this is the DOM fact under test
    // The console posts to this window — that is how the port is handed over. A browser delivers
    // such a message to this window's own listeners; the test delivers it explicitly instead, so
    // that "the port was offered" and "the page answered" stay two separate observations.
    postMessage: (data, targetOrigin, transfer) => toFrame.push({ data, targetOrigin, transfer: transfer || [] }),
    addEventListener: (type, fn) => { if (type === 'message') frameListeners.add(fn); },
    removeEventListener: (type, fn) => { if (type === 'message') frameListeners.delete(fn); },
    setInterval: (fn, ms) => { intervals.push({ fn, ms }); return intervals.length; },
    clearInterval: () => {},
  };
  frameWindow.window = frameWindow;
  frameWindow.self = frameWindow;
  const document = {
    readyState: 'complete', // start() runs synchronously, which is what the preview does
    addEventListener: () => {},
    querySelectorAll: () => [],
    querySelector: () => null,
  };
  const context = vm.createContext({ window: frameWindow, document, JSON });
  vm.runInContext(bridgeSource, context, { filename: 'aigw-ui-bridge.js' });
  return { frameWindow, toParent, toFrame, frameListeners, intervals };
}

// handPortToPage plays the browser again: a message the console posted to the frame's window
// arrives at that window's own listener, with the transferred port in `ports`.
function handPortToPage(page, message) {
  for (const fn of [...page.frameListeners]) {
    fn({ data: message.data, source: page.frameWindow, ports: message.transfer });
  }
}

// ---------------------------------------------------------------------------
// the handshake, both halves at once
// ---------------------------------------------------------------------------

{
  const { createUIPort, listeners } = await loadConsoleHalf();
  const frame = { window: null, get contentWindow() { return this.window; } };
  const port = createUIPort({ frame, onEvent: () => {}, onError: () => {} });

  // The order the console really uses: the port exists before the frame is attached, so the
  // frame's window is null at this moment and may only be resolved later (defect 1).
  assert.equal(frame.contentWindow, null,
    'the stand-in frame must start with no content window, or this test proves nothing about the '
    + 'order chat_artifact.js creates the port in');

  const page = runInjectedScript();
  frame.window = page.frameWindow;

  // The page greets on its own. Defect 3 lives here: a script that bails out when framed never
  // produces this message, and nothing after it can work.
  const greet = page.toParent.find((m) => m.data && m.data.t === 'hello');
  assert.ok(greet, 'the injected script must greet its parent even though it is framed');
  assert.deepEqual({ ...greet.data }, { aigw: 'ui', t: 'hello' },
    'the greeting carries no port and no self-declared frame flag: the console owns the channel');
  assert.ok(page.frameWindow.AIGW && page.frameWindow.AIGW.__aigwBridge,
    'the injected script must expose window.AIGW — model pages call it, and it only exists if the '
    + 'script ran to the end');

  deliverGreeting(listeners, page.frameWindow, greet.data);

  // Defect 2 lives here: the console must create the channel and transfer one end to the greeting's
  // source window, which is what the page is waiting for.
  const offer = page.toFrame.find((m) => m.data && m.data.t === 'port');
  assert.ok(offer, 'the console must open the channel and hand one end to the frame');
  assert.equal(offer.transfer.length, 1, 'the offer must transfer exactly one MessagePort');
  assert.equal(port.state.status, 'handshaking',
    'handing over a port is not proof the page received it: 已连接 only after the page answers');

  handPortToPage(page, offer);
  for (let i = 0; i < 50 && port.state.status !== 'ready'; i++) {
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
  assert.equal(port.state.status, 'ready',
    'the page answers on the port it was given, and that ack is what completes the handshake');
  port.destroy();
  assert.equal(port.state.status, 'closed', 'destroy closes the channel');
}

// ---------------------------------------------------------------------------
// the checks that must survive the fix
// ---------------------------------------------------------------------------

{
  // Only the frame's own window is ever answered. A sibling frame (or an opener) can post the same
  // message shape, but it is not the window the console loaded, so it must not receive a port.
  const { createUIPort, listeners } = await loadConsoleHalf();
  const page = runInjectedScript();
  const port = createUIPort({ frameWindow: page.frameWindow, onEvent: () => {} });
  const stranger = { postMessage: () => { throw new Error('a port was handed to a stranger'); } };
  port.handleWindowMessage({ aigw: 'ui', t: 'hello' }, stranger);
  assert.equal(port.state.status, 'handshaking', 'a greeting from another window is not answered');

  // …and through the real listener path, for the same reason: the listener compares `ev.source`
  // against the frame's window, resolved at that moment.
  const frame = { window: null, get contentWindow() { return this.window; } };
  const other = createUIPort({ frame, onEvent: () => {} });
  frame.window = { name: 'frame-window' };
  deliverGreeting(listeners, { name: 'someone-else' }, { aigw: 'ui', t: 'hello' });
  assert.equal(other.state.status, 'handshaking', 'the listener answers that window alone');
  other.destroy();
  port.destroy();
}

{
  // Shape: the two fields that identify a greeting must match, and nothing is posted back for a
  // message that is not one. Extra fields are tolerated on purpose — the page shares a document
  // with our script, and a field added by a newer console must not lock an older page out.
  const { createUIPort } = await loadConsoleHalf();
  const sent = [];
  const frameWindow = { postMessage: (data, origin, transfer) => sent.push({ data, transfer }) };
  const port = createUIPort({ frameWindow, onEvent: () => {} });
  port.handleWindowMessage({ aigw: 'other', t: 'hello' }, undefined);
  assert.equal(sent.length, 0, 'the bridge kind must match exactly');
  port.handleWindowMessage({ aigw: 'ui', t: 'not-hello' }, undefined);
  assert.equal(sent.length, 0, 'only a greeting opens the channel');
  port.handleWindowMessage({ aigw: 'ui', t: 'hello', extra: true }, undefined);
  assert.equal(sent.length, 1, 'a greeting with extra fields is still a greeting');
  assert.equal(port.state.status, 'handshaking', 'and it still waits for the page to answer on the port');
  port.destroy();
}

console.log('chat preview handshake checks passed (both halves: greeting sent, port handed over, ack received).');
