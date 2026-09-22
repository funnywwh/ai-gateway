// Tests for the plugin entry point (index.js): config parsing, node-pty resolution from the dsh
// release, RPC channel registration, and the two lifecycle effects.
//
// This loads the real module, so a failure here means the host row would fail to activate.

import { createRequire } from 'node:module'
import { strict as assert } from 'node:assert'
import { existsSync, mkdtempSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const HERE = dirname(fileURLToPath(import.meta.url))
const PLUGIN_DIR = join(HERE, '..')
// The trace goes to a throwaway directory, never next to the module: that is where a row without a
// `traceFile` points, and a test run must not leave runtime files in the checkout.
const TRACE = join(mkdtempSync(join(tmpdir(), 'dshgw-web-tty-test-')), 'trace.jsonl')

const plugin = await import('../index.js')

/** A cordis-shaped context that records what the plugin registers. */
function createContext() {
  const effects = []
  const channels = new Map()
  const logs = []
  return {
    effects,
    channels,
    logs,
    logger: { info: (message) => logs.push(message), warn: (message) => logs.push(`WARN ${message}`), error: (message) => logs.push(`ERROR ${message}`) },
    effect(callback, label) {
      const dispose = callback()
      effects.push({ label, dispose })
      return dispose
    },
    connection: {
      rpc: {
        handle: (channel, handler) => { channels.set(channel, handler); return async () => { channels.delete(channel) } },
      },
    },
  }
}

test('host: the plugin exports the cordis face the loader needs', () => {
  assert.equal(plugin.name, 'dshgw-web-tty')
  assert.deepEqual(plugin.inject, ['connection'])
  assert.equal(typeof plugin.apply, 'function')
  assert.equal(typeof plugin.default?.apply, 'function')
})

test('host: the row may name the trace file, and an out-of-tree row keeps the module\'s own', () => {
  // Every tenant shares one installed copy (M75), so the gateway names a per-tenant trace; a row
  // that stays silent keeps the $DSH_HOME/plugins default, and a relative path is not a path.
  assert.equal(plugin.readConfig({}).traceFile, join(PLUGIN_DIR, 'trace.jsonl'))
  assert.equal(plugin.readConfig({ traceFile: TRACE }).traceFile, TRACE)
  assert.equal(plugin.readConfig({ traceFile: 'trace.jsonl' }).traceFile, join(PLUGIN_DIR, 'trace.jsonl'))
  assert.equal(plugin.readConfig({ traceFile: 42 }).traceFile, join(PLUGIN_DIR, 'trace.jsonl'))
})

test('host: activates, resolves node-pty, and answers every endpoint over its channel', async () => {
  const ctx = createContext()
  const before = existsSync(TRACE) ? readFileSync(TRACE, 'utf8') : ''
  plugin.apply(ctx, { shell: '/bin/bash', args: ['--noprofile', '--norc', '-i'], trace: true, traceFile: TRACE })

  assert.ok(ctx.channels.has('/dshgw-web-tty'), 'the browser channel is registered')
  assert.ok(ctx.effects.some((effect) => effect.label === 'web-tty: terminal reaper'), 'the PTY reaper is registered')

  const handler = ctx.channels.get('/dshgw-web-tty')
  const envelope = async (endpoint, payload, signal) => await handler(endpoint, payload, signal)

  const hello = await envelope('hello', {})
  assert.equal(hello.ok, true)
  assert.equal(hello.value.version.length > 0, true)
  assert.match(String(hello.value.nodePty), /^1\./)
  assert.equal(hello.value.shell, '/bin/bash')

  const opened = await envelope('open', { cols: 90, rows: 25 })
  assert.equal(opened.ok, true)
  const id = opened.value.session.id
  assert.equal(opened.value.session.cols, 90)
  assert.equal(opened.value.session.rows, 25)
  assert.ok(opened.value.session.pid > 0)

  const wrote = await envelope('write', { id, data: 'echo HOST-$((3*14))\n' })
  assert.equal(wrote.ok, true)
  assert.ok(wrote.value.wrote > 0)

  // `read` long-polls until the shell answers, which is the whole point of the endpoint.
  let cursor = opened.value.page.offset
  let seen = ''
  for (let attempt = 0; attempt < 40 && !seen.includes('HOST-42'); attempt += 1) {
    const page = await envelope('read', { id, offset: cursor, waitMs: 2000 })
    assert.equal(page.ok, true)
    seen += page.value.data
    cursor = page.value.offset
  }
  assert.match(seen, /HOST-42/)

  const resized = await envelope('resize', { id, cols: 120, rows: 40 })
  assert.equal(resized.ok, true)
  assert.equal(resized.value.changed, true)

  const listed = await envelope('list', {})
  assert.equal(listed.value.sessions.length, 1)
  assert.equal(listed.value.sessions[0].cols, 120)

  const diag = await envelope('diag', {})
  assert.equal(diag.ok, true)
  assert.equal(diag.value.config.shell, '/bin/bash')

  const closed = await envelope('close', { id })
  assert.equal(closed.value.closed, true)

  // A missing session must come back as NO_SESSION so the browser can offer a new terminal.
  const missing = await envelope('read', { id, offset: 0, waitMs: 0 })
  assert.equal(missing.ok, false)
  assert.equal(missing.error.code, 'NO_SESSION')

  const unknown = await envelope('nope', {})
  assert.equal(unknown.ok, false)
  assert.equal(unknown.error.code, 'web-tty/unknown-endpoint')

  // Aborting a long poll releases it instead of pinning the request until its deadline.
  const idle = await envelope('open', {})
  const controller = new AbortController()
  const startedAt = Date.now()
  const pending = envelope('read', { id: idle.value.session.id, offset: idle.value.page.offset, waitMs: 20_000 }, controller.signal)
  setTimeout(() => controller.abort(), 50)
  const aborted = await pending
  assert.equal(aborted.ok, true)
  assert.ok(Date.now() - startedAt < 5000, 'the abort ended the hold')
  await envelope('close', { id: idle.value.session.id })

  // The tracer is the only diagnostics channel an operator can read.
  const after = readFileSync(TRACE, 'utf8').slice(before.length)
  const events = after.trim().split('\n').filter(Boolean).map((line) => JSON.parse(line))
  assert.ok(events.some((event) => event.event === 'activated'), 'activation is recorded')
  assert.ok(events.some((event) => event.event === 'open'), 'session creation is recorded')
  assert.ok(events.some((event) => event.event === 'rpc' && event.endpoint === 'hello'), 'browser calls are recorded')

  for (const effect of ctx.effects) await effect.dispose?.()
})

test('host: a session cap of one rejects a second terminal with TOO_MANY', async () => {
  const ctx = createContext()
  plugin.apply(ctx, { maxSessions: 1, traceFile: TRACE })
  const handler = ctx.channels.get('/dshgw-web-tty')
  const first = await handler('open', {})
  assert.equal(first.ok, true)
  const second = await handler('open', {})
  assert.equal(second.ok, false)
  assert.equal(second.error.code, 'TOO_MANY')
  for (const effect of ctx.effects) await effect.dispose?.()
})
