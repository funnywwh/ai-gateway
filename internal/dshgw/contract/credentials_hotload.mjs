#!/usr/bin/env node
// Usage: NODE credentials_hotload.mjs NODE /absolute/dsh/lib/bin.js
// Uses only the installed DSH's public Web RPC/mux and published packages.
// All provider traffic targets a private, disposable loopback fake; no real key
// or provider endpoint is inherited. The existing DSH GUI is never contacted.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { EventEmitter } from 'node:events';
import fs from 'node:fs/promises';
import http from 'node:http';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const MAX_BODY = 8 * 1024 * 1024;
const MAX_LOG = 64 * 1024;
const MAX_FRAMES = 2000;
const REQUEST_MS = 15_000;
const OVERALL_MS = 60_000;
const MODEL = 'dshgw-hotload-probe';
const KEY_A = 'sk-dshgw-hotload-dummy-one';
const KEY_B = 'sk-dshgw-hotload-dummy-two';
const REF = 'AIGW_API_KEY';
const stop = new AbortController();
const bus = new EventEmitter();
const frames = [];
const requests = [];
let frameBytes = 0;
let fixture;
let worker;
let workerDone;
let workerExited = false;
let socket;
let server;
let closing = false;
let cleanupTask;
let rotationTask;
let exitCode = 1;
let announcements = 0;
let stdout = '';
let stderr = '';
let startupLine = '';
let startupURL;
let fakePort;

function fail(error) {
  if (!stop.signal.aborted) stop.abort(error instanceof Error ? error : new Error(String(error)));
}

// Races both the overall stop signal and a local deadline. Always attaches
// rejection handlers to the underlying operation, even when already aborted.
function bounded(promise, ms, label, signal = stop.signal) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (fn, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener('abort', aborted);
      fn(value);
    };
    const aborted = () => finish(reject, signal.reason ?? new Error('aborted'));
    const timer = setTimeout(() => finish(reject, new Error(`timeout: ${label}`)), ms);
    Promise.resolve(promise).then(value => finish(resolve, value), error => finish(reject, error));
    signal?.addEventListener('abort', aborted, { once: true });
    if (signal?.aborted) aborted();
  });
}

async function event(emitter, name, ms, label) {
  let handler;
  const promise = new Promise(resolve => {
    handler = (...args) => resolve(args);
    emitter.once(name, handler);
  });
  try { return await bounded(promise, ms, label); }
  finally { emitter.off(name, handler); }
}

async function frame(predicate, from, label) {
  const existing = frames.slice(from).find(predicate);
  if (existing) return existing;
  let handler;
  const promise = new Promise(resolve => {
    handler = value => { if (predicate(value)) resolve(value); };
    bus.on('frame', handler);
  });
  try { return await bounded(promise, REQUEST_MS, label); }
  finally { bus.off('frame', handler); }
}

function assertLoopbackURL(raw, expectedPath) {
  const url = new URL(raw);
  assert.equal(url.protocol, 'http:', 'worker must use plain loopback HTTP');
  assert.equal(url.hostname, '127.0.0.1', 'worker must use explicit IPv4 loopback');
  assert(url.port && url.port !== '3080', 'must not contact the existing GUI');
  assert.equal(url.pathname, expectedPath);
  assert.equal(url.username, '');
  assert.equal(url.password, '');
  return url;
}

async function responseText(response, limit = MAX_BODY) {
  const reader = response.body?.getReader();
  if (!reader) return '';
  let size = 0;
  const chunks = [];
  try {
    for (;;) {
      const { value, done } = await bounded(reader.read(), REQUEST_MS, 'HTTP response body');
      if (done) break;
      size += value.byteLength;
      if (size > limit) throw new Error('HTTP response body exceeds contract limit');
      chunks.push(Buffer.from(value));
    }
    return Buffer.concat(chunks).toString('utf8');
  } finally {
    await bounded(reader.cancel(), 2000, 'cancel HTTP response', null).catch(() => {});
    reader.releaseLock();
  }
}

function signalWorker(signal) {
  if (!worker?.pid || workerExited) return;
  try {
    if (process.platform === 'win32') worker.kill(signal);
    else process.kill(-worker.pid, signal); // Only this disposable process group.
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}

function cleanup() {
  return cleanupTask ??= (async () => {
    closing = true;
    const failures = [];
    try { socket?.terminate(); } catch (error) { failures.push(error); }
    try {
      if (worker && !workerExited) {
        signalWorker('SIGTERM');
        try { await bounded(workerDone, 4000, 'worker shutdown', null); }
        catch {
          signalWorker('SIGKILL');
          await bounded(workerDone, 4000, 'worker kill', null);
        }
      }
    } catch (error) { failures.push(error); }
    try {
      if (server) {
        server.closeAllConnections();
        if (server.listening) await bounded(new Promise(resolve => server.close(resolve)), 2000, 'fake server shutdown', null);
      }
    } catch (error) { failures.push(error); }
    try {
      // A canceled wait does not cancel filesystem work already admitted by
      // withFileLock. Let that bounded local writer settle before deleting home.
      if (rotationTask) await bounded(rotationTask.catch(() => {}), 5000, 'credential writer cleanup', null);
    } catch (error) { failures.push(error); }
    try {
      if (fixture) await bounded(fs.rm(fixture, { recursive: true, force: true }), 4000, 'fixture cleanup', null);
    } catch (error) { failures.push(error); }
    if (failures.length) throw new AggregateError(failures, failures.map(error => error.message).join('; '));
  })();
}

const interrupted = signal => {
  exitCode = signal === 'SIGINT' ? 130 : 143;
  fail(new Error(`interrupted by ${signal}`));
};
const onINT = () => interrupted('SIGINT');
const onTERM = () => interrupted('SIGTERM');
process.on('SIGINT', onINT);
process.on('SIGTERM', onTERM);
const overall = setTimeout(() => fail(new Error('credentials hotload contract exceeded 60 seconds')), OVERALL_MS);

async function run() {
  assert.equal(process.argv.length, 4, 'usage: NODE credentials_hotload.mjs NODE DSH_BIN_JS');
  const node = await fs.realpath(process.argv[2]);
  const bin = await fs.realpath(process.argv[3]);
  assert((await fs.stat(node)).isFile(), 'Node runtime must be a file');
  assert((await fs.stat(bin)).isFile(), 'DSH entry must be a file');
  const packageJSON = path.join(path.dirname(path.dirname(bin)), 'package.json');
  const pkg = JSON.parse(await fs.readFile(packageJSON, 'utf8'));
  assert.equal(pkg.name, '@deepseek-ai/dsh', 'DSH bin must belong to the installed CLI package');
  const require = createRequire(packageJSON);
  const WebSocket = require('ws');
  const yaml = require('js-yaml');
  const { withFileLock, writeFileAtomic } = await import(pathToFileURL(require.resolve('@deepseek-ai/dsh-atomic-write')));

  stop.signal.throwIfAborted();
  fixture = await fs.mkdtemp(path.join(os.tmpdir(), 'dshgw-credentials-hotload-'));
  const home = path.join(fixture, 'home');
  const dshHome = path.join(home, '.dsh');
  const cwd = path.join(fixture, 'work');
  await fs.mkdir(dshHome, { recursive: true, mode: 0o700 });
  await fs.mkdir(cwd, { recursive: true, mode: 0o700 });

  server = http.createServer(async (req, res) => {
    req.setTimeout(5000, () => { fail(new Error('fake request timed out')); req.destroy(); });
    try {
      assert.equal(req.method, 'POST', 'unexpected fake endpoint method');
      assert.equal(req.url, '/v1/responses', 'unexpected fake endpoint path');
      assert.equal(req.headers.host, `127.0.0.1:${fakePort}`, 'unexpected fake endpoint authority');
      let size = 0;
      const chunks = [];
      for await (const chunk of req) {
        size += chunk.byteLength;
        if (size > MAX_BODY) throw new Error('fake request body exceeds contract limit');
        chunks.push(chunk);
      }
      const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      assert.equal(body.model, MODEL);
      assert.equal(body.stream, true);
      const n = requests.length + 1;
      assert(n <= 2, 'unexpected extra model request (title generation/retries must be disabled)');
      // Do not include a surprising Authorization value in an assertion error.
      assert(req.headers.authorization === `Bearer ${n === 1 ? KEY_A : KEY_B}`, `request ${n} did not use the expected dummy credential`);
      requests.push({ model: body.model, phase: n });
      const text = `hotload-ok-${n}`;
      const message = { id: `msg_hotload_${n}`, type: 'message', status: 'completed', role: 'assistant', content: [{ type: 'output_text', text, annotations: [] }] };
      const response = {
        id: `resp_hotload_${n}`, object: 'response', created_at: Math.floor(Date.now() / 1000), status: 'completed', model: MODEL, output: [message],
        usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2, input_tokens_details: { cached_tokens: 0 }, output_tokens_details: { reasoning_tokens: 0 } },
      };
      // Minimal complete Responses SSE accepted by installed pi-ai. Terminal
      // response.completed is required; [DONE] alone is not a completed response.
      const events = [
        { type: 'response.created', response: { ...response, status: 'in_progress', output: [] } },
        { type: 'response.output_item.added', output_index: 0, item: { ...message, status: 'in_progress', content: [] } },
        { type: 'response.output_text.delta', output_index: 0, content_index: 0, item_id: message.id, delta: text },
        { type: 'response.output_item.done', output_index: 0, item: message },
        { type: 'response.completed', response },
      ];
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'close' });
      for (const value of events) res.write(`event: ${value.type}\ndata: ${JSON.stringify(value)}\n\n`);
      res.end();
    } catch (error) {
      fail(error);
      if (!res.headersSent) res.writeHead(500);
      res.end('fake provider contract failed');
    }
  });
  server.on('error', fail);
  stop.signal.throwIfAborted();
  server.listen({ port: 0, host: '127.0.0.1', signal: stop.signal });
  await event(server, 'listening', 5000, 'fake listener');
  fakePort = server.address().port;
  assert.notEqual(fakePort, 3080);

  const credentials = path.join(dshHome, '.credentials.yaml');
  await fs.writeFile(credentials, yaml.dump({ version: 1, refs: { [REF]: KEY_A }, records: {} }), { mode: 0o600 });
  await fs.writeFile(path.join(dshHome, 'settings.yaml'), yaml.dump({
    'agent-default-model': { provider: 'aigw', model: MODEL },
    'llm-pi-ai': { providers: { aigw: {
      apiKeyEnv: REF, api: 'openai-responses', baseURL: `http://127.0.0.1:${fakePort}/v1`,
      models: [{ id: MODEL, name: 'Disposable hotload probe' }], defaultMaxTokens: 32,
      retryPolicy: { mode: 'normal', maxRetries: 0 },
    } } },
  }), { mode: 0o600 });
  await fs.writeFile(path.join(dshHome, 'cordis.patch.yml'), '- id: session-title-llm\n  disabled: true\n', { mode: 0o600 });

  stop.signal.throwIfAborted();
  worker = spawn(node, [bin, 'web', '--no-open', '--host', '127.0.0.1', '--port', '0'], {
    cwd,
    // An inherited API key shadows the file forever. Do not inherit provider
    // keys, proxy variables, real HOME, DSH config, or caller preload hooks.
    env: { PATH: `${path.dirname(node)}${path.delimiter}${process.env.PATH ?? ''}`, HOME: home, DSH_HOME: dshHome, LANG: 'C.UTF-8', NO_COLOR: '1' },
    detached: process.platform !== 'win32',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const pid = worker.pid;
  workerDone = new Promise(resolve => {
    worker.once('exit', (code, signal) => {
      workerExited = true;
      resolve();
      if (!closing) fail(new Error(`disposable worker exited unexpectedly (${code ?? signal})`));
    });
    worker.once('error', error => { workerExited = true; resolve(); fail(error); });
  });
  worker.stdout.on('data', data => {
    stdout = (stdout + data.toString()).slice(-MAX_LOG);
    startupLine += data.toString();
    if (startupLine.length > MAX_LOG) startupLine = startupLine.slice(-MAX_LOG);
    let newline;
    while ((newline = startupLine.indexOf('\n')) !== -1) {
      const line = startupLine.slice(0, newline).replace(/\r$/, '');
      startupLine = startupLine.slice(newline + 1);
      const match = line.match(/^dsh web: (http:\/\/127\.0\.0\.1:\d+\/\?token=[A-Za-z0-9_-]{43})(?: \(LAN: .+\))?$/);
      if (match) { announcements++; startupURL = match[1]; bus.emit('startup'); }
    }
  });
  worker.stderr.on('data', data => { stderr = (stderr + data.toString()).slice(-MAX_LOG); });
  if (!startupURL) await event(bus, 'startup', 25_000, 'worker readiness');
  const url = assertLoopbackURL(startupURL, '/');
  const origin = url.origin;
  const fetched = async (target, options = {}) => {
    const selected = assertLoopbackURL(target, new URL(target).pathname);
    assert.equal(selected.origin, origin, 'request escaped disposable worker authority');
    return bounded(fetch(selected, { ...options, signal: AbortSignal.any([stop.signal, AbortSignal.timeout(REQUEST_MS)]) }), REQUEST_MS, 'worker HTTP request');
  };
  const auth = await fetched(startupURL, { redirect: 'manual' });
  assert.equal(auth.status, 303, 'startup token exchange must redirect');
  assert.equal(auth.headers.get('location'), '/');
  const cookie = auth.headers.get('set-cookie')?.split(';', 1)[0];
  assert(cookie?.startsWith('dsh-auth-'), 'token exchange did not issue a browser cookie');
  await responseText(auth, 4096);
  let rpcId = 0;
  const rpc = async (method, args) => {
    const id = `hotload-rpc-${++rpcId}`;
    const response = await fetched(`${origin}/api/${method}`, {
      method: 'POST', redirect: 'error', headers: { 'Content-Type': 'application/json', Cookie: cookie },
      body: JSON.stringify({ type: 'client-request', rpcId: id, method, payload: { args } }),
    });
    assert.equal(response.status, 200, `RPC ${method} failed HTTP status`);
    const envelope = JSON.parse(await responseText(response));
    assert.equal(envelope.type, 'server-response');
    assert.equal(envelope.rpcId, id);
    assert.equal(envelope.result?.ok, true, `RPC ${method} failed: ${envelope.result?.error?.code ?? 'invalid response'}`);
    return envelope.result.value;
  };

  socket = new WebSocket(`${origin.replace('http:', 'ws:')}/api/remote.mux`, {
    headers: { Cookie: cookie }, maxPayload: MAX_BODY, handshakeTimeout: 5000, followRedirects: false,
  });
  socket.on('error', error => { if (!closing) fail(error); });
  socket.on('close', () => { if (!closing) fail(new Error('disposable RPC stream closed unexpectedly')); });
  socket.on('message', (data, binary) => {
    try {
      assert.equal(binary, false, 'RPC mux must use text frames');
      frameBytes += data.byteLength;
      assert(frameBytes <= MAX_BODY && frames.length < MAX_FRAMES, 'RPC mux exceeded contract bounds');
      const value = JSON.parse(data.toString());
      assert(['item', 'end', 'error'].includes(value.type), 'unknown mux frame');
      if (value.type !== 'item') throw new Error(`RPC stream ${value.streamId} ended unexpectedly (${value.type})`);
      frames.push(value);
      bus.emit('frame', value);
    } catch (error) { fail(error); }
  });
  await event(socket, 'open', 5000, 'worker websocket');
  socket.send(JSON.stringify({ type: 'open', streamId: 'events', endpoint: '$events', payload: { args: {} } }));
  await frame(value => value.streamId === 'events' && value.value?.type === 'ready', 0, 'event subscription readiness');
  const { sessionId } = await rpc('session/create', { request: { cwd } });
  assert.equal(typeof sessionId, 'string');
  await rpc('session/selectModel', { request: { sessionId, provider: 'aigw', model: MODEL } });
  socket.send(JSON.stringify({ type: 'open', streamId: 'history', endpoint: 'session/follow', payload: { args: { request: { address: { kind: 'session', sessionId }, maxMessages: 20 } } } }));
  await frame(value => value.streamId === 'history' && value.value?.type === 'snapshot', 0, 'session opening snapshot');

  const prompt = async number => {
    const from = frames.length;
    const requestId = `hotload-prompt-${number}`;
    const accepted = await rpc('session/prompt', { request: { requestId, sessionId, mode: 'queue', content: [{ type: 'text', text: `Say hotload-ok-${number} only.` }] } });
    assert.equal(accepted?.accepted, true);
    const ended = await frame(value => value.streamId === 'history' && value.value?.type === 'event' && value.value.event.type === 'turn/end', from, `turn ${number} completion`);
    assert.equal(ended.value.event.data.reason.kind, 'completed', 'model turn did not complete successfully');
    const events = frames.slice(from).filter(value => value.streamId === 'history' && value.value?.type === 'event').map(value => value.value.event);
    assert(events.some(value => value.type === 'user/message' && value.data.source?.rpcId === requestId), 'prompt correlation missing from transcript');
    assert(events.some(value => value.type === 'assistant/message' && value.data.turn === ended.value.event.data.turn && value.data.interrupted !== true && value.data.message.content.some(part => part.type === 'text' && part.text === `hotload-ok-${number}`)), 'completed assistant reply missing from transcript');
  };
  await prompt(1);
  assert.equal(requests.length, 1, 'first prompt must make exactly one fake request');

  const beforeRotation = frames.length;
  rotationTask = withFileLock(credentials, async () => {
    const document = yaml.load(await fs.readFile(credentials, 'utf8'));
    assert(document.records?.['client-connection/browser-session'], 'must preserve browser-auth record during rotation');
    document.refs[REF] = KEY_B;
    await writeFileAtomic(credentials, yaml.dump(document), { mode: 0o600 });
  });
  await bounded(rotationTask, 5000, 'credential writer lock');
  // No arbitrary sleeps: this forwarded public event is sent only after the
  // credentials provider has installed the new in-memory snapshot.
  await frame(value => value.streamId === 'events' && value.value?.type === 'emit' && value.value.event === 'credentials/reference-updated' && value.value.args?.[0] === REF, beforeRotation, 'credential reload barrier');
  await prompt(2);
  assert.equal(requests.length, 2, 'rotation must not cause extra provider requests');
  assert.equal(worker.pid, pid);
  assert.equal(workerExited, false, 'worker restarted or exited');
  assert.equal(announcements, 1, 'worker announced more than one startup');
  return { passed: true, dshVersion: pkg.version, fakeRequests: 2, sameSession: true, workerPidUnchanged: true, startupAnnouncements: announcements, credentialReloadEvent: true, firstKeyObserved: true, rotatedKeyObserved: true };
}

function scrubDiagnostic(value) { return String(value).replace(/([?&]token=)[A-Za-z0-9_-]+/g, '$1[redacted]').replaceAll(KEY_A, '[dummy-key]').replaceAll(KEY_B, '[dummy-key]'); }

let result;
try {
  result = await run();
  stop.signal.throwIfAborted();
  exitCode = 0;
} catch (error) {
  fail(error);
  console.error(`credentials hotload contract failed: ${scrubDiagnostic(error.message)}`);
  // Only disposable-home diagnostics exist, but scrub launch tokens and dummy
  // keys anyway so this script remains safe to integrate into operator logs.
  const scrub = value => value.replace(/\?token=[A-Za-z0-9_-]+/g, '?token=[redacted]').replaceAll(KEY_A, '[dummy-key]').replaceAll(KEY_B, '[dummy-key]');
  if (stderr) console.error(scrub(stderr.slice(-6000)));
  if (stdout) console.error(scrub(stdout.slice(-2000)));
} finally {
  clearTimeout(overall);
  try { await cleanup(); }
  catch (error) { exitCode = 1; console.error(`credentials hotload cleanup failed: ${scrubDiagnostic(error.message)}`); }
  process.off('SIGINT', onINT);
  process.off('SIGTERM', onTERM);
  if (exitCode === 0) console.log(JSON.stringify({ ...result, cleaned: true }));
  process.exitCode = exitCode;
}
