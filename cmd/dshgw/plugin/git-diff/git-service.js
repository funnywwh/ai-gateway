// The git domain of dshgw-git-diff: repo discovery, index-level queries, chunked worktree scans,
// and per-file diffs. No dsh imports, so the whole thing runs against temporary repositories in
// the tests exactly as it runs against the tenant's workspace in the GUI.
//
// Two properties shape every function here:
//
//   1. THIS REPOSITORY NEVER GETS WRITTEN. Every invocation passes `--no-optional-locks` and the
//      environment sets `GIT_OPTIONAL_LOCKS=0`, so git neither refreshes nor takes a lock on
//      `.git/index`. The plugin is a reader: no stage, no checkout, no discard. `host.test.mjs`
//      asserts the index's bytes and mtime survive a full scan.
//
//   2. WORKTREE SCANS ARE CHUNKED, because in this deployment the workspace is an sshfs mount of a
//      26 GiB AOSP checkout (805,368 tracked files). Index-only queries answer in ~1.2 s, but
//      comparing the worktree against the index costs one `lstat` per tracked file over the
//      network: 12,018 files took 1.8 s, the 5,021 files under `device/` took over 120 s, and the
//      whole tree did not finish in 300 s. So a scan is a plan of top-level chunks, cheapest
//      first, run one at a time with a per-chunk timeout and cancellable at any point; a chunk
//      that blows its budget is marked and skipped instead of sinking the whole run.

import { spawn } from 'node:child_process'
import { accessSync, constants, existsSync } from 'node:fs'
import { readFile, readdir, realpath, rename, stat, unlink, writeFile } from 'node:fs/promises'
import { basename, isAbsolute, join, relative, resolve, sep } from 'node:path'

/** Our failure shape: a stable code the browser branches on, plus optional structured details. */
export class Failure extends Error {
  constructor(code, message, details = {}) {
    super(message)
    this.name = 'GitFailure'
    this.code = code
    this.details = details
  }
}

/** Throw one coded failure. */
export function fail(code, message, details = {}) {
  return new Failure(code, message, details)
}

/** The failure codes this plugin can answer with; the browser switches on these, never on text. */
export const CODES = {
  unavailable: 'git/unavailable',
  notARepo: 'git/not-a-repo',
  notFound: 'git/not-found',
  outsideRoot: 'git/outside-root',
  unsupportedPath: 'git/unsupported-path',
  tooLarge: 'git/too-large',
  timeout: 'git/timeout',
  scanBusy: 'git/scan-busy',
  noScan: 'git/no-scan',
  unknownEndpoint: 'git/unknown-endpoint',
  failed: 'git/failed',
}

/** Message text of anything thrown. */
export function messageOf(error) {
  return error instanceof Error ? error.message : String(error)
}

/** Clamp one integer option, falling back when the value is malformed. */
export function clampInt(value, fallback, min, max) {
  const number = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(number)) return fallback
  return Math.min(max, Math.max(min, Math.trunc(number)))
}

/**
 * Environment shared by every git call.
 *
 * `GIT_OPTIONAL_LOCKS=0` is the belt to `--no-optional-locks`'s braces, `GIT_TERMINAL_PROMPT=0`
 * keeps a credential prompt from hanging a scan forever, and a stable locale keeps the parsing
 * (status letters, byte counts) independent of the tenant's environment. Paths themselves travel
 * NUL-separated and unquoted, so non-ASCII names survive without `core.quotepath` ever mattering.
 */
function gitEnv() {
  return { ...process.env, GIT_OPTIONAL_LOCKS: '0', GIT_TERMINAL_PROMPT: '0', GIT_PAGER: 'cat', LC_ALL: 'C' }
}

/**
 * Arguments every invocation shares: never write, never re-quote, never launch an external differ.
 *
 * `core.checkStat=minimal` is what makes this plugin's answer TRUE in this deployment. git decides
 * whether a tracked file changed by comparing the stat data it recorded in the index against the
 * worktree — and it includes the device and inode numbers. An sshfs mount does not present the
 * remote's inode numbers (measured here: the index says inode 23068684, sshfs says 929051 for the
 * same file), so every one of the 805,368 tracked files looks stat-dirty: a plain
 * `diff-files --name-status` reported all 12,018 files under `a-ztc/` as modified on a tree that
 * `git show HEAD` proves is clean. `minimal` compares only mtime and size — exactly what git does on
 * a local disk, so the panel agrees with the `git status` the author sees on the machine that owns
 * the checkout. The cost of the setting is git's own normal caveat: a file whose size and mtime are
 * both unchanged is not re-read.
 *
 * `diff.external=` plus `--no-ext-diff` keep a configured external differ from being launched, and
 * `core.fsmonitor=false` keeps a daemon out of a channel that must stay pure I/O.
 */
const BASE_ARGS = [
  '--no-optional-locks',
  '-c', 'core.quotepath=false',
  '-c', 'core.checkStat=minimal',
  '-c', 'core.fsmonitor=false',
  '-c', 'diff.external=',
]

/** How long a signal-killed git process may take to actually die before we stop waiting for it. */
const KILL_GRACE_MS = 5000
/** How long a timed-out git process may take to die before we stop waiting for it. */
const TIMEOUT_GRACE_MS = 10000

/**
 * Run one git command and collect everything it wrote.
 *
 * The timeout is enforced here rather than through `execFile`'s own option because a git process
 * blocked on a hung network filesystem cannot be reaped promptly: we escalate SIGTERM → SIGKILL
 * and then, if the child still has not closed, resolve with `timedOut` anyway so one pathological
 * chunk can never wedge the scan loop that is awaiting it.
 *
 * @param options - working directory, arguments, and the three budgets: time, bytes, cancellation.
 * @returns stdout as a Buffer (paths are bytes, not text), stderr as text, and the exit code.
 */
export function runGit({ cwd, args, timeoutMs = 30_000, maxBytes = 32 * 1024 * 1024, signal = null }) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted === true) {
      reject(fail(CODES.failed, '调用已取消'))
      return
    }
    let child
    try {
      child = spawn('git', [...BASE_ARGS, ...args], {
        cwd,
        windowsHide: true,
        env: gitEnv(),
        stdio: ['ignore', 'pipe', 'pipe'],
      })
    } catch (error) {
      reject(fail(CODES.unavailable, `无法运行 git：${messageOf(error)}`, { cause: messageOf(error) }))
      return
    }
    // stdout is collected here rather than by execFile because these arguments carry raw bytes:
    // `-z` output is NUL-separated with unquoted paths, so it must never be decoded line by line.
    const stdoutChunks = []
    const stderrChunks = []
    let stdoutBytes = 0
    let stderrBytes = 0
    let settled = false
    let timedOut = false
    let killed = false
    let overflow = false
    let timer = null
    let grace = null

    const cleanup = () => {
      if (timer !== null) clearTimeout(timer)
      if (grace !== null) clearTimeout(grace)
      timer = null
      grace = null
      signal?.removeEventListener?.('abort', onAbort)
    }
    const result = (code, killSignal) => ({
      code,
      stdout: Buffer.concat(stdoutChunks),
      stderr: Buffer.concat(stderrChunks).toString('utf8'),
      timedOut,
      overflow,
      killed: timedOut || killed || killSignal !== null,
      aborted: signal?.aborted === true || (killed && !timedOut),
    })
    const settle = (fn) => {
      if (settled) return
      settled = true
      cleanup()
      fn()
    }
    const onAbort = () => {
      killed = true
      child.kill('SIGTERM')
    }
    signal?.addEventListener?.('abort', onAbort, { once: true })

    child.stdout.on('data', (chunk) => {
      if (settled) return
      stdoutBytes += chunk.length
      if (stdoutBytes > maxBytes) {
        // Over budget: keep no more, and stop the command instead of buffering a runaway diff.
        overflow = true
        child.kill('SIGKILL')
        return
      }
      stdoutChunks.push(chunk)
    })
    child.stderr.on('data', (chunk) => {
      if (stderrBytes > 64 * 1024) return
      stderrBytes += chunk.length
      stderrChunks.push(chunk)
    })

    timer = setTimeout(() => {
      timedOut = true
      child.kill('SIGTERM')
      grace = setTimeout(() => {
        child.kill('SIGKILL')
        // Last resort: stop waiting for a child stuck in an uninterruptible read, so one hung chunk
        // cannot wedge the scan loop awaiting it.
        settle(() => resolve(result(null, null)))
      }, TIMEOUT_GRACE_MS)
    }, timeoutMs)

    child.on('error', (error) => {
      settle(() => reject(fail(CODES.unavailable, `无法运行 git：${messageOf(error)}`, { cause: messageOf(error) })))
    })
    child.on('close', (code, killSignal) => {
      settle(() => resolve(result(code, killSignal ?? null)))
    })
  })
}

/**
 * Run git and insist on a successful exit.
 *
 * `allowExit` exists for `git diff --no-index`, which reports "differences found" as exit 1 — the
 * normal case for an untracked file, not an error.
 */
async function git(options) {
  const { allowExit = [0], label = '' } = options
  const result = await runGit(options)
  if (result.aborted) throw fail(CODES.failed, '调用已取消', { aborted: true })
  if (result.timedOut) {
    throw fail(CODES.timeout, `git ${label} 超时（${Math.round(options.timeoutMs / 1000)} 秒）`, { timeoutMs: options.timeoutMs ?? null })
  }
  if (result.overflow === true) {
    throw fail(CODES.tooLarge, `git ${label} 的输出超过字节上限`, { partial: result.stdout.toString('utf8') })
  }
  if (result.code === null) throw fail(CODES.failed, `git ${label} 被信号终止`)
  if (!allowExit.includes(result.code)) {
    const stderr = result.stderr.trim().split('\n').at(-1) ?? ''
    throw fail(CODES.failed, `git ${label} 失败（退出码 ${result.code}）${stderr === '' ? '' : `：${stderr}`}`, {
      exitCode: result.code, stderr: result.stderr.slice(0, 4096),
    })
  }
  return result
}

/** True when `candidate` is `root` or lives beneath it. */
export function isInside(root, candidate) {
  if (candidate === root) return true
  return candidate.startsWith(root.endsWith(sep) ? root : `${root}${sep}`)
}

/** Split a NUL-delimited byte stream into strings, dropping the trailing empty field. */
function splitNul(buffer) {
  const text = buffer.toString('utf8')
  const parts = text.split('\0')
  if (parts.length > 0 && parts.at(-1) === '') parts.pop()
  return parts
}

/**
 * Parse `--name-status -z` output into `{status, path}` records.
 *
 * With `-z` each record is `status\0path\0`; rename detection is off everywhere in this plugin, so
 * a two-path record can never appear — but a defensive reader keeps a future flag change from
 * silently mis-pairing statuses with paths.
 */
export function parseNameStatus(buffer) {
  const fields = splitNul(buffer)
  const records = []
  let index = 0
  while (index < fields.length) {
    const status = fields[index]
    index += 1
    if (status === '') continue
    const letter = status.slice(0, 1)
    const path = fields[index] ?? ''
    index += 1
    if (letter === 'R' || letter === 'C') {
      // Rename/copy: consume the second path and report both ends.
      const target = fields[index] ?? ''
      index += 1
      records.push({ status: letter, path: target, from: path })
      continue
    }
    if (path === '') continue
    records.push({ status: letter, path })
  }
  return records
}

/** Status letter → a short human label used by the panel. */
export function statusLabel(letter) {
  switch (letter) {
    case 'M': return '修改'
    case 'A': return '新增'
    case 'D': return '删除'
    case 'T': return '类型变更'
    case '?': return '未跟踪'
    case 'R': return '重命名'
    case 'C': return '复制'
    case 'U': return '冲突'
    default: return letter === '' ? '变更' : String(letter)
  }
}

/**
 * Count tracked files per repository-root segment, by streaming `git ls-files -z`.
 *
 * The result orders the scan plan cheapest-first, which is what makes results appear quickly: the
 * small directories answer in a second while `prebuilts/` and `external/` (240k and 208k tracked
 * files) wait at the end of the queue and can be skipped by cancelling. Streaming keeps memory at
 * one pipe chunk even for 805,368 entries, and the byte budget bounds a repository we did not
 * expect.
 *
 * `rootEntries` — the names of tracked files that live directly at the repository root — matters as
 * much as the counts: the index, not the directory listing, is the authority on what exists, so a
 * root-level file whose deletion has not been staged is still a path the scan has to ask about.
 *
 * @returns `{counts, rootEntries}`: per-segment tracked-file counts, and the root-level names.
 */
export async function countTopLevelTracked(cwd, { timeoutMs = 60_000, bytesLimit = 192 * 1024 * 1024, signal = null, extraArgs = [] } = {}) {
  return new Promise((resolvePromise, rejectPromise) => {
    const child = spawn('git', [...BASE_ARGS, ...extraArgs, 'ls-files', '-z'], {
      cwd, env: gitEnv(), stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true,
    })
    const counts = new Map()
    const rootEntries = new Set()
    let pending = Buffer.alloc(0)
    let bytes = 0
    let settled = false
    let timer = null

    const summary = () => ({ counts, rootEntries })
    const finish = (fn, ...args) => {
      if (settled) return
      settled = true
      if (timer !== null) clearTimeout(timer)
      signal?.removeEventListener?.('abort', onAbort)
      fn(...args)
    }
    const onAbort = () => { child.kill('SIGTERM') }
    signal?.addEventListener?.('abort', onAbort, { once: true })

    timer = setTimeout(() => {
      child.kill('SIGKILL')
      finish(rejectPromise, fail(CODES.timeout, '统计受控文件超时'))
    }, timeoutMs)

    child.stdout.on('data', (chunk) => {
      if (settled) return
      bytes += chunk.length
      if (bytes > bytesLimit) {
        child.kill('SIGKILL')
        finish(resolvePromise, summary())
        return
      }
      pending = pending.length === 0 ? chunk : Buffer.concat([pending, chunk])
      let start = 0
      for (;;) {
        const end = pending.indexOf(0, start)
        if (end === -1) break
        const name = pending.subarray(start, end).toString('utf8')
        const cut = name.indexOf('/')
        if (cut === -1) {
          counts.set('', (counts.get('') ?? 0) + 1)
          rootEntries.add(name)
        } else {
          const key = name.slice(0, cut)
          counts.set(key, (counts.get(key) ?? 0) + 1)
        }
        start = end + 1
      }
      pending = pending.subarray(start)
      // A single path longer than this does not exist; keep the tail so the parser stays bounded.
      if (pending.length > 4 * 1024 * 1024) pending = pending.subarray(pending.length - 4 * 1024 * 1024)
    })
    child.on('error', (error) => finish(rejectPromise, fail(CODES.unavailable, `无法运行 git：${messageOf(error)}`)))
    child.on('close', () => finish(resolvePromise, summary()))
  })
}

/**
 * The per-repository arguments every repo-scoped call prepends.
 *
 * `safe.directory` is not a security relaxation here, it is the opposite: the path is one this
 * plugin already resolved through `realpath` and checked against `config.root`, so the exemption is
 * exactly as wide as the clamp. Without it git refuses every read in this deployment, because the
 * tenant's workspace arrives over mounts whose uid does not match the process — the gateway's
 * `browser/` mount is owned by `nobody` — and it fails with "detected dubious ownership" before a
 * single object is read. The exemption is never a wildcard and never a path the caller chose
 * directly.
 */
export function ownershipArgs(repo) {
  return ['-c', `safe.directory=${repo.path}`]
}

/**
 * Pack the cheapest-first plan into chunks of roughly `target` tracked files each.
 *
 * Every chunk costs one git invocation, and each of those re-reads the whole index — 118 MB here,
 * over sshfs — before it looks at a single file. Measured on this repository, a 31-chunk plan spent
 * about 108 s of its 130 s in that repeated index read, while the same work in one pass took 22 s.
 * Packing the small directories together (they are the ones that report first, so the top of the
 * list still fills in early) buys most of that back without giving up incremental results,
 * cancellation, or the per-chunk timeout.
 *
 * A directory larger than the target is never packed with anything: `prebuilts` (240,939 tracked
 * files) and `external` (207,787) stay chunks of their own, which is also what keeps one pathological
 * directory from dragging its neighbours past the timeout.
 */
function packChunks(chunks, target, maxMembers = 12) {
  if (!(target > 0)) return chunks
  const packed = []
  let group = null
  for (const chunk of chunks) {
    if (group !== null && (group.fileCount + chunk.fileCount <= target) && group.paths.length + chunk.paths.length <= maxMembers * 4) {
      group.paths.push(...chunk.paths)
      group.fileCount += chunk.fileCount
      group.members.push(chunk.label)
      continue
    }
    group = { paths: [...chunk.paths], fileCount: chunk.fileCount, members: [chunk.label], state: 'pending', count: 0, ms: 0, error: null }
    packed.push(group)
  }
  return packed.map((chunk) => ({
    ...chunk,
    label: chunk.members.length === 1 ? chunk.members[0] : `${chunk.members[0]} 等 ${chunk.members.length} 个目录`,
    members: chunk.members,
  }))
}

/**
 * One repository, resolved and clamped.
 *
 * `writable` is reported because a workspace can hold several views of the same tree: this
 * deployment mounts the tenant's checkout twice, once over ssh (writable, the tree the session
 * actually works in) and once through the gateway's read-only `browser/` mount. Reviewing changes
 * in the tree you can edit is the sane default, so a writable work tree sorts first.
 */
function repoDescriptor(root, path) {
  const rel = relative(root, path)
  let writable = false
  try {
    accessSync(path, constants.W_OK)
    writable = true
  } catch {
    writable = false
  }
  return { path, rel: rel === '' ? '.' : rel, label: basename(path), writable }
}

/**
 * Count added/removed lines in a unified diff without a full parser.
 *
 * The panel's own parser owns rendering; these counts only label the list rows and the diff header,
 * so a line-level scan is enough and keeps them out of the browser's critical path.
 */
export function diffStats(text) {
  let additions = 0
  let deletions = 0
  for (const line of text.split('\n')) {
    if (line.startsWith('+++') || line.startsWith('---')) continue
    if (line.startsWith('+')) additions += 1
    else if (line.startsWith('-')) deletions += 1
  }
  return { additions, deletions }
}

/** True when the diff text says the two sides are not text at all. */
export function looksBinaryDiff(text) {
  return /(^|\n)Binary files .* differ\n?$/.test(text) || /(^|\n)GIT binary patch\n?$/.test(text)
}

/** What a scan job knows right now, in the shape the browser renders. */
function jobSnapshot(job) {
  return {
    jobId: job.id,
    repo: job.repo,
    state: job.state,
    startedAt: job.startedAt,
    finishedAt: job.finishedAt,
    elapsedMs: (job.finishedAt ?? Date.now()) - job.startedAt,
    includeUntracked: job.includeUntracked,
    includeStaged: job.includeStaged,
    plan: job.plan.map((chunk) => ({ path: chunk.label, fileCount: chunk.fileCount, state: chunk.state, count: chunk.count, ms: chunk.ms, error: chunk.error ?? null })),
    chunksDone: job.plan.filter((chunk) => chunk.state !== 'pending' && chunk.state !== 'running').length,
    chunksTotal: job.plan.length,
    files: job.files,
    total: job.files.length,
    truncated: job.truncated,
    error: job.error,
  }
}

/**
 * The service behind every endpoint.
 *
 * State is deliberately tiny: a discovery cache, a per-repo metadata cache, one running-or-finished
 * scan job per repository, and the last completed scan of each repository (also written to
 * `cache.json`, so a browser refresh — or a server restart — still paints a list immediately).
 */
export class GitService {
  constructor(options) {
    this.root = options.root
    this.rootLabel = options.rootLabel
    this.limits = options.limits
    this.log = options.log ?? (() => {})
    this.trace = options.trace ?? (() => {})
    this.cacheFile = options.cacheFile ?? null
    this.gitVersion = null
    this.gitAvailable = false
    this.repoCache = new Map()
    this.metaCache = new Map()
    this.discovery = { at: 0, repos: null }
    this.jobs = new Map()
    this.results = new Map()
    this.jobCounter = 0
  }

  /**
   * Resolve the root, detect git, and load the previous scan cache.
   *
   * A root that cannot be resolved is an assembly error and fails activation loudly, exactly like
   * the workspace-files plugin: a surface that can never answer should not pretend to serve.
   */
  static async create(options) {
    let root
    try {
      root = await realpath(options.root)
    } catch (error) {
      throw fail(CODES.notFound, `工作区根目录不可用：${messageOf(error)}`, { root: options.root })
    }
    const service = new GitService({ ...options, root })
    try {
      const result = await runGit({ cwd: root, args: ['--version'], timeoutMs: 10_000, maxBytes: 64 * 1024 })
      if (result.code === 0) {
        service.gitAvailable = true
        service.gitVersion = result.stdout.toString('utf8').trim()
      }
    } catch (error) {
      service.log(`git 不可用：${messageOf(error)}`)
    }
    if (options.repo !== null && options.repo !== undefined && options.repo !== '') {
      service.pinnedRepo = await service.resolveRepo(options.repo)
    }
    await service.loadCache()
    return service
  }

  /** The numbers the browser adopts: it never asks for more than these. */
  hello() {
    return {
      version: this.limits.version,
      root: this.root,
      rootLabel: this.rootLabel,
      git: { available: this.gitAvailable, version: this.gitVersion },
      repo: this.pinnedRepo ?? null,
      untracked: this.limits.untrackedDefault === true,
      autoScan: this.limits.autoScan !== false,
      limits: {
        maxFiles: this.limits.maxFiles,
        maxDiffBytes: this.limits.maxDiffBytes,
        chunkTimeoutMs: this.limits.chunkTimeoutMs,
        chunkTargetFiles: this.limits.chunkTargetFiles,
        maxRepoDepth: this.limits.maxRepoDepth,
      },
    }
  }

  /**
   * Resolve one repository path and clamp it to the root.
   *
   * The clamp is the security boundary of this plugin: a repository — and therefore every diff it
   * can serve — must live under `config.root`, so the panel can never read a git object from
   * outside the tenant's workspace, however the request was phrased.
   */
  async resolveRepo(input) {
    if (typeof input !== 'string' || input.trim() === '') {
      throw fail(CODES.notARepo, '没有指定仓库路径', { repo: input ?? null })
    }
    const raw = input.trim()
    if (raw.includes('\0')) throw fail(CODES.unsupportedPath, '仓库路径不合法')
    const absolute = isAbsolute(raw) ? resolve(raw) : resolve(this.root, raw)
    let real
    try {
      real = await realpath(absolute)
    } catch (error) {
      throw fail(CODES.notFound, `仓库路径不存在：${raw}`, { repo: raw, cause: messageOf(error) })
    }
    if (!isInside(this.root, real)) {
      throw fail(CODES.outsideRoot, '仓库不在工作区内', { repo: real, root: this.root })
    }
    if (!existsSync(join(real, '.git'))) {
      throw fail(CODES.notARepo, `${raw} 不是 git 仓库`, { repo: real })
    }
    const cached = this.repoCache.get(real)
    if (cached !== undefined) return cached
    const descriptor = repoDescriptor(this.root, real)
    this.repoCache.set(real, descriptor)
    return descriptor
  }

  /**
   * Discover git work trees under the root, breadth-first, newest-and-shallowest first.
   *
   * The walk stops at a repository instead of descending into it: this is what keeps discovery
   * cheap next to a 26 GiB checkout, because the moment `.git` is seen the whole AOSP tree behind
   * it — hundreds of thousands of directories — is out of scope. Dot-directories and
   * `node_modules`/`out` are skipped for the same reason, and both the depth and the visited
   * count are capped so an unexpected workspace cannot turn discovery into a crawl.
   */
  async discover({ refresh = false } = {}) {
    const now = Date.now()
    if (!refresh && this.discovery.repos !== null && now - this.discovery.at < 30_000) return this.discovery.repos
    if (this.pinnedRepo !== undefined) {
      const repos = [this.pinnedRepo]
      this.discovery = { at: now, repos }
      return repos
    }
    const repos = []
    const found = new Set()
    const queue = [{ dir: this.root, depth: 0 }]
    let visited = 0
    while (queue.length > 0 && visited < this.limits.maxRepoCandidates) {
      const { dir, depth } = queue.shift()
      visited += 1
      let entries
      try {
        entries = await readdir(dir, { withFileTypes: true })
      } catch {
        continue
      }
      if (entries.some((entry) => entry.name === '.git')) {
        if (!found.has(dir)) {
          found.add(dir)
          repos.push(repoDescriptor(this.root, dir))
        }
        continue
      }
      if (depth >= this.limits.maxRepoDepth) continue
      for (const entry of entries) {
        if (!entry.isDirectory()) continue
        if (entry.name.startsWith('.')) continue
        if (entry.name === 'node_modules' || entry.name === 'out') continue
        queue.push({ dir: join(dir, entry.name), depth: depth + 1 })
      }
    }
    repos.sort((left, right) => (
      Number(right.writable) - Number(left.writable)
      || left.rel.length - right.rel.length
      || left.rel.localeCompare(right.rel)
    ))
    this.discovery = { at: now, repos }
    this.trace('discover', { root: this.root, repos: repos.map((repo) => repo.rel), visited })
    return repos
  }

  /** Branch/HEAD facts for one repository, cached for a few seconds. */
  async metadata(repo) {
    const cached = this.metaCache.get(repo.path)
    if (cached !== undefined && Date.now() - cached.at < 5_000) return cached.value
    const options = { cwd: repo.path, timeoutMs: 20_000, maxBytes: 4 * 1024 * 1024 }
    const meta = { head: null, short: null, subject: null, author: null, date: null, branch: null, detached: false, unborn: false }
    const branch = await runGit({ ...options, args: [...ownershipArgs(repo), 'rev-parse', '--abbrev-ref', 'HEAD'] })
    if (branch.code === 0) meta.branch = branch.stdout.toString('utf8').trim()
    const head = await runGit({ ...options, args: [...ownershipArgs(repo), 'rev-parse', 'HEAD'] })
    if (head.code !== 0) {
      meta.unborn = true
    } else {
      meta.head = head.stdout.toString('utf8').trim()
      meta.short = meta.head.slice(0, 10)
      const log = await runGit({ ...options, args: [...ownershipArgs(repo), 'log', '-1', '--format=%s%x00%an%x00%ad', '--date=short'] })
      if (log.code === 0) {
        const [subject, author, date] = splitNul(log.stdout)
        meta.subject = subject ?? null
        meta.author = author ?? null
        meta.date = date ?? null
      }
    }
    if (meta.branch === 'HEAD') meta.detached = true
    this.metaCache.set(repo.path, { at: Date.now(), value: meta })
    return meta
  }

  /**
   * The index-level change set: what is staged relative to HEAD.
   *
   * This is the fast half of the panel — one index read, no worktree access — so it is what the
   * tab paints before any scan finishes. An unborn HEAD (a repository without commits) has no tree
   * to compare against, so every cached path counts as newly added instead.
   */
  async stagedChanges(repo) {
    const options = { cwd: repo.path, timeoutMs: 60_000, maxBytes: 64 * 1024 * 1024 }
    const result = await runGit({
      ...options,
      args: [...ownershipArgs(repo), 'diff-index', '--cached', '--name-status', '-z', '--no-renames', 'HEAD', '--'],
    })
    if (result.code === 0) return parseNameStatus(result.stdout).map((record) => ({ ...record, side: 'staged' }))
    const listed = await git({ ...options, args: [...ownershipArgs(repo), 'ls-files', '--cached', '-z'], label: 'ls-files' })
    return parseNameStatus(Buffer.from(splitNul(listed.stdout).map((path) => `A\0${path}`).join('\0'), 'utf8'))
      .map((record) => ({ ...record, side: 'staged' }))
  }

  /** The worktree change set of one chunk (and optionally its untracked files). */
  async scanChunk(repo, chunk, includeUntracked, signal) {
    const records = []
    const result = await runGit({
      cwd: repo.path,
      args: [...ownershipArgs(repo), 'diff-files', '--name-status', '-z', '--no-renames', '--', ...chunk.paths],
      timeoutMs: this.limits.chunkTimeoutMs,
      maxBytes: 32 * 1024 * 1024,
      signal,
    })
    if (result.aborted) throw fail(CODES.failed, '调用已取消', { aborted: true })
    if (result.timedOut) throw fail(CODES.timeout, `扫描 ${chunk.label} 超时`)
    if (result.overflow === true) throw fail(CODES.tooLarge, `扫描 ${chunk.label} 的输出超过字节上限`)
    if (result.code !== 0) {
      throw fail(CODES.failed, `扫描 ${chunk.label} 失败：${result.stderr.trim().split('\n').at(-1) ?? `退出码 ${result.code}`}`)
    }
    for (const record of parseNameStatus(result.stdout)) records.push({ ...record, side: 'unstaged' })

    if (includeUntracked) {
      const untracked = await runGit({
        cwd: repo.path,
        args: [...ownershipArgs(repo), 'ls-files', '--others', '--exclude-standard', '-z', '--', ...chunk.paths],
        timeoutMs: this.limits.chunkTimeoutMs,
        maxBytes: 32 * 1024 * 1024,
        signal,
      })
      if (untracked.aborted) throw fail(CODES.failed, '调用已取消', { aborted: true })
      if (untracked.timedOut) throw fail(CODES.timeout, `扫描 ${chunk.label} 的未跟踪文件超时`)
      if (untracked.code === 0) {
        let added = 0
        for (const path of splitNul(untracked.stdout)) {
          if (added >= this.limits.maxUntrackedEntries) { records.truncated = true; break }
          added += 1
          records.push({ path, status: '?', side: 'untracked' })
        }
      }
    }
    return records
  }

  /**
   * Build the scan plan: one chunk per top-level directory, plus one for the paths at the root.
   *
   * The plan is the union of what is on disk and what the index tracks, because the two disagree
   * exactly where it matters: a file deleted from the worktree is gone from the directory listing
   * but still in the index, and a pathspec list built from the listing alone would never ask git
   * about it — which is how a deletion goes missing from the list. Directory chunks are whole
   * prefixes, so deletions inside them are covered either way; the root chunk is the one that needs
   * the index's own names.
   *
   * Skipped directory names mirror discovery (`node_modules`, `out`, dot-directories): they hold no
   * tracked content worth a round trip, and `out/` in this workspace is an un-ignored build tree
   * with millions of files that an untracked scan must never be pointed at.
   *
   * Ordering is by ascending tracked-file count so the panel fills in from the cheap end, and a
   * user who only cares about one subtree can start a scoped scan instead of waiting for the whole
   * AOSP checkout.
   */
  async planChunks(repo) {
    const entries = await readdir(repo.path, { withFileTypes: true })
    const directories = []
    const rootFiles = new Set()
    for (const entry of entries) {
      if (entry.name === '.git') continue
      if (entry.isDirectory()) {
        if (entry.name.startsWith('.') || entry.name === 'node_modules' || entry.name === 'out') continue
        directories.push(entry.name)
      } else if (entry.isFile()) {
        rootFiles.add(entry.name)
      }
    }
    let counts = new Map()
    let trackedRoot = new Set()
    try {
      const summary = await countTopLevelTracked(repo.path, { signal: null, extraArgs: ownershipArgs(repo) })
      counts = summary.counts
      trackedRoot = summary.rootEntries
    } catch (error) {
      this.log(`统计受控文件失败，按名称排序：${messageOf(error)}`)
    }
    const chunks = directories.map((name) => ({
      label: name,
      paths: [name],
      fileCount: counts.get(name) ?? 0,
      state: 'pending',
      count: 0,
      ms: 0,
      error: null,
    }))
    // Tracked root-level names first: a staged-but-deleted file must keep its slot in the plan.
    const rootPaths = [...new Set([...trackedRoot, ...rootFiles])].sort()
    if (rootPaths.length > 0) {
      chunks.push({
        label: '（根目录文件）',
        paths: rootPaths,
        fileCount: counts.get('') ?? rootPaths.length,
        state: 'pending',
        count: 0,
        ms: 0,
        error: null,
      })
    }
    const known = new Set(directories)
    for (const [key, value] of counts) {
      if (key === '' || known.has(key)) continue
      // A tracked path whose top segment is no longer a directory on disk (a removed subtree):
      // keep it as its own chunk rather than silently dropping the deletions inside it.
      chunks.push({ label: key, paths: [key], fileCount: value, state: 'pending', count: 0, ms: 0, error: null })
    }
    chunks.sort((left, right) => left.fileCount - right.fileCount || left.label.localeCompare(right.label))
    const plan = packChunks(chunks, this.limits.chunkTargetFiles)
    this.trace('plan', {
      repo: repo.rel,
      chunks: plan.length,
      directories: chunks.length,
      files: [...counts.values()].reduce((total, value) => total + value, 0),
    })
    return plan
  }

  /**
   * Start (or return) the background scan of one repository.
   *
   * Exactly one job runs at a time: a second request returns the running job rather than racing it,
   * and `force` cancels the current one first. Work is never awaited by the caller — the browser
   * polls `scanStatus` and renders whatever has arrived.
   */
  async startScan({ repo, scopes = null, includeUntracked = false, includeStaged = false, force = false, signal = null }) {
    const key = repo.path
    const running = this.jobs.get(key)
    if (running !== undefined && running.state === 'running') {
      if (!force) return { ...jobSnapshot(running), alreadyRunning: true }
      this.cancelScan({ jobId: running.id })
    }
    let plan
    if (Array.isArray(scopes) && scopes.length > 0) {
      const clean = scopes
        .filter((scope) => typeof scope === 'string' && scope.trim() !== '' && !scope.includes('\0') && !isAbsolute(scope) && !scope.split('/').includes('..'))
        .map((scope) => scope.trim())
      if (clean.length === 0) throw fail(CODES.unsupportedPath, '扫描范围不合法', { scopes })
      plan = clean.map((scope) => ({ label: scope, paths: [scope], fileCount: 0, state: 'pending', count: 0, ms: 0, error: null }))
    } else {
      plan = await this.planChunks(repo)
    }
    if (plan.length === 0) throw fail(CODES.notARepo, '仓库没有可扫描的目录', { repo: repo.rel })
    this.jobCounter += 1
    const job = {
      id: `scan-${this.jobCounter}`,
      repo,
      plan,
      includeUntracked,
      includeStaged,
      scoped: Array.isArray(scopes) && scopes.length > 0,
      state: 'running',
      startedAt: Date.now(),
      finishedAt: null,
      files: [],
      seen: new Set(),
      truncated: false,
      error: null,
      controller: new AbortController(),
    }
    if (signal !== null) signal.addEventListener?.('abort', () => this.cancelScan({ jobId: job.id }), { once: true })
    this.jobs.set(key, job)
    this.trace('scan-start', { repo: repo.rel, job: job.id, chunks: plan.length, untracked: includeUntracked })
    void this.runJob(job).catch((error) => {
      job.state = 'failed'
      job.error = messageOf(error)
      job.finishedAt = Date.now()
      this.log(`扫描失败：${messageOf(error)}`)
    })
    return jobSnapshot(job)
  }

  /** The scan loop: one chunk at a time, each with its own budget and its own failure line. */
  async runJob(job) {
    const started = Date.now()
    let staged = []
    if (job.includeStaged) {
      try {
        staged = await this.stagedChanges(job.repo)
      } catch (error) {
        this.log(`读取暂存变更失败：${messageOf(error)}`)
      }
      for (const record of staged) this.addFile(job, record)
    }
    for (const chunk of job.plan) {
      if (job.state !== 'running') break
      chunk.state = 'running'
      const chunkStarted = Date.now()
      try {
        const records = await this.scanChunk(job.repo, chunk, job.includeUntracked, job.controller.signal)
        for (const record of records) this.addFile(job, record)
        if (records.truncated === true) job.truncated = true
        chunk.state = 'done'
        chunk.count = records.length
      } catch (error) {
        if (job.controller.signal.aborted) {
          chunk.state = 'pending'
          break
        }
        chunk.state = error?.code === CODES.timeout ? 'timeout' : 'failed'
        chunk.error = messageOf(error)
        this.trace('chunk-failed', { chunk: chunk.label, code: error?.code ?? null, message: messageOf(error) })
      }
      chunk.ms = Date.now() - chunkStarted
    }
    const aborted = job.controller.signal.aborted
    if (job.state === 'running') job.state = aborted ? 'cancelled' : 'done'
    job.finishedAt = Date.now()
    if (job.state === 'done') {
      this.results.set(job.repo.path, {
        completedAt: job.finishedAt,
        files: job.files,
        plan: job.plan.map((chunk) => ({ path: chunk.label, state: chunk.state, count: chunk.count, ms: chunk.ms, error: chunk.error ?? null })),
        truncated: job.truncated,
        includeUntracked: job.includeUntracked,
        scoped: job.scoped === true,
        slowChunks: job.plan.filter((chunk) => chunk.state === 'timeout' || chunk.state === 'failed').map((chunk) => chunk.label),
        elapsedMs: job.finishedAt - started,
      })
      await this.saveCache()
    }
    this.metaCache.delete(job.repo.path)
    this.trace('scan-end', { repo: job.repo.rel, state: job.state, files: job.files.length, ms: job.finishedAt - started })
  }

  /** Add one record, keeping the first sighting of each (side, path) and respecting the cap. */
  addFile(job, record) {
    const key = `${record.side}\0${record.path}`
    if (job.seen.has(key)) return
    if (job.files.length >= this.limits.maxFiles) {
      job.truncated = true
      return
    }
    job.seen.add(key)
    job.files.push({ path: record.path, status: record.status, side: record.side })
  }

  /** Snapshot of the current job (or the last finished one) for one repository. */
  scanStatus({ jobId = null } = {}) {
    if (jobId !== null) {
      for (const job of this.jobs.values()) {
        if (job.id === jobId) return jobSnapshot(job)
      }
      return null
    }
    let latest = null
    for (const job of this.jobs.values()) {
      if (latest === null || job.startedAt > latest.startedAt) latest = job
    }
    return latest === null ? null : jobSnapshot(latest)
  }

  /** Cancel the running job. The kill escalates inside `runGit`, so this returns immediately. */
  cancelScan({ jobId = null } = {}) {
    for (const job of this.jobs.values()) {
      if (jobId !== null && job.id !== jobId) continue
      if (job.state !== 'running') continue
      job.state = 'cancelled'
      job.finishedAt = Date.now()
      job.controller.abort()
      this.trace('scan-cancel', { repo: job.repo.rel, job: job.id })
      return jobSnapshot(job)
    }
    return null
  }

  /** The cached scan result of one repository, if any. */
  scanResult(repo) {
    const job = this.jobs.get(repo.path)
    const result = this.results.get(repo.path)
    const plan = result?.plan ?? (job?.plan ?? []).map((chunk) => ({ path: chunk.label, state: chunk.state, count: chunk.count, ms: chunk.ms, error: chunk.error ?? null }))
    return {
      state: job?.state ?? (result === undefined ? 'idle' : 'done'),
      jobId: job?.id ?? null,
      startedAt: job?.startedAt ?? null,
      elapsedMs: job === undefined ? (result?.elapsedMs ?? null) : (job.finishedAt ?? Date.now()) - job.startedAt,
      chunks: plan,
      chunksDone: plan.filter((chunk) => chunk.state !== 'pending' && chunk.state !== 'running').length,
      chunksTotal: plan.length,
      files: job !== undefined && job.state === 'running' ? job.files : (result?.files ?? job?.files ?? []),
      total: (job !== undefined && job.state === 'running' ? job.files : (result?.files ?? job?.files ?? [])).length,
      truncated: result?.truncated === true || job?.truncated === true,
      includeUntracked: result?.includeUntracked ?? job?.includeUntracked ?? false,
      completedAt: result?.completedAt ?? null,
      error: job?.error ?? null,
    }
  }

  /**
   * Everything the panel needs to paint: repository identity, the fast staged set, and whatever the
   * last scan of this repository already knows.
   */
  async status({ repo }) {
    const meta = await this.metadata(repo)
    const staged = await this.stagedChanges(repo)
    const scan = this.scanResult(repo)
    const files = []
    const seen = new Set()
    for (const record of staged) {
      const key = `${record.side}\0${record.path}`
      if (seen.has(key)) continue
      seen.add(key)
      files.push({ path: record.path, status: record.status, side: record.side })
    }
    for (const record of scan.files) {
      const key = `${record.side}\0${record.path}`
      if (seen.has(key)) continue
      seen.add(key)
      files.push(record)
    }
    return { repo, meta, staged, scan, files, total: files.length }
  }

  /**
   * Validate one repository-relative path and clamp it inside the repository.
   *
   * Absolute paths, `..` segments, backslashes and NUL bytes are refused outright rather than
   * normalized, because a path that needs normalizing is a path the caller did not mean; whatever
   * survives is then resolved and re-checked against the repository root, so a symlink pointing out
   * of the tree is caught too. Every git call still passes the path after `--`, so a name that
   * looks like an option is data, not an argument.
   */
  async resolveRepoPath(repo, input) {
    if (typeof input !== 'string' || input.trim() === '') throw fail(CODES.unsupportedPath, '没有指定文件路径')
    const raw = input.trim()
    if (raw.includes('\0') || raw.includes('\\') || isAbsolute(raw)) {
      throw fail(CODES.unsupportedPath, '文件路径不合法', { path: raw })
    }
    const segments = raw.split('/')
    if (segments.includes('..') || segments.includes('.')) {
      throw fail(CODES.unsupportedPath, '文件路径不合法', { path: raw })
    }
    const absolute = resolve(repo.path, raw)
    if (!isInside(repo.path, absolute)) {
      throw fail(CODES.outsideRoot, '文件不在仓库内', { path: raw })
    }
    let real = absolute
    try {
      real = await realpath(absolute)
    } catch {
      // A deleted or untracked-and-gone path has no realpath; the lexical check above stands.
      try {
        real = join(await realpath(resolve(absolute, '..')), basename(absolute))
      } catch {
        real = absolute
      }
    }
    if (!isInside(repo.path, real)) {
      throw fail(CODES.outsideRoot, '文件不在仓库内（符号链接）', { path: raw, resolved: real })
    }
    return { path: raw, absolute: real }
  }

  /**
   * One file's diff, as the raw unified text the browser parses into two columns.
   *
   * The two sides are named for the header: `HEAD → 索引` for the staged set, `索引 → 工作区` for
   * the worktree set, `HEAD → 工作区` for everything at once, and `/dev/null → 工作区` for an
   * untracked file (which git only diffs through `--no-index`, whose "differences found" exit code
   * of 1 is the expected answer, not a failure).
   */
  async diff({ repo, path, side = 'unstaged', context = 3 }) {
    const target = await this.resolveRepoPath(repo, path)
    // The three comparisons go through PLUMBING, not `git diff`. The porcelain refreshes the index
    // before it answers, and that refresh is a full worktree pass: on this mount `git diff -- <one
    // file>` did not finish in 60 s, while the equivalent plumbing command answered in 1.2 s without
    // touching the index at all. Plumbing also means the plugin cannot write the index even if a
    // future git changes the porcelain's default.
    const sideLabels = {
      staged: { oldLabel: 'HEAD', newLabel: '索引', command: ['diff-index', '--cached'], revision: 'HEAD' },
      unstaged: { oldLabel: '索引', newLabel: '工作区', command: ['diff-files'], revision: null },
      combined: { oldLabel: 'HEAD', newLabel: '工作区', command: ['diff-index'], revision: 'HEAD' },
      untracked: { oldLabel: '/dev/null', newLabel: '工作区', command: null, revision: null },
    }
    const chosen = sideLabels[side]
    if (chosen === undefined) throw fail(CODES.unsupportedPath, `未知的对比来源 ${JSON.stringify(side)}`, { side })
    const unifiedContext = clampInt(context, 3, 0, 200)
    const budget = this.limits.maxDiffBytes

    if (side !== 'staged') {
      const info = await stat(target.absolute).catch(() => null)
      if (info !== null && info.isFile() && info.size > budget) {
        throw fail(CODES.tooLarge, `文件过大（${info.size} 字节），超过 ${budget} 字节上限`, { path: path, size: info.size, limit: budget })
      }
    }

    const diffOptions = ['--no-color', '--no-ext-diff', '--no-textconv', '--no-renames', `-U${unifiedContext}`]
    const command = side === 'untracked'
      // An untracked file has no index entry to compare against, so it is diffed against /dev/null.
      // `--no-index` never touches the index, and its "differences found" exit code of 1 is the
      // expected answer rather than a failure.
      ? [...ownershipArgs(repo), 'diff', '--no-color', '--no-ext-diff', '--no-textconv', `-U${unifiedContext}`, '--no-index', '--', '/dev/null', target.absolute]
      : [
          ...ownershipArgs(repo),
          ...chosen.command,
          '-p',
          ...diffOptions,
          ...(chosen.revision === null ? [] : [chosen.revision]),
          '--',
          target.path,
        ]

    let text = ''
    let truncated = false
    try {
      const result = await git({
        cwd: repo.path,
        args: command,
        label: 'diff',
        allowExit: side === 'untracked' ? [0, 1] : [0],
        timeoutMs: this.limits.diffTimeoutMs,
        maxBytes: budget + 4 * 1024 * 1024,
      })
      text = result.stdout.toString('utf8')
    } catch (error) {
      // A diff larger than its budget still has a useful prefix: show it and say it was cut.
      if (error?.code === CODES.tooLarge && typeof error.details?.partial === 'string') {
        text = error.details.partial
        truncated = true
      } else {
        throw error
      }
    }
    if (Buffer.byteLength(text, 'utf8') >= budget) {
      text = text.slice(0, budget)
      truncated = true
    }
    const binary = looksBinaryDiff(text)
    const stats = binary ? { additions: 0, deletions: 0 } : diffStats(text)
    const record = (this.scanResult(repo).files ?? []).find((file) => file.path === target.path)
    this.trace('diff', { repo: repo.rel, path: target.path, side, bytes: text.length, binary, truncated })
    return {
      repo,
      path: target.path,
      side,
      status: record?.status ?? null,
      oldLabel: chosen.oldLabel,
      newLabel: chosen.newLabel,
      unified: text,
      truncated,
      binary,
      empty: text.trim() === '',
      stats,
      limit: budget,
    }
  }

  // ---- cache ------------------------------------------------------------------------------

  /** Read `cache.json`: the last completed scan of each repository, so a refresh paints at once. */
  async loadCache() {
    if (this.cacheFile === null) return
    let parsed
    try {
      parsed = JSON.parse(await readFile(this.cacheFile, 'utf8'))
    } catch {
      return
    }
    if (parsed === null || typeof parsed !== 'object' || parsed.version !== 1 || typeof parsed.entries !== 'object') return
    for (const [path, entry] of Object.entries(parsed.entries ?? {})) {
      if (typeof path !== 'string' || entry === null || typeof entry !== 'object') continue
      if (!isInside(this.root, path) || !existsSync(join(path, '.git'))) continue
      if (!Array.isArray(entry.files)) continue
      this.results.set(path, {
        completedAt: typeof entry.completedAt === 'number' ? entry.completedAt : null,
        files: entry.files.filter((file) => file !== null && typeof file === 'object' && typeof file.path === 'string').slice(0, this.limits.maxFiles),
        plan: Array.isArray(entry.plan) ? entry.plan : [],
        truncated: entry.truncated === true,
        includeUntracked: entry.includeUntracked === true,
        elapsedMs: typeof entry.elapsedMs === 'number' ? entry.elapsedMs : null,
      })
    }
    this.trace('cache-loaded', { entries: this.results.size })
  }

  /** Write the cache back, atomically and best-effort: diagnostics must never break a scan. */
  async saveCache() {
    if (this.cacheFile === null) return
    const entries = {}
    for (const [path, result] of this.results) {
      entries[path] = { ...result, files: result.files.slice(0, this.limits.maxFiles) }
    }
    const body = JSON.stringify({ version: 1, savedAt: new Date().toISOString(), entries })
    const temporary = `${this.cacheFile}.tmp`
    try {
      await writeFile(temporary, body)
      await rename(temporary, this.cacheFile)
    } catch (error) {
      try {
        await unlink(temporary)
      } catch {
        // Nothing to clean up.
      }
      this.trace('cache-write-failed', { message: messageOf(error) })
    }
  }
}
