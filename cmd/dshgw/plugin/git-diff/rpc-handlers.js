// The RPC endpoint table for dshgw-git-diff.
//
// Kept apart from index.js so the wire contract is testable without a cordis tree: the tests drive
// this table against a real GitService over temporary repositories, which is the same code path the
// browser takes.
//
// Every handler answers a plain value; index.js wraps it as `{ok:true,value}` and turns a thrown
// coded failure into `{ok:false,error:{code,message,details}}`.

import { fail as defaultFail, messageOf as defaultMessageOf } from './git-service.js'

/**
 * Build the endpoint table.
 *
 * @param options - the service, the resolved config, the diagnostics sinks, and optionally the
 *   failure helpers. index.js injects its own copies (its module graph is revision-tagged, see
 *   index.js) so that a coded failure raised here is the same class the service throws; the tests
 *   leave them out and get the plain imports.
 * @returns the table plus a dispatcher that counts, traces and rejects unknown endpoints.
 */
export function createHandlers({
  service, config, trace = () => {}, log = () => {},
  fail = defaultFail, messageOf = defaultMessageOf,
}) {
  /** Resolve the repository every repository-scoped endpoint needs. */
  const repoOf = async (payload) => {
    const target = payload?.repo ?? service.pinnedRepo?.path ?? null
    return await service.resolveRepo(target)
  }

  const handlers = {
    hello() {
      return { ...service.hello(), pinnedRepo: service.pinnedRepo ?? null }
    },

    async repos(payload) {
      const repos = await service.discover({ refresh: payload?.refresh === true })
      return { repos, cachedAt: service.discovery.at }
    },

    async status(payload) {
      return await service.status({ repo: await repoOf(payload) })
    },

    async scanStart(payload) {
      const repo = await repoOf(payload)
      const scopes = Array.isArray(payload?.scopes) ? payload.scopes : null
      return await service.startScan({
        repo,
        scopes,
        includeUntracked: payload?.includeUntracked === true,
        includeStaged: payload?.includeStaged === true,
        force: payload?.force === true,
      })
    },

    scanStatus(payload) {
      const snapshot = service.scanStatus({ jobId: typeof payload?.jobId === 'string' ? payload.jobId : null })
      if (snapshot === null) {
        return { jobId: null, state: 'idle', plan: [], chunksDone: 0, chunksTotal: 0, files: [], total: 0, truncated: false, error: null, elapsedMs: null }
      }
      return snapshot
    },

    scanCancel(payload) {
      const snapshot = service.cancelScan({ jobId: typeof payload?.jobId === 'string' ? payload.jobId : null })
      return snapshot ?? service.scanStatus({ jobId: null }) ?? { jobId: null, state: 'idle', files: [], plan: [] }
    },

    async diff(payload) {
      const repo = await repoOf(payload)
      return await service.diff({
        repo,
        path: payload?.path,
        side: typeof payload?.side === 'string' ? payload.side : 'unstaged',
        context: payload?.context,
      })
    },

    diag() {
      return {
        version: config.version,
        pid: process.pid,
        node: process.version,
        pluginDir: config.pluginDir,
        traceFile: config.trace === false ? null : config.traceFile,
        git: { available: service.gitAvailable, version: service.gitVersion },
        root: service.root,
        pinnedRepo: service.pinnedRepo ?? null,
        repos: service.discovery.repos,
        jobs: [...service.jobs.values()].map((job) => ({ id: job.id, state: job.state, files: job.files.length, chunks: job.plan.length })),
        cachedRepos: [...service.results.keys()],
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
   * @param signal - request abort signal, forwarded to the short-lived endpoints only.
   */
  async function dispatch(endpoint, payload, signal) {
    const handler = typeof endpoint === 'string' && Object.hasOwn(handlers, endpoint) ? handlers[endpoint] : undefined
    if (handler === undefined) {
      throw fail('git/unknown-endpoint', `未知的接口 ${JSON.stringify(String(endpoint))}`, { endpoint: String(endpoint) })
    }
    counts[endpoint] = (counts[endpoint] ?? 0) + 1
    const started = Date.now()
    try {
      const value = await handler(payload ?? {}, signal)
      trace('rpc', { endpoint, ms: Date.now() - started })
      return value
    } catch (error) {
      trace('rpc-failed', { endpoint, ms: Date.now() - started, code: error?.code ?? null, message: messageOf(error) })
      log(`${endpoint} 失败：${messageOf(error)}`)
      throw error
    }
  }

  return { handlers, dispatch }
}

/** Message text of anything thrown; re-exported so the host half has one name for it. */
const messageOfExport = defaultMessageOf
export { messageOfExport as messageOf }
