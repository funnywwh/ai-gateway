// Host half of dshgw-workspace-files: a workspace-scoped file manager for the dsh web GUI.
//
// The browser half (client.js, built by build-client.mjs) lives in the shell's overlay slot and
// speaks exactly one channel, `RPC_CHANNEL`, through `ctx.connection.rpc`:
//
//   POST /dshgw-workspace-files/<endpoint>   { type, rpcId, method, payload }  ->  { rpcId, result }
//
// The channel is the same authenticated, same-origin carrier the shipped ssh/browser-workspace
// plugins use: no extra port, no second authentication story, and no route of its own. File bytes
// travel as bounded base64 chunks in both directions, so a download is a loop of readChunk calls
// and an upload is a loop of writeChunk calls, and neither side ever holds a whole file twice.
//
// Everything is clamped to one root: `config.root` (default: the process working directory, which
// is the tenant's workspace). See fs-service.js for the clamp itself.

import { appendFileSync, mkdirSync, statSync, truncateSync } from 'node:fs'
import { dirname, isAbsolute, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { WorkspaceFiles, clampInt, fail } from './fs-service.js'
import { createHandlers, messageOf } from './rpc-handlers.js'

/** Stable cordis plugin name; also the client package name and the RPC channel root. */
export const name = 'dshgw-workspace-files'

/** The authenticated browser RPC carrier is the only service this plugin needs. */
export const inject = ['connection']

const RPC_CHANNEL = '/dshgw-workspace-files'
const PLUGIN_DIR = dirname(fileURLToPath(import.meta.url))
const VERSION = '0.1.0'
const TRACE_FILE = join(PLUGIN_DIR, 'trace.jsonl')
const TRACE_MAX_BYTES = 262_144

const ok = (value) => ({ ok: true, value })
const failed = (error) => ({
  ok: false,
  error: {
    code: typeof error?.code === 'string' && error.code !== '' ? error.code : 'files/failed',
    message: messageOf(error),
    details: error?.details !== null && typeof error?.details === 'object' ? error.details : {},
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
  const numbers = config.limits !== null && typeof config.limits === 'object' ? config.limits : {}
  return {
    version: VERSION,
    pluginDir: PLUGIN_DIR,
    traceFile: configuredPath(config.traceFile) ?? TRACE_FILE,
    root: typeof config.root === 'string' && config.root !== '' ? config.root : process.cwd(),
    rootLabel: typeof config.rootLabel === 'string' ? config.rootLabel : null,
    readOnly: config.readOnly === true,
    maxTextBytes: clampInt(numbers.maxTextBytes ?? config.maxTextBytes, 2 * 1024 * 1024, 4096, 64 * 1024 * 1024),
    chunkBytes: clampInt(numbers.chunkBytes ?? config.chunkBytes, 1024 * 1024, 16 * 1024, 8 * 1024 * 1024),
    maxListEntries: clampInt(numbers.maxListEntries ?? config.maxListEntries, 2000, 50, 50_000),
    maxUploadBytes: clampInt(numbers.maxUploadBytes ?? config.maxUploadBytes, 512 * 1024 * 1024, 1024, 8 * 1024 * 1024 * 1024),
    maxSearchResults: clampInt(numbers.maxSearchResults ?? config.maxSearchResults, 200, 1, 5000),
    maxSearchDepth: clampInt(numbers.maxSearchDepth ?? config.maxSearchDepth, 6, 1, 32),
    maxSearchVisits: clampInt(numbers.maxSearchVisits ?? config.maxSearchVisits, 20_000, 100, 1_000_000),
    trace: config.trace !== false,
  }
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
      // Never let diagnostics break a file operation.
    }
  }
}

/** The plugin body. */
export async function apply(ctx, rawConfig) {
  const config = readConfig(rawConfig)
  const trace = createTracer(config.trace, config.traceFile)
  const log = (message) => {
    ctx.logger?.info?.(`workspace-files: ${message}`)
    trace('log', { message })
  }

  let service
  try {
    service = await WorkspaceFiles.create({
      root: config.root,
      rootLabel: config.rootLabel,
      readOnly: config.readOnly,
      limits: config,
      log,
    })
  } catch (error) {
    // A root that cannot be resolved is an assembly error, not a per-call 404: fail the row so the
    // loader reports it instead of serving a surface that can never answer.
    ctx.logger?.error?.(`workspace-files: ${messageOf(error)}`)
    trace('root-failed', { root: config.root, message: messageOf(error) })
    throw error
  }

  const { dispatch } = createHandlers({ service, config, trace, log })

  ctx.effect(() => ctx.connection.rpc.handle(RPC_CHANNEL, async (endpoint, payload) => {
    try {
      return ok(await dispatch(endpoint, payload))
    } catch (error) {
      return failed(error)
    }
  }), 'workspace-files: browser file RPC channel')

  trace('activated', {
    version: VERSION,
    node: process.version,
    root: service.root,
    readOnly: service.readOnly,
    limits: service.limits,
    pid: process.pid,
  })
  log(`ready: channel ${RPC_CHANNEL}, root ${service.root}${service.readOnly ? ' (read-only)' : ''}`)
}

/** Exported for the tests: the same coded failure the endpoints throw. */
export { fail }

export default { name, inject, apply }
