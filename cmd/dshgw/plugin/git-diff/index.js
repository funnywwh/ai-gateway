// Host half of dshgw-git-diff: the workspace's git changes, read-only, for the dsh web GUI.
//
// The browser half (client.js, stamped from client.src.js by build-client.mjs) registers one
// conversation View — a 「变更」 tab in the session's main area — and speaks exactly one channel,
// `RPC_CHANNEL`, through `ctx.connection.rpc`:
//
//   POST /dshgw-git-diff/<endpoint>   { type, rpcId, method, payload }  ->  { rpcId, result }
//
// The channel is the same authenticated, same-origin carrier the shipped ssh/browser-workspace
// plugins and dshgw-workspace-files use: no extra port, no second authentication story, no route of
// its own.
//
// Two invariants are worth stating where the wiring is decided:
//
//   * This plugin only ever reads. Every git invocation passes `--no-optional-locks` (and the
//     environment sets `GIT_OPTIONAL_LOCKS=0`), there is no stage/checkout/discard endpoint, and the
//     host tests assert that `.git/index` is byte-identical after a full scan.
//   * Everything is clamped to one root: `config.root` (default: the process working directory,
//     which is the tenant's workspace). Repositories and diff paths are both resolved through
//     realpath and required to stay inside it. See git-service.js.

import { appendFileSync, mkdirSync, statSync, truncateSync } from 'node:fs'
import { dirname, isAbsolute, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * Revision of this host half's own module graph, taken from this module's URL.
 *
 * The row in `$DSH_HOME/cordis.patch.yml` names this file with a `?rev=` suffix, and that suffix is
 * carried into the two imports below — so bumping the row's suffix gives the whole host graph fresh
 * module identities. That matters because dsh's loader imports a row's module through Node's ESM
 * cache and this profile runs with the host-side `hmr` row disabled: without the suffix, editing
 * `git-service.js` and re-applying the patch row restarts the row on top of the PREVIOUS module
 * bodies, and the plugin keeps serving the old code until the whole gateway restarts. The suffix is
 * inert everywhere else — `fileURLToPath` ignores a query, so package discovery and the client
 * bundle still resolve normally.
 */
const REVISION = new URL(import.meta.url).search

const { GitService, clampInt, fail } = await import(`./git-service.js${REVISION}`)
const { createHandlers, messageOf } = await import(`./rpc-handlers.js${REVISION}`)

/** Stable cordis plugin name; also the client package name and the RPC channel root. */
export const name = 'dshgw-git-diff'

/** The authenticated browser RPC carrier is the only service this plugin needs. */
export const inject = ['connection']

const RPC_CHANNEL = '/dshgw-git-diff'
const PLUGIN_DIR = dirname(fileURLToPath(import.meta.url))
const VERSION = '0.1.0'
const TRACE_FILE = join(PLUGIN_DIR, 'trace.jsonl')
const TRACE_MAX_BYTES = 262_144
const CACHE_FILE = join(PLUGIN_DIR, 'cache.json')

const ok = (value) => ({ ok: true, value })
const failed = (error) => ({
  ok: false,
  error: {
    code: typeof error?.code === 'string' && error.code !== '' ? error.code : 'git/failed',
    message: messageOf(error),
    details: error?.details !== null && typeof error?.details === 'object' ? error.details : {},
  },
})

/**
 * An optional absolute path from the row's config, or null.
 *
 * A gateway that installs this plugin once for every tenant (M75) cannot let the trace — or the
 * scan cache, which holds repository paths — land next to the module: one shared file mixes
 * tenants together, and in a root-owned plugin directory it cannot be written at all. The row
 * therefore names per-tenant files, and this plugin keeps writing next to itself only when the row
 * stays silent — the out-of-tree `$DSH_HOME/plugins` shape its README documents.
 */
const configuredPath = (value) => (typeof value === 'string' && isAbsolute(value) ? value : null)

/** Read this plugin's config, ignoring anything malformed rather than failing activation. */
export function readConfig(raw) {
  const config = raw !== null && typeof raw === 'object' ? raw : {}
  const numbers = config.limits !== null && typeof config.limits === 'object' ? config.limits : {}
  const pick = (key, fallback, min, max) => clampInt(numbers[key] ?? config[key], fallback, min, max)
  return {
    version: VERSION,
    pluginDir: PLUGIN_DIR,
    traceFile: configuredPath(config.traceFile) ?? TRACE_FILE,
    cacheFile: configuredPath(config.cacheFile) ?? CACHE_FILE,
    root: typeof config.root === 'string' && config.root !== '' ? config.root : process.cwd(),
    rootLabel: typeof config.rootLabel === 'string' ? config.rootLabel : null,
    repo: typeof config.repo === 'string' && config.repo !== '' ? config.repo : null,
    // Repository discovery: how deep to walk and how many directories to look at. Both are capped
    // because discovery runs over the same network filesystem the scans do.
    maxRepoDepth: pick('maxRepoDepth', 6, 0, 32),
    maxRepoCandidates: pick('maxRepoCandidates', 400, 1, 100_000),
    // Scanning: one chunk at a time, each with its own budget, so a pathological directory is
    // marked and skipped instead of sinking the run.
    chunkTimeoutMs: pick('chunkTimeoutMs', 90_000, 1000, 60 * 60 * 1000),
    // Roughly how many tracked files one chunk may cover. Every chunk is one git invocation, and
    // each of those re-reads the whole index over the network, so packing small directories together
    // is what keeps a full scan from spending most of its time re-reading the index; 0 disables
    // packing and scans one directory per chunk.
    chunkTargetFiles: pick('chunkTargetFiles', 25_000, 0, 5_000_000),
    diffTimeoutMs: pick('diffTimeoutMs', 60_000, 1000, 60 * 60 * 1000),
    // Untracked files are off by default: the tenant's AOSP checkout has an un-ignored `out/` tree
    // holding millions of build outputs, and enumerating it is exactly the crawl this design avoids.
    untrackedDefault: config.untracked === true,
    // Whether opening the View starts the background scan by itself. The scan is cancellable and
    // incremental, so the useful default is yes; `autoScan: false` makes the tab opt-in.
    autoScan: config.autoScan !== false,
    maxUntrackedEntries: pick('maxUntrackedEntries', 2000, 1, 100_000),
    maxFiles: pick('maxFiles', 20_000, 1, 500_000),
    maxDiffBytes: pick('maxDiffBytes', 2 * 1024 * 1024, 4096, 64 * 1024 * 1024),
    trace: config.trace !== false,
  }
}

/**
 * Append-only JSONL breadcrumb next to this plugin.
 *
 * The tenant's own stdout belongs to the gateway, so a file is the only place a person can look
 * after a refresh to see what the two halves did — and, for this plugin, how long each chunk took.
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
      // Never let diagnostics break a git call.
    }
  }
}

/** The plugin body. */
export async function apply(ctx, rawConfig) {
  const config = readConfig(rawConfig)
  const trace = createTracer(config.trace, config.traceFile)
  const log = (message) => {
    ctx.logger?.info?.(`git-diff: ${message}`)
    trace('log', { message })
  }

  let service
  try {
    service = await GitService.create({
      root: config.root,
      rootLabel: config.rootLabel,
      repo: config.repo,
      limits: config,
      cacheFile: config.cacheFile,
      log,
      trace,
    })
  } catch (error) {
    // A root that cannot be resolved is an assembly error, not a per-call 404: fail the row so the
    // loader reports it instead of serving a surface that can never answer.
    ctx.logger?.error?.(`git-diff: ${messageOf(error)}`)
    trace('root-failed', { root: config.root, message: messageOf(error) })
    throw error
  }

  const { dispatch } = createHandlers({ service, config, trace, log, fail, messageOf })

  ctx.effect(() => ctx.connection.rpc.handle(RPC_CHANNEL, async (endpoint, payload) => {
    try {
      return ok(await dispatch(endpoint, payload))
    } catch (error) {
      return failed(error)
    }
  }), 'git-diff: browser git RPC channel')

  // A row reload or a plugin reload must not leave a git process scanning a 26 GiB checkout.
  ctx.effect(() => () => {
    service.cancelScan({ jobId: null })
    trace('disposed', {})
  }, 'git-diff: cancel a running scan on teardown')

  trace('activated', {
    version: VERSION,
    node: process.version,
    root: service.root,
    git: service.gitVersion,
    // The settings that actually change what the panel does, so the trace says which build and which
    // tuning is live after a patch reload.
    chunkTargetFiles: config.chunkTargetFiles,
    chunkTimeoutMs: config.chunkTimeoutMs,
    autoScan: config.autoScan,
    pinnedRepo: service.pinnedRepo?.rel ?? null,
    cachedRepos: [...service.results.keys()].map((path) => path.replace(`${service.root}/`, '')),
    pid: process.pid,
  })
  log(`ready: channel ${RPC_CHANNEL}, root ${service.root}, git ${service.gitVersion ?? '不可用'}`)
}

/** Exported for the tests: the same coded failure the endpoints throw. */
export { fail }

export default { name, inject, apply }

// (hmr probe 1790058778)
