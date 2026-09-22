// Tests for the terminal core (tty-session.js) against a real PTY.
//
// node-pty is resolved from the active dsh release exactly the way index.js resolves it, so this
// file also proves that resolution path works from an out-of-tree plugin directory.

import { createRequire } from 'node:module'
import { strict as assert } from 'node:assert'
import test from 'node:test'

import { OutputBuffer, TtyRegistry } from '../tty-session.js'

const ANCHOR = process.env.DSHGW_DSH_ANCHOR ?? process.argv[1]
const pty = createRequire(ANCHOR)('node-pty')

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/** Read until `predicate` matches everything seen so far, or the deadline passes. */
async function readUntil(session, predicate, timeoutMs = 8000) {
  const deadline = Date.now() + timeoutMs
  let cursor = 0
  let seen = ''
  while (Date.now() < deadline) {
    const page = session.read(cursor, 1_000_000)
    seen += page.data
    cursor = page.offset
    if (predicate(seen)) return seen
    if (session.status !== 'running') return seen
    // The shell may still be typing its prompt: a wake with no new bytes just loops again.
    await session.waitForOutput(cursor, Math.min(500, Math.max(10, deadline - Date.now())))
  }
  throw new Error(`timed out waiting for output; saw ${JSON.stringify(seen.slice(-400))}`)
}

/** Read until the child exits, or the deadline passes. */
async function waitForExit(session, timeoutMs = 8000) {
  const deadline = Date.now() + timeoutMs
  let cursor = 0
  while (Date.now() < deadline && session.status === 'running') {
    const page = session.read(cursor, 1_000_000)
    cursor = page.offset
    if (session.status !== 'running') break
    await session.waitForOutput(cursor, Math.min(500, Math.max(10, deadline - Date.now())))
  }
  return session.status
}

function registry(overrides = {}) {
  return new TtyRegistry({
    pty,
    defaults: {
      shell: '/bin/bash',
      args: ['--noprofile', '--norc', '-i'],
      cwd: process.cwd(),
      cols: 80,
      rows: 24,
      termName: 'xterm-256color',
      bufferChars: 4096,
      maxSessions: 4,
      env: { ...process.env, TERM: 'xterm-256color', PS1: 'wtt> ' },
      ...overrides,
    },
  })
}

test('OutputBuffer: absolute offsets survive trimming', () => {
  const buffer = new OutputBuffer(4096)
  buffer.push('a'.repeat(4000))
  assert.equal(buffer.base, 0)
  assert.equal(buffer.end, 4000)
  buffer.push('b'.repeat(200))
  assert.equal(buffer.end, 4200)
  assert.equal(buffer.base, 104, 'the window slides and the base tracks every dropped code unit')
  const page = buffer.read(4100, 100)
  assert.equal(page.reset, false)
  assert.equal(page.data, 'b'.repeat(100))
  assert.equal(page.offset, 4200)
})

test('OutputBuffer: an offset below the window reports reset instead of a torn screen', () => {
  const buffer = new OutputBuffer(4096)
  buffer.push('x'.repeat(9000))
  const page = buffer.read(10, 16)
  assert.equal(page.reset, true)
  assert.equal(page.data.length, 16)
  assert.equal(page.offset, buffer.base + 16)
})

test('OutputBuffer: a cut never starts on a low surrogate', () => {
  const buffer = new OutputBuffer(4096)
  // 2 + 4095 code units overflows the ring by exactly one, which would put the window start on
  // the low half of the emoji; the ring moves one unit further instead.
  buffer.push(`😀${'y'.repeat(4095)}`)
  assert.equal(buffer.base, 2)
  assert.equal(buffer.text.startsWith('y'), true)
  assert.equal(buffer.end, 4097)
})

test('registry: spawns a real PTY, echoes input, and reports geometry', async () => {
  const sessions = registry()
  const session = sessions.open({ cols: 100, rows: 30 })
  try {
    assert.equal(session.status, 'running')
    assert.ok(session.pid > 0)
    assert.equal(session.cols, 100)
    assert.equal(session.rows, 30)
    session.write('echo READY-$((6*7))\n')
    const seen = await readUntil(session, (text) => text.includes('READY-42'))
    assert.match(seen, /READY-42/)
    session.write('stty size\n')
    const size = await readUntil(session, (text) => /\b30 100\b/.test(text))
    assert.match(size, /30 100/)
  } finally {
    await sessions.close(session.id)
  }
})

test('registry: resize reaches the foreground program', async () => {
  const sessions = registry()
  const session = sessions.open({ cols: 80, rows: 24 })
  try {
    session.write('echo START\n')
    await readUntil(session, (text) => text.includes('START'))
    const changed = session.resize(120, 42)
    assert.equal(changed.changed, true)
    assert.equal(changed.cols, 120)
    assert.equal(changed.rows, 42)
    session.write('echo SIZE-$(stty size)\n')
    const seen = await readUntil(session, (text) => text.includes('SIZE-42 120'))
    assert.match(seen, /SIZE-42 120/)
  } finally {
    await sessions.close(session.id)
  }
})

test('session: long poll resolves on output, on exit, and on its own deadline', async () => {
  const sessions = registry()
  const session = sessions.open({ cols: 80, rows: 24 })
  try {
    await readUntil(session, (text) => text.includes('wtt> '))
    const quiet = Date.now()
    session.read(0, 1_000_000)
    const idle = session.buffer.end
    await session.waitForOutput(idle, 150)
    const held = Date.now() - quiet
    assert.ok(held >= 120, `an idle long poll waits for its window (held ${held}ms)`)
    session.write('echo WOKE\n')
    await session.waitForOutput(idle, 5000)
    const woke = await readUntil(session, (text) => text.includes('WOKE'))
    assert.match(woke, /WOKE/)
    session.write('exit 7\n')
    assert.equal(await waitForExit(session), 'exited')
    assert.equal(session.exitCode, 7)
    const page = session.read(0, 1_000_000)
    assert.equal(page.status, 'exited')
    assert.match(page.data, /WOKE/)
  } finally {
    await sessions.close(session.id)
  }
})

test('registry: refuses to exceed the session cap but reclaims exited shells', async () => {
  const sessions = registry({ maxSessions: 1 })
  try {
    const first = sessions.open()
    assert.equal(sessions.list().length, 1)
    assert.throws(() => sessions.open(), /at most 1 terminals/)
    first.write('exit 0\n')
    assert.equal(await waitForExit(first), 'exited')
    const second = sessions.open()
    assert.equal(sessions.list().length, 1)
    assert.notEqual(second.id, first.id)
  } finally {
    await sessions.closeAll()
  }
  assert.equal(sessions.list().length, 0)
})

test('registry: unknown sessions surface as NO_SESSION for the client to reconnect', () => {
  const sessions = registry()
  assert.throws(() => sessions.expect('tty-999'), (error) => error.code === 'NO_SESSION')
})
