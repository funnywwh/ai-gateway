// The RPC endpoint table for dshgw-workspace-files.
//
// Kept apart from index.js so the wire contract is testable without a cordis tree: the tests drive
// this table directly against a real WorkspaceFiles over a temporary workspace, which is the same
// code path the browser uses.
//
// Every handler answers a plain value; index.js wraps it as `{ok:true,value}` and turns a thrown
// coded error into `{ok:false,error:{code,message,details}}`.

import { clampInt, fail } from './fs-service.js'

/**
 * Build the endpoint table.
 *
 * @param options - the workspace service, the resolved config, and the diagnostics sinks.
 * @returns the table plus a dispatcher that rejects an unknown endpoint.
 */
export function createHandlers({ service, config, trace = () => {}, log = () => {} }) {
  const handlers = {
    hello() {
      return { version: config.version, ...service.hello(), maxWaitMs: 0 }
    },
    list(payload) {
      return service.list(payload?.path, { showHidden: payload?.showHidden === true })
    },
    stat(payload) {
      return service.stat(payload?.path)
    },
    readText(payload) {
      return service.readText(payload?.path, { maxBytes: clampInt(payload?.maxBytes, config.maxTextBytes, 1024, config.maxTextBytes) })
    },
    writeText(payload) {
      const expected = payload?.expectedMtimeMs === undefined || payload?.expectedMtimeMs === null
        ? null
        : Number(payload.expectedMtimeMs)
      return service.writeText(payload?.path, payload?.text, { expectedMtimeMs: expected })
    },
    readChunk(payload) {
      return service.readChunk(payload?.path, { offset: payload?.offset, length: payload?.length })
    },
    writeChunk(payload) {
      return service.writeChunk(payload?.path, { offset: payload?.offset, data: payload?.data })
    },
    mkdir(payload) {
      return service.mkdir(payload?.path, { name: payload?.name ?? null })
    },
    rename(payload) {
      return service.rename(payload?.path, { name: payload?.name })
    },
    remove(payload) {
      return service.remove(payload?.path, { recursive: payload?.recursive === true })
    },
    find(payload) {
      return service.find(payload?.query, { limit: payload?.limit, maxDepth: payload?.maxDepth, showHidden: payload?.showHidden === true })
    },
    diag() {
      return {
        version: config.version,
        pid: process.pid,
        node: process.version,
        pluginDir: config.pluginDir,
        traceFile: config.trace === false ? null : config.traceFile,
        config: {
          root: service.root,
          rootLabel: service.rootLabel,
          readOnly: service.readOnly,
          limits: service.limits,
          trace: config.trace,
        },
        calls: counts,
      }
    },
  }

  /** Per-endpoint call counters: the cheapest way to see what a page is actually asking for. */
  const counts = {}

  /**
   * Route one call.
   * @param endpoint - endpoint name from the wire.
   * @param payload - decoded payload.
   * @param signal - request abort signal (unused: every endpoint answers immediately).
   */
  async function dispatch(endpoint, payload, signal) {
    void signal
    const handler = typeof endpoint === 'string' && Object.hasOwn(handlers, endpoint) ? handlers[endpoint] : undefined
    if (handler === undefined) {
      throw fail('files/unknown-endpoint', `未知的接口 ${JSON.stringify(String(endpoint))}`, { endpoint: String(endpoint) })
    }
    counts[endpoint] = (counts[endpoint] ?? 0) + 1
    const started = Date.now()
    try {
      const value = await handler(payload ?? {}, signal)
      trace('rpc', { endpoint, ms: Date.now() - started })
      return value
    } catch (error) {
      trace('rpc-failed', { endpoint, ms: Date.now() - started, code: error?.code ?? null, message: messageOf(error) })
      log(`workspace-files: ${endpoint} failed: ${messageOf(error)}`)
      throw error
    }
  }

  return { handlers, dispatch }
}

/** Message text of anything thrown. */
export function messageOf(error) {
  return error instanceof Error ? error.message : String(error)
}
