// Terminal session core for the dshgw-web-tty plugin.
//
// This module is deliberately free of any dsh import: it owns one real PTY per session
// (through an injected node-pty-like module) plus a bounded raw-byte scrollback ring, and it
// is exercised directly by test/session.test.mjs. index.js only wires it to the plugin's RPC
// endpoints and to node-pty resolved from the active dsh release.
//
// Why not ctx.terminals: that seam is line-oriented on purpose — it sanitizes output (CSI and
// OSC stripped, \r rewritten) and exposes no resize verb, so a browser terminal cannot render
// a screen from it. A raw PTY is the only thing an xterm.js client can attach to.

/** Default scrollback ring size in UTF-16 code units. */
export const DEFAULT_BUFFER_CHARS = 400_000

/** Smallest accepted scrollback ring size; below this a full-screen program cannot repaint. */
const MIN_BUFFER_CHARS = 4096

/** Extend a slice that would otherwise end between the halves of a surrogate pair. */
function avoidSplitSurrogate(text, from, length, total) {
  const end = from + length
  if (end >= total || length === 0) return length
  const last = text.charCodeAt(end - 1)
  if (last >= 0xd800 && last <= 0xdbff) return length + 1
  return length
}

/**
 * Bounded raw-output ring with absolute offsets.
 *
 * Offsets are client-facing cursors: they count every code unit ever written, so a browser can
 * reconnect after a disconnect, ask for "everything after 12345", and be told when the bytes it
 * missed have already been dropped instead of silently rendering a torn screen.
 */
export class OutputBuffer {
  /**
   * @param maxChars - retained code units; older output is dropped from the front.
   */
  constructor(maxChars = DEFAULT_BUFFER_CHARS) {
    this.maxChars = Math.max(MIN_BUFFER_CHARS, Math.floor(maxChars) || DEFAULT_BUFFER_CHARS)
    this.text = ''
    /** Absolute offset of `text[0]`. */
    this.base = 0
    /** Code units dropped since creation, for diagnostics. */
    this.dropped = 0
  }

  /** Absolute offset one past the last written code unit. */
  get end() {
    return this.base + this.text.length
  }

  /**
   * Append output and trim the front when the ring overflows.
   * @param chunk - raw terminal text as decoded from the PTY.
   */
  push(chunk) {
    if (typeof chunk !== 'string' || chunk.length === 0) return
    this.text += chunk
    if (this.text.length <= this.maxChars) return
    let cut = this.text.length - this.maxChars
    // Never start the retained window on a low surrogate: it would render as garbage forever.
    const first = this.text.charCodeAt(cut)
    if (first >= 0xdc00 && first <= 0xdfff) cut += 1
    this.text = this.text.slice(cut)
    this.base += cut
    this.dropped += cut
  }

  /**
   * Read from an absolute offset.
   * @param offset - absolute cursor; a value below the retained window yields `reset: true`.
   * @param maxChars - page size.
   * @returns the page, the next cursor, and whether the window had already been dropped.
   */
  read(offset, maxChars = 65_536) {
    const requested = Number.isFinite(offset) ? Math.floor(offset) : this.base
    const reset = requested < this.base
    const start = Math.max(requested, this.base)
    const from = start - this.base
    const limit = Math.max(1, Math.floor(maxChars) || 65_536)
    const length = avoidSplitSurrogate(this.text, from, Math.min(limit, this.text.length - from), this.text.length)
    const data = this.text.slice(from, from + length)
    return { data, offset: start + data.length, reset, more: from + data.length < this.text.length }
  }

  /** Discard everything, as `term.reset()` on the client side requires. */
  clear() {
    const end = this.end
    this.text = ''
    this.base = end
  }
}

/**
 * One PTY-backed session.
 *
 * State: `running` until the child exits, then `exited` with `exitCode`. Output arriving after a
 * close is still appended so the browser can read the child's last words.
 */
export class TtySession {
  /**
   * @param options - id, the spawned PTY handle, ring size, and an exit observer.
   */
  constructor({ id, term, bufferChars, onExit, now = () => Date.now() }) {
    this.id = id
    this.term = term
    this.now = now
    this.buffer = new OutputBuffer(bufferChars)
    this.status = 'running'
    this.exitCode = null
    this.exitSignal = null
    this.createdAt = now()
    this.lastActivityAt = this.createdAt
    this.bytesIn = 0
    this.bytesOut = 0
    this.closing = false
    this.waiters = new Set()
    this.disposers = []
    this.onExit = onExit
    this.disposers.push(term.onData((chunk) => {
      this.buffer.push(chunk)
      this.bytesOut += chunk.length
      this.wake()
    }))
    this.disposers.push(term.onExit(({ exitCode, signal }) => {
      this.status = 'exited'
      this.exitCode = Number.isFinite(exitCode) ? exitCode : null
      this.exitSignal = typeof signal === 'number' ? signal : null
      this.wake()
      this.onExit?.(this)
    }))
  }

  /** Columns reported to the PTY. */
  get cols() { return this.term.cols }

  /** Rows reported to the PTY. */
  get rows() { return this.term.rows }

  /** PID of the child process backing this session, when the backend exposes one. */
  get pid() { return this.term.pid }

  /** Wake every long-poll waiter; called whenever the observable state changes. */
  wake() {
    for (const waiter of [...this.waiters]) waiter()
  }

  /**
   * Send raw keyboard bytes to the PTY.
   * @param data - exactly what xterm.js produced (control bytes included).
   */
  write(data) {
    if (typeof data !== 'string' || data === '') return 0
    if (this.status !== 'running') throw new Error(`session ${this.id} has exited`)
    this.term.write(data)
    this.bytesIn += data.length
    this.lastActivityAt = this.now()
    return data.length
  }

  /**
   * Resize the PTY, which delivers SIGWINCH to the foreground group so full-screen programs
   * repaint at the new geometry.
   */
  resize(cols, rows) {
    if (this.status !== 'running') return { cols: this.cols, rows: this.rows, changed: false }
    const nextCols = Math.max(2, Math.min(1000, Math.floor(cols) || 0))
    const nextRows = Math.max(1, Math.min(1000, Math.floor(rows) || 0))
    if (nextCols === this.cols && nextRows === this.rows) return { cols: nextCols, rows: nextRows, changed: false }
    this.term.resize(nextCols, nextRows)
    this.lastActivityAt = this.now()
    return { cols: nextCols, rows: nextRows, changed: true }
  }

  /**
   * Read a page of output.
   * @param offset - absolute cursor from a previous read, or undefined for "from the window start".
   * @param maxChars - page size.
   */
  read(offset, maxChars) {
    const page = this.buffer.read(offset, maxChars)
    this.lastActivityAt = this.now()
    return { ...page, status: this.status, exitCode: this.exitCode, exitSignal: this.exitSignal, cols: this.cols, rows: this.rows }
  }

  /**
   * Resolve as soon as there is output past `offset`, the session exits, the caller aborts, or
   * `waitMs` elapses. This is what makes a single POST a long poll instead of a busy poll.
   * @param offset - absolute cursor the caller already consumed.
   * @param waitMs - upper bound on the hold time.
   * @param signal - aborts when the browser navigates away or the plugin unloads.
   */
  async waitForOutput(offset, waitMs, signal) {
    const ready = () => this.buffer.end > offset || this.status !== 'running'
    const hold = Math.max(0, Math.min(60_000, Math.floor(waitMs) || 0))
    if (hold === 0 || ready() || signal?.aborted === true) return
    await new Promise((resolve) => {
      let timer = null
      const finish = () => {
        this.waiters.delete(finish)
        if (timer !== null) clearTimeout(timer)
        signal?.removeEventListener('abort', finish)
        resolve()
      }
      this.waiters.add(finish)
      timer = setTimeout(finish, hold)
      timer.unref?.()
      signal?.addEventListener('abort', finish, { once: true })
    })
  }

  /**
   * Stop the child. Sends SIGHUP first, then SIGKILL after `graceMs`, and resolves once the
   * process is gone (or the grace period expired).
   */
  async close({ reason = 'closed by client', graceMs = 1500 } = {}) {
    if (this.closing) return false
    this.closing = true
    if (this.status === 'running') {
      try {
        this.term.kill()
      } catch {
        // Killing an already-dead PTY is not an error worth surfacing.
      }
      await new Promise((resolve) => {
        const timer = setTimeout(resolve, Math.max(0, graceMs))
        timer.unref?.()
        const stop = () => { clearTimeout(timer); resolve() }
        this.waiters.add(stop)
        if (this.status !== 'running') stop()
      })
    }
    for (const dispose of this.disposers.splice(0)) {
      try {
        dispose?.dispose?.()
      } catch {
        // Term handles are already gone when this throws.
      }
    }
    this.term = null
    this.closedReason = reason
    this.wake()
    return true
  }

  /** Serializable view for the browser's tab list. */
  snapshot() {
    return {
      id: this.id,
      status: this.status,
      exitCode: this.exitCode,
      pid: this.pid ?? null,
      cols: this.cols,
      rows: this.rows,
      createdAt: this.createdAt,
      lastActivityAt: this.lastActivityAt,
      buffered: this.buffer.text.length,
      dropped: this.buffer.dropped,
      cwd: this.cwd ?? null,
      shell: this.shell ?? null,
    }
  }
}

/**
 * Session table for one plugin instance.
 *
 * Every browser tab gets its own PTY; there is no sharing and no cross-owner transfer, which
 * matches how the deployment runs one person per tenant.
 */
export class TtyRegistry {
  /**
   * @param options - the node-pty module, spawn defaults, an optional logger and clock.
   */
  constructor({ pty, defaults = {}, log = () => {}, now = () => Date.now() }) {
    if (!pty || typeof pty.spawn !== 'function') throw new Error('web-tty: a node-pty compatible module is required')
    this.pty = pty
    this.defaults = defaults
    this.log = log
    this.now = now
    this.sessions = new Map()
    this.counter = 0
    this.disposed = false
  }

  /** True while the registry accepts new sessions. */
  assertUsable() {
    if (this.disposed) throw Object.assign(new Error('web-tty: the terminal service is disposed'), { code: 'DISPOSED' })
  }

  /**
   * Spawn a new shell.
   * @param request - optional per-call overrides (cwd, columns, rows, shell, args, env).
   * @returns the new session's snapshot plus its first output page.
   */
  open(request = {}) {
    this.assertUsable()
    const max = Math.max(1, Math.floor(this.defaults.maxSessions) || 8)
    if (this.sessions.size >= max) {
      // Reclaim an exited session before refusing: a tab that closed without closing its shell
      // must not lock the user out of opening a new one.
      const exited = [...this.sessions.values()].find((session) => session.status !== 'running')
      if (exited === undefined) {
        throw Object.assign(new Error(`web-tty: at most ${max} terminals are open`), { code: 'TOO_MANY' })
      }
      this.sessions.delete(exited.id)
    }
    const cols = Math.max(2, Math.min(1000, Math.floor(request.cols) || this.defaults.cols || 80))
    const rows = Math.max(1, Math.min(1000, Math.floor(request.rows) || this.defaults.rows || 24))
    const shell = typeof request.shell === 'string' && request.shell !== '' ? request.shell : this.defaults.shell
    const args = Array.isArray(request.args) && request.args.length > 0 ? request.args.map(String) : this.defaults.args
    const cwd = typeof request.cwd === 'string' && request.cwd.startsWith('/') ? request.cwd : this.defaults.cwd
    const id = `tty-${++this.counter}`
    const term = this.pty.spawn(shell, args, {
      name: this.defaults.termName ?? 'xterm-256color',
      cols,
      rows,
      cwd,
      env: { ...this.defaults.env, ...(request.env ?? {}) },
    })
    const session = new TtySession({
      id,
      term,
      bufferChars: this.defaults.bufferChars,
      now: this.now,
      onExit: (exited) => this.log(`session ${exited.id} exited code=${exited.exitCode} signal=${exited.exitSignal}`),
    })
    session.cwd = cwd
    session.shell = [shell, ...args].join(' ')
    this.sessions.set(id, session)
    this.log(`session ${id} started pid=${session.pid ?? '?'} ${cols}x${rows} cwd=${cwd}`)
    return session
  }

  /** Look up a live session or throw the code the client maps to "reconnect". */
  expect(id) {
    const session = this.sessions.get(String(id))
    if (session === undefined) throw Object.assign(new Error(`web-tty: no terminal ${JSON.stringify(String(id))}`), { code: 'NO_SESSION' })
    return session
  }

  /** All sessions, oldest first. */
  list() {
    return [...this.sessions.values()].map((session) => session.snapshot())
  }

  /** Close one session and forget it. */
  async close(id) {
    const session = this.sessions.get(String(id))
    if (session === undefined) return false
    this.sessions.delete(session.id)
    await session.close({ reason: 'closed by client' })
    this.log(`session ${session.id} closed`)
    return true
  }

  /** Close everything, used on plugin dispose and on harness shutdown. */
  async closeAll(reason = 'plugin disposed') {
    const sessions = [...this.sessions.values()]
    this.sessions.clear()
    await Promise.all(sessions.map((session) => session.close({ reason }).catch(() => {})))
  }

  /** Mark the registry dead and reap every child. */
  async dispose() {
    this.disposed = true
    await this.closeAll()
  }
}
