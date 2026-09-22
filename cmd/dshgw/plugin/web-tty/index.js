// Host half of dshgw-web-tty: a real PTY per browser terminal tab, reached over dsh's own
// authenticated RPC channel.
//
// The browser half (client.js, built by build-client.mjs) lives in the shell's overlay slot and
// speaks exactly one channel, `RPC_CHANNEL`, through `ctx.connection.rpc`:
//
//   POST /dshgw-web-tty/<endpoint>   { type, rpcId, method, payload }  ->  { rpcId, result }
//
// `read` is a long poll: the handler holds the request until there is output past the caller's
// cursor, the child exits, the client aborts, or `waitMs` elapses. That single trick is what
// makes a plain POST-per-call transport feel like a stream — no websocket, no extra port, no
// second authentication story. (A websocket over ctx.webServer.registerUpgrade would buy raw
// frames; it would also need its own requestRejection gate and would depend on the reverse proxy
// forwarding Upgrade, so the long poll stays the smaller, safer contract.)
//
// Everything below is tenant-side. The PTY inherits this process's bubblewrap sandbox, so the
// terminal has exactly the powers the agent's own bash tool has: no more, no less.

import { appendFileSync, mkdirSync, statSync, truncateSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, isAbsolute, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { createHandlers, num } from './rpc-handlers.js'
import { TtyRegistry } from './tty-session.js'

/** Stable cordis plugin name; also the client package name and the RPC channel root. */
export const name = 'dshgw-web-tty'

/** The authenticated browser RPC carrier is the only service this plugin needs. */
export const inject = ['connection']

const RPC_CHANNEL = '/dshgw-web-tty'
const PLUGIN_DIR = dirname(fileURLToPath(import.meta.url))
const VERSION = '0.1.0'
const TRACE_FILE = join(PLUGIN_DIR, 'trace.jsonl')
const TRACE_MAX_BYTES = 262_144

const ok = (value) => ({ ok: true, value })
const messageOf = (error) => (error instanceof Error ? error.message : String(error))
const failed = (error) => ({
  ok: false,
  error: {
    code: typeof error?.code === 'string' && error.code !== '' ? error.code : 'web-tty/failed',
    message: messageOf(error),
    details: {},
  },
})

/**
 * An optional absolute path from the row's config, or null.
 *
 * A gateway that installs this plugin once for every tenant (M75) cannot let the trace land next
 * to the module: one shared file mixes tenants together, and in a root-owned plugin directory it
 * cannot be written at all. The row therefore names a per-tenant file, and this plugin keeps
 * writing next to itself only when the row stays silent — the out-of-tree `$DSH_HOME/plugins`
 * shape its README documents.
 */
const configuredPath = (value) => (typeof value === 'string' && isAbsolute(value) ? value : null)

/** Read this plugin's config, ignoring anything malformed rather than failing activation. */
export function readConfig(raw) {
  const config = raw !== null && typeof raw === 'object' ? raw : {}
  const strings = Array.isArray(config.args) ? config.args.filter((arg) => typeof arg === 'string') : null
  return {
    version: VERSION,
    pluginDir: PLUGIN_DIR,
    traceFile: configuredPath(config.traceFile) ?? TRACE_FILE,
    shell: typeof config.shell === 'string' && config.shell !== '' ? config.shell : '/bin/bash',
    args: strings ?? ['-i'],
    cwd: typeof config.cwd === 'string' && isAbsolute(config.cwd) ? config.cwd : process.cwd(),
    cwdRoot: typeof config.cwdRoot === 'string' && isAbsolute(config.cwdRoot) ? config.cwdRoot : '',
    cols: num(config.cols, 120, 2, 1000),
    rows: num(config.rows, 32, 1, 1000),
    termName: typeof config.termName === 'string' && config.termName !== '' ? config.termName : 'xterm-256color',
    bufferChars: num(config.bufferChars, 400_000, 4096, 8_000_000),
    maxSessions: num(config.maxSessions, 8, 1, 64),
    maxReadChars: num(config.maxReadChars, 262_144, 1024, 2_000_000),
    maxWaitMs: num(config.maxWaitMs, 25_000, 0, 60_000),
    idleTimeoutMs: num(config.idleTimeoutMs, 0, 0, 86_400_000),
    trace: config.trace !== false,
  }
}

/**
 * Resolve `node-pty` from the active dsh release.
 *
 * An out-of-tree `file://` plugin cannot resolve dsh's bare imports from its own directory, so
 * the release anchor is used as the resolution base — the same trick picker-clamp.js uses. The
 * anchor is `DSHGW_DSH_ANCHOR` when the gateway sets it, otherwise the launcher entry point.
 */
function loadNodePty(ctx) {
  const anchors = [process.env.DSHGW_DSH_ANCHOR, process.argv[1]].filter((anchor) => typeof anchor === 'string' && anchor !== '')
  const failures = []
  for (const anchor of anchors) {
    try {
      const requireFromDsh = createRequire(anchor)
      const pty = requireFromDsh('node-pty')
      // node-pty's own module object does not carry its version; read it off the manifest so
      // `hello`/`diag` can report which native build a tenant is actually running.
      if (typeof pty.version !== 'string') {
        try {
          pty.version = requireFromDsh('node-pty/package.json').version
        } catch {
          // A missing manifest only costs the version string in diagnostics.
        }
      }
      return pty
    } catch (error) {
      failures.push(`${anchor}: ${messageOf(error)}`)
    }
  }
  ctx.logger?.error?.(`web-tty: cannot resolve node-pty (${failures.join('; ')})`)
  throw new Error(`web-tty: node-pty is unreachable from the dsh release (${failures.join('; ')})`)
}

/**
 * Append-only JSONL breadcrumb next to this plugin.
 *
 * The tenant's own stdout belongs to the gateway, so a file is the only place a person can look
 * after a refresh to see whether the browser half reached the host half.
 */
function createTracer(enabled, file) {
  if (!enabled) return () => {}
  try {
    // The row's directory is created here rather than by the gateway: this tracer runs as the
    // tenant account, which is the only account that may own a directory under its own DSH home.
    mkdirSync(dirname(file), { recursive: true })
    if ((statSync(file, { throwIfNoEntry: false })?.size ?? 0) > TRACE_MAX_BYTES) truncateSync(file, 0)
  } catch {
    // A missing or unwritable file only costs diagnostics.
  }
  return (event, fields = {}) => {
    try {
      appendFileSync(file, `${JSON.stringify({ ts: new Date().toISOString(), event, ...fields })}\n`)
    } catch {
      // Never let diagnostics break a terminal.
    }
  }
}

/** The plugin body. */
export function apply(ctx, rawConfig) {
  const config = readConfig(rawConfig)
  const trace = createTracer(config.trace, config.traceFile)
  const log = (message) => {
    ctx.logger?.info?.(`web-tty: ${message}`)
    trace('log', { message })
  }
  const pty = loadNodePty(ctx)
  const registry = new TtyRegistry({
    pty,
    defaults: {
      shell: config.shell,
      args: config.args,
      cwd: config.cwd,
      cols: config.cols,
      rows: config.rows,
      termName: config.termName,
      bufferChars: config.bufferChars,
      maxSessions: config.maxSessions,
      env: {
        ...process.env,
        TERM: config.termName,
        COLORTERM: process.env.COLORTERM ?? 'truecolor',
        LANG: process.env.LANG ?? 'C.UTF-8',
        DSH_WEB_TTY: VERSION,
      },
    },
    log,
  })
  const { dispatch } = createHandlers({ registry, config, pty, trace, log })

  ctx.effect(() => ctx.connection.rpc.handle(RPC_CHANNEL, async (endpoint, payload, signal) => {
    const started = Date.now()
    try {
      const value = await dispatch(endpoint, payload, signal)
      trace('rpc', { endpoint, ms: Date.now() - started })
      return ok(value)
    } catch (error) {
      trace('rpc-failed', { endpoint, ms: Date.now() - started, message: messageOf(error) })
      if (!(error?.code === 'NO_SESSION' || error?.code === 'TOO_MANY')) {
        ctx.logger?.warn?.(`web-tty: ${endpoint} failed: ${messageOf(error)}`)
      }
      return failed(error)
    }
  }), 'web-tty: browser terminal RPC channel')

  if (config.idleTimeoutMs > 0) {
    const sweep = setInterval(() => {
      const cutoff = Date.now() - config.idleTimeoutMs
      for (const session of [...registry.sessions.values()]) {
        if (session.lastActivityAt < cutoff && session.status === 'running') {
          log(`session ${session.id} idle for ${config.idleTimeoutMs}ms; closing`)
          void registry.close(session.id)
        }
      }
    }, Math.min(60_000, Math.max(5_000, Math.floor(config.idleTimeoutMs / 4))))
    sweep.unref?.()
    ctx.effect(() => () => clearInterval(sweep), 'web-tty: idle sweep')
  }

  ctx.effect(() => () => { void registry.dispose() }, 'web-tty: terminal reaper')

  trace('activated', {
    version: VERSION,
    node: process.version,
    nodePty: typeof pty.version === 'string' ? pty.version : null,
    cwd: config.cwd,
    shell: config.shell,
    pid: process.pid,
  })
  log(`ready: channel ${RPC_CHANNEL}, shell ${config.shell} ${config.args.join(' ')}, cwd ${config.cwd}`)
}

export default { name, inject, apply }
