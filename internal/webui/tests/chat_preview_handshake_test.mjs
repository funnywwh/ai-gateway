// Node-only guard for the console's half of the interactive-preview handshake (M34).
//
// It pins one fact that is invisible in an editor and was shipped once: the frame's window is
// resolved **when the greeting arrives**, not when the port is created. The console deliberately
// creates the port before it attaches the frame (that ordering is what keeps the first greeting
// from being dropped), and an iframe has no content window until it is connected to a document
// (the spec creates the child navigable in the element's post-connection steps, and
// `contentWindow` returns null while there is none). An eager read therefore captures null, every
// greeting fails the identity check, and the only symptom the operator sees is the toolbar saying
// 「不可交互」 three seconds later.
//
// What this file is not: a substitute for the browser. It drives the *decision* (which source may
// hand over a port), with a window stand-in, because the real channel needs real frames and real
// ports — that is scripts/ui-harness (`chat` view, live phase), which loads the actual injected
// script in a real sandboxed iframe. The two are complementary, and this one runs where no
// browser exists.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const sourceURL = new URL('../static/js/pages/chat_ui.js', import.meta.url);
const source = await readFile(sourceURL, 'utf8');

// The window the module registers its listener on. Only what createUIPort uses is provided, and
// removeEventListener really removes: each scenario below owns exactly one port.
const listeners = new Set();
const context = vm.createContext({
  window: {
    addEventListener: (type, fn) => { if (type === 'message') listeners.add(fn); },
    removeEventListener: (type, fn) => { if (type === 'message') listeners.delete(fn); },
  },
  // The handshake timer is the "nobody answered" path; it must not fire here, or a scenario that
  // failed would be reported as a timeout instead of the rejected greeting it was.
  setTimeout: () => 1,
  clearTimeout: () => {},
});

const module = new vm.SourceTextModule(source, { identifier: sourceURL.href, context });
await module.link(() => {
  throw new Error('chat_ui.js must not import anything: this test cannot stand up a dependency');
});
await module.evaluate();
const { createUIPort } = module.namespace;

// portFor builds one port over a frame that is *not yet attached* (contentWindow === null), which
// is the state chat_artifact.js creates it in.
function portFor() {
  const frame = { window: null, get contentWindow() { return this.window; } };
  const port = createUIPort({ frame, onEvent: () => {}, onError: () => {} });
  return { frame, port };
}

// greet delivers one greeting exactly as the browser would: a message event on the frame's own
// window registration, carrying whatever ports the sender transferred.
function greet(source, data, ports) {
  for (const fn of [...listeners]) fn({ source, data, ports });
}

{
  // The regression. The frame is attached after the port exists, so at creation time its window is
  // null; the greeting arrives once it is not.
  const { frame, port } = portFor();
  assert.equal(frame.contentWindow, null,
    'the stand-in frame must start with no content window, or this test proves nothing about the '
    + 'order chat_artifact.js creates the port in');
  frame.window = { name: 'frame-window' };
  const channel = { sent: [], onmessage: null, start() {}, close() {}, postMessage(m) { this.sent.push(m); } };
  greet(frame.window, { aigw: 'ui', t: 'hello', framed: false }, [channel]);
  assert.equal(port.state.status, 'ready',
    'a greeting from the frame after it is attached must complete the handshake — reading '
    + 'frame.contentWindow while the frame is still detached captures null and silently kills the '
    + 'channel (the toolbar then reports 不可交互)');
  assert.equal(port.state.bridge, true, 'the adopted port is this preview\'s channel');
  port.send({ k: 'state', v: 'busy' });
  assert.deepEqual(channel.sent, [{ k: 'state', v: 'busy' }],
    'the console answers over the port it adopted, never over a global channel the page could read');
  port.destroy();
}

{
  // The late read must not have loosened anything: a greeting from another window is not ours.
  const { frame, port } = portFor();
  frame.window = { name: 'frame-window' };
  const channel = { sent: [], onmessage: null, start() {}, close() {}, postMessage() {} };
  greet({ name: 'some-other-window' }, { aigw: 'ui', t: 'hello', framed: false }, [channel]);
  assert.equal(port.state.status, 'handshaking',
    'only the exact frame window may hand over a port (a sibling frame or an opener must not)');
  port.destroy();
}

{
  // …and a message-shaped object without a port is not our bridge either.
  const { frame, port } = portFor();
  frame.window = { name: 'frame-window' };
  greet(frame.window, { aigw: 'ui', t: 'hello', framed: false }, []);
  assert.equal(port.state.status, 'handshaking', 'a hello without a MessagePort is ignored');
  port.destroy();
}

{
  // A nested frame on the model's page can post to us itself; it says so in the payload and must
  // stay out, port or no port.
  const { frame, port } = portFor();
  frame.window = { name: 'frame-window' };
  const channel = { sent: [], onmessage: null, start() {}, close() {}, postMessage() {} };
  greet(frame.window, { aigw: 'ui', t: 'hello', framed: true }, [channel]);
  assert.equal(port.state.status, 'handshaking', 'a hello that declares itself framed is refused');
  port.destroy();
}

console.log('chat preview handshake checks passed (window resolved at greeting time, sources still checked).');
