// The RPC endpoint table for dshgw-web-tty.
//
// Kept apart from index.js so the endpoint semantics can be tested directly (see
// test/client.test.mjs, which drives the real browser half against this table and a real PTY)
// without standing up a cordis tree.

/** Clamp a value to an integer inside a range, falling back to a default. */
export function num(value, fallback, min, max) {
  const parsed = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(parsed)) return fallback
  return Math.max(min, Math.min(max, Math.floor(parsed)))
}

/**
 * Build the endpoint table.
 *
 * `read` is the only long-polling endpoint: it holds the request until there is output past the
 * caller's cursor, the child exits, the caller aborts, or `waitMs` elapses. Every other endpoint
 * answers immediately.
 *
 * @param options - the session registry, the resolved config, the node-pty module, and sinks.
 * @returns the endpoint table plus a dispatcher that throws on unknown endpoints.
 */
export function createHandlers({ registry, config, pty, trace = () => {}, log = () => {} }) {
  const handlers = {
    hello() {
      return {
        version: config.version,
        nodePty: typeof pty.version === 'string' ? pty.version : null,
        shell: config.shell,
        args: config.args,
        cwd: config.cwd,
        cwdRoot: config.cwdRoot,
        termName: config.termName,
        maxSessions: config.maxSessions,
        maxWaitMs: config.maxWaitMs,
        maxReadChars: config.maxReadChars,
        sessions: registry.list(),
      }
    },
    open(payload) {
      const session = registry.open({
        cwd: clampedCwd(payload?.cwd, config),
        cols: payload?.cols,
        rows: payload?.rows,
      })
      const page = session.read(0, config.maxReadChars)
      trace('open', { id: session.id, pid: session.pid ?? null, cwd: session.cwd, cols: session.cols, rows: session.rows })
      return { session: session.snapshot(), page }
    },
    async read(payload, signal) {
      const session = registry.expect(payload?.id)
      const offset = Number.isFinite(payload?.offset) ? Number(payload.offset) : undefined
      const waitMs = num(payload?.waitMs, 0, 0, config.maxWaitMs)
      await session.waitForOutput(offset ?? session.buffer.base, waitMs, signal)
      const page = session.read(offset, num(payload?.maxChars, config.maxReadChars, 1, config.maxReadChars))
      if (page.data.length > 0 || page.status !== 'running') {
        trace('read', { id: session.id, bytes: page.data.length, offset: page.offset, reset: page.reset, status: page.status })
      }
      return page
    },
    write(payload) {
      const session = registry.expect(payload?.id)
      const wrote = session.write(payload?.data)
      trace('write', { id: session.id, chars: wrote, head: typeof payload?.data === 'string' ? payload.data.slice(0, 40) : null })
      return { id: session.id, wrote }
    },
    resize(payload) {
      const session = registry.expect(payload?.id)
      const size = session.resize(payload?.cols, payload?.rows)
      trace('resize', { id: session.id, cols: size.cols, rows: size.rows, changed: size.changed })
      return { id: session.id, ...size }
    },
    async close(payload) {
      const id = String(payload?.id ?? '')
      const closed = await registry.close(id)
      trace('close', { id, closed })
      return { id, closed }
    },
    list() {
      return { sessions: registry.list() }
    },
    diag() {
      return {
        version: config.version,
        pid: process.pid,
        node: process.version,
        nodePty: typeof pty.version === 'string' ? pty.version : null,
        pluginDir: config.pluginDir,
        traceFile: config.trace === false ? null : config.traceFile,
        config,
        sessions: registry.list(),
      }
    },
  }

  /**
   * Route one call.
   * @param endpoint - endpoint name from the wire.
   * @param payload - decoded payload.
   * @param signal - request abort signal, forwarded to long-polling reads.
   */
  async function dispatch(endpoint, payload, signal) {
    const handler = typeof endpoint === 'string' && Object.hasOwn(handlers, endpoint) ? handlers[endpoint] : undefined
    if (handler === undefined) {
      throw Object.assign(new Error(`unknown endpoint ${JSON.stringify(String(endpoint))}`), { code: 'web-tty/unknown-endpoint' })
    }
    return await handler(payload, signal)
  }

  void log
  return { handlers, dispatch }
}

/** Keep a requested cwd inside the configured root, when one is configured. */
export function clampedCwd(requested, config) {
  if (typeof requested !== 'string' || !requested.startsWith('/')) return config.cwd
  if (config.cwdRoot === '' || config.cwdRoot === undefined) return requested
  const root = config.cwdRoot.endsWith('/') ? config.cwdRoot : `${config.cwdRoot}/`
  if (requested === config.cwdRoot || requested.startsWith(root)) return requested
  return config.cwd
}
