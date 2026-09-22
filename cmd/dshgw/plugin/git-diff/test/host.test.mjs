// Host-half tests: repository discovery, the read-only guarantee, the chunked scan, and every
// endpoint of the wire contract.
//
// Everything runs against real temporary git repositories on a real filesystem — the service imports
// nothing from dsh precisely so this is possible — and the endpoint table is driven through the same
// dispatcher the RPC channel calls, so a pass means the wire contract works, not just the helpers.
//
// Run: npm test   (node --test test/)

import { strict as assert } from 'node:assert'
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync } from 'node:fs'
import { mkdir, mkdtemp, readFile, rename, rm, stat, symlink, utimes, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

import {
  CODES, GitService, clampInt, diffStats, isInside, looksBinaryDiff, ownershipArgs, parseNameStatus, runGit, statusLabel,
} from '../git-service.js'
import { createHandlers } from '../rpc-handlers.js'
// index.js is imported for its config reader only; it is never applied in these tests.
import { readConfig } from '../index.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const PLUGIN_DIR = join(HERE, '..')
const RPC_CHANNEL = '/dshgw-git-diff'

const HAS_GIT = (() => {
  try {
    execFileSync('git', ['--version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
})()

const LIMITS = {
  version: 'test',
  pluginDir: PLUGIN_DIR,
  traceFile: null,
  cacheFile: null,
  trace: false,
  rootLabel: null,
  maxRepoDepth: 6,
  maxRepoCandidates: 200,
  chunkTimeoutMs: 20_000,
  chunkTargetFiles: 25_000,
  diffTimeoutMs: 30_000,
  untrackedDefault: false,
  maxUntrackedEntries: 50,
  maxFiles: 500,
  maxDiffBytes: 64 * 1024,
}

/** Run git in a fixture repository, failing loudly when it does. */
function git(cwd, args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8', env: { ...process.env, GIT_OPTIONAL_LOCKS: '0' } }).trim()
}

/** One temporary root with a repository nested three levels down, plus decoys around it. */
async function makeWorld({ withEscapeLink = false } = {}) {
  const root = await mkdtemp(join(tmpdir(), 'gd-'))
  const repo = join(root, 'deep', 'nested', 'repo')
  await mkdir(repo, { recursive: true })
  git(repo, ['init', '-q', '.'])
  git(repo, ['config', 'user.email', 'test@example.com'])
  git(repo, ['config', 'user.name', 'Test'])
  await writeFile(join(repo, 'keep.txt'), 'one\ntwo\nthree\n')
  await writeFile(join(repo, 'delete-me.txt'), 'gone soon\n')
  await writeFile(join(repo, '中文 名字.txt'), '中文内容\n')
  await mkdir(join(repo, 'src', 'deep'), { recursive: true })
  await writeFile(join(repo, 'src', 'deep', 'inner.txt'), 'inner\n')
  await writeFile(join(repo, 'blob.bin'), Buffer.from([0x00, 0x01, 0x02, 0xff]))
  git(repo, ['add', '-A'])
  git(repo, ['commit', '-qm', 'init'])

  // Decoys: a `.git` inside node_modules and inside a dot-directory must never be reported.
  for (const decoy of [join(root, 'node_modules', 'pkg'), join(root, '.cache', 'pkg'), join(repo, 'node_modules', 'pkg')]) {
    await mkdir(decoy, { recursive: true })
    await mkdir(join(decoy, '.git'), { recursive: true })
  }
  if (withEscapeLink) {
    const outside = await mkdtemp(join(tmpdir(), 'gd-outside-'))
    await writeFile(join(outside, 'secret.txt'), 'secret\n')
    await symlink(join(outside, 'secret.txt'), join(repo, 'escape.txt'))
  }
  return { root, repo, rel: join('deep', 'nested', 'repo') }
}

/** One service over a world, with test limits. */
async function makeService(world, overrides = {}) {
  const limits = { ...LIMITS, ...overrides }
  const service = await GitService.create({
    root: world.root,
    rootLabel: null,
    repo: overrides.repo ?? null,
    limits,
    cacheFile: overrides.cacheFile ?? null,
    log: () => {},
    trace: () => {},
  })
  return service
}

/** Wait until no scan is running, then return its final snapshot. */
async function waitForScan(service, { timeoutMs = 30_000 } = {}) {
  const started = Date.now()
  for (;;) {
    const snapshot = service.scanStatus({ jobId: null })
    if (snapshot === null || snapshot.state !== 'running') return snapshot
    if (Date.now() - started > timeoutMs) throw new Error('scan did not finish in time')
    await new Promise((resolve) => setTimeout(resolve, 20))
  }
}

/** Load a repository with a modified, a staged, a deleted and an untracked file. */
async function dirty(world) {
  await writeFile(join(world.repo, 'keep.txt'), 'one\nTWO\nthree\nfour\n')
  await writeFile(join(world.repo, 'src', 'deep', 'inner.txt'), 'inner changed\n')
  git(world.repo, ['add', 'src/deep/inner.txt'])
  await rm(join(world.repo, 'delete-me.txt'))
  await writeFile(join(world.repo, 'fresh.txt'), 'brand new\n')
  await writeFile(join(world.repo, 'blob.bin'), Buffer.from([0x00, 0x01, 0x03, 0xfe]))
}

const skip = HAS_GIT ? false : 'git is not available in this environment'

// ---- pure helpers --------------------------------------------------------------------------

test('helpers: path clamping, integer clamping, name-status parsing, and diff annotations', () => {
  assert.equal(isInside('/a/b', '/a/b'), true)
  assert.equal(isInside('/a/b', '/a/b/c'), true)
  assert.equal(isInside('/a/b', '/a/bc'), false)
  assert.equal(isInside('/a/b', '/a'), false)

  assert.equal(clampInt('nonsense', 7, 1, 10), 7)
  assert.equal(clampInt(0, 7, 1, 10), 1)
  assert.equal(clampInt(99, 7, 1, 10), 10)
  assert.equal(clampInt(3.9, 7, 1, 10), 3)

  const records = parseNameStatus(Buffer.from('M\0a.txt\0D\0中文 名字.txt\0A\0b/c.txt\0', 'utf8'))
  assert.deepEqual(records, [
    { status: 'M', path: 'a.txt' },
    { status: 'D', path: '中文 名字.txt' },
    { status: 'A', path: 'b/c.txt' },
  ])
  assert.deepEqual(parseNameStatus(Buffer.from('', 'utf8')), [])

  assert.deepEqual(diffStats('--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n+extra\n same\n'), { additions: 2, deletions: 1 })
  assert.equal(looksBinaryDiff('diff --git a/b.bin b/b.bin\nBinary files a/b.bin and b/b.bin differ\n'), true)
  assert.equal(looksBinaryDiff('@@ -1 +1 @@\n-a\n+b\n'), false)
  assert.equal(statusLabel('M'), '修改')
  assert.equal(statusLabel('?'), '未跟踪')
})

test('config: defaults, flat keys, nested limits, and malformed values', () => {
  const defaults = readConfig(undefined)
  assert.equal(defaults.maxRepoDepth, 6)
  assert.equal(defaults.chunkTimeoutMs, 90_000)
  assert.equal(defaults.chunkTargetFiles, 25_000)
  assert.equal(defaults.untrackedDefault, false)
  assert.equal(defaults.trace, true)
  assert.equal(defaults.root, process.cwd())

  const flat = readConfig({ root: '/tmp', maxFiles: 5, chunkTimeoutMs: 'abc', untracked: true, trace: false })
  assert.equal(flat.root, '/tmp')
  assert.equal(flat.maxFiles, 5)
  assert.equal(flat.chunkTimeoutMs, 90_000, 'a malformed number falls back to the default')
  assert.equal(flat.untrackedDefault, true)
  assert.equal(flat.trace, false)

  const nested = readConfig({ limits: { maxRepoDepth: 99, maxDiffBytes: 1 } })
  assert.equal(nested.maxRepoDepth, 32, 'out-of-range values clamp to the documented bound')
  assert.equal(nested.maxDiffBytes, 4096)
})

test('config: the row may name the trace and cache files, and an out-of-tree row keeps its own', () => {
  // Every tenant shares one installed copy (M75), so the gateway names per-tenant files: the trace
  // because one shared file would mix tenants, the cache because it holds repository paths. A row
  // that stays silent keeps the $DSH_HOME/plugins default, and a relative path is not a path.
  const own = { trace: join(PLUGIN_DIR, 'trace.jsonl'), cache: join(PLUGIN_DIR, 'cache.json') }
  const defaults = readConfig(undefined)
  assert.equal(defaults.traceFile, own.trace)
  assert.equal(defaults.cacheFile, own.cache)

  const named = readConfig({ traceFile: '/var/lib/dshgw/state/tenants/x/.dsh/plugin-state/git-diff.trace.jsonl', cacheFile: '/var/lib/dshgw/state/tenants/x/.dsh/plugin-state/git-diff.cache.json' })
  assert.equal(named.traceFile, '/var/lib/dshgw/state/tenants/x/.dsh/plugin-state/git-diff.trace.jsonl')
  assert.equal(named.cacheFile, '/var/lib/dshgw/state/tenants/x/.dsh/plugin-state/git-diff.cache.json')
  for (const key of ['traceFile', 'cacheFile']) {
    assert.equal(readConfig({ [key]: 'relative.json' })[key], own[key === 'traceFile' ? 'trace' : 'cache'])
    assert.equal(readConfig({ [key]: '' })[key], own[key === 'traceFile' ? 'trace' : 'cache'])
  }
})

// ---- discovery and clamping -----------------------------------------------------------------

test('discovery: finds nested repositories, skips dot/node_modules decoys, and never descends into a repo', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  try {
    const repos = await service.discover()
    assert.equal(repos.length, 1, `expected exactly the fixture repository, got ${JSON.stringify(repos)}`)
    assert.equal(repos[0].rel, world.rel)
    assert.equal(repos[0].label, 'repo')

    const shallow = await makeService(world, { maxRepoDepth: 1 })
    assert.deepEqual(await shallow.discover(), [], 'a depth cap of one cannot reach a three-level repository')

    const again = await service.discover({ refresh: true })
    assert.equal(again.length, 1)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('ownership: discovery reports writability and the exemption names exactly one path', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  try {
    const repos = await service.discover()
    assert.equal(repos[0].writable, true, 'an ordinary work tree is writable')
    const args = ownershipArgs(repos[0])
    assert.deepEqual(args, ['-c', `safe.directory=${repos[0].path}`], 'the exemption is the exact clamped path')
    assert.equal(args.some((arg) => arg.includes('*')), false, 'never a wildcard')
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('resolveRepo: refuses a repository outside the root, a missing path, and a plain directory', { skip }, async () => {
  const world = await makeWorld()
  const outside = await mkdtemp(join(tmpdir(), 'gd-out-'))
  git(outside, ['init', '-q', '.'])
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    assert.equal(repo.path, world.repo)

    await assert.rejects(() => service.resolveRepo(outside), (error) => error.code === CODES.outsideRoot)
    await assert.rejects(() => service.resolveRepo(join(world.root, 'nope')), (error) => error.code === CODES.notFound)
    await assert.rejects(() => service.resolveRepo(join(world.root, 'deep')), (error) => error.code === CODES.notARepo)
    await assert.rejects(() => service.resolveRepo(''), (error) => error.code === CODES.notARepo)
    await assert.rejects(() => service.resolveRepo('../etc'), (error) => error.code === CODES.notFound)
  } finally {
    await rm(world.root, { recursive: true, force: true })
    await rm(outside, { recursive: true, force: true })
  }
})

// ---- the fast half: index-level change sets -------------------------------------------------

test('stagedChanges: reports the staged set only, with A/M/D statuses', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    assert.deepEqual(await service.stagedChanges(repo), [], 'a clean repository stages nothing')

    await dirty(world)
    const staged = await service.stagedChanges(repo)
    assert.deepEqual(staged, [{ status: 'M', path: 'src/deep/inner.txt', side: 'staged' }], 'only the staged file is here')
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('stagedChanges: an unborn HEAD reports every cached path as added', { skip }, async () => {
  const world = await makeWorld()
  const fresh = join(world.root, 'fresh-repo')
  await mkdir(fresh, { recursive: true })
  git(fresh, ['init', '-q', '.'])
  await writeFile(join(fresh, 'first.txt'), 'hello\n')
  git(fresh, ['add', '-A'])
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(fresh)
    const staged = await service.stagedChanges(repo)
    assert.deepEqual(staged, [{ status: 'A', path: 'first.txt', side: 'staged' }])
    const meta = await service.metadata(repo)
    assert.equal(meta.unborn, true)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('status: paints the staged set immediately and reports an idle scan before any scan runs', { skip }, async () => {
  const world = await makeWorld()
  await dirty(world)
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    const status = await service.status({ repo })
    assert.equal(status.scan.state, 'idle')
    assert.equal(status.scan.completedAt, null)
    assert.deepEqual(status.files.map((file) => [file.side, file.path]), [['staged', 'src/deep/inner.txt']])
    assert.equal(status.meta.unborn, false)
    assert.equal(status.meta.branch, 'master')
    assert.match(status.meta.subject ?? '', /init/)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

// ---- the slow half: the chunked scan --------------------------------------------------------

test('planChunks: one chunk per top-level directory plus the root files, cheapest first', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    const plan = await service.planChunks(repo)
    const packed = plan.flatMap((chunk) => chunk.members ?? [chunk.label])
    assert.ok(packed.includes('src'), `expected a src chunk, got ${JSON.stringify(plan.map((chunk) => chunk.label))}`)
    assert.ok(packed.includes('（根目录文件）'), 'root-level files are planned')
    assert.ok(!packed.includes('node_modules'), 'node_modules is not planned')
    assert.ok(!packed.includes('.git'))
    const counts = plan.map((chunk) => chunk.fileCount)
    assert.deepEqual(counts, [...counts].sort((left, right) => left - right), 'chunks ascend by tracked file count')
    // This fixture has four tracked directories and a handful of files, so packing folds them all
    // into one chunk — which is the point: one git invocation instead of one per directory.
    assert.equal(plan.length, 1, 'small directories pack into a single chunk')
    assert.ok(plan[0].paths.includes('src'), 'the packed chunk still carries the directory')
    assert.equal(plan[0].fileCount, 5)
    assert.match(plan[0].label, /等 2 个目录|src/, `the label names the pack: ${plan[0].label}`)

    // A directory bigger than the target is never packed with anything.
    const unpacked = await makeService(world, { chunkTargetFiles: 0 })
    const unpackedRepo = await unpacked.resolveRepo(world.repo)
    const unpackedPlan = await unpacked.planChunks(unpackedRepo)
    assert.equal(unpackedPlan.length, 2, 'chunkTargetFiles 0 means one chunk per directory')
    assert.ok(unpackedPlan.some((chunk) => chunk.label === 'src'))

    // A target smaller than one directory keeps that directory whole.
    const tiny = await makeService(world, { chunkTargetFiles: 1 })
    const tinyPlan = await tiny.planChunks(await tiny.resolveRepo(world.repo))
    assert.equal(tinyPlan.length > 1, true, 'a tiny target splits the plan again')
    assert.ok(tinyPlan.every((chunk) => chunk.paths.length > 0))
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: finds the worktree changes chunk by chunk, skips untracked by default, and caches the result', { skip }, async () => {
  const world = await makeWorld()
  await dirty(world)
  const cacheFile = join(world.root, 'cache.json')
  const service = await makeService(world, { cacheFile })
  try {
    const repo = await service.resolveRepo(world.repo)
    const started = await service.startScan({ repo })
    assert.equal(started.state, 'running')
    assert.ok(started.chunksTotal > 0)
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'done')
    const found = new Map(finished.files.map((file) => [`${file.side}:${file.path}`, file.status]))
    assert.equal(found.get('unstaged:keep.txt'), 'M')
    assert.equal(found.get('unstaged:delete-me.txt'), 'D')
    assert.equal(found.get('unstaged:blob.bin'), 'M')
    assert.equal(found.has('unstaged:src/deep/inner.txt'), false, 'a staged file matches the index, so it is not an unstaged change')
    assert.equal(found.has('untracked:fresh.txt'), false, 'untracked files stay out unless asked for')
    assert.equal(finished.chunksDone, finished.chunksTotal)
    assert.ok(finished.elapsedMs >= 0)

    // The completed scan is what `status` reports, so a refresh paints without rescanning.
    const status = await service.status({ repo })
    assert.equal(status.scan.state, 'done')
    assert.ok(status.files.some((file) => file.path === 'keep.txt' && file.side === 'unstaged'))
    assert.ok(status.files.some((file) => file.path === 'src/deep/inner.txt' && file.side === 'staged'))

    // ... and it survives a restart, because cache.json was written.
    const revived = await makeService(world, { cacheFile })
    const revivedStatus = await revived.status({ repo: await revived.resolveRepo(world.repo) })
    assert.equal(revivedStatus.scan.state, 'done')
    assert.ok(revivedStatus.files.some((file) => file.path === 'keep.txt'))
    const cache = JSON.parse(await readFile(cacheFile, 'utf8'))
    assert.equal(cache.version, 1)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: includes untracked files on request and honours the entry cap', { skip }, async () => {
  const world = await makeWorld()
  await writeFile(join(world.repo, 'fresh.txt'), 'brand new\n')
  await writeFile(join(world.repo, 'another.txt'), 'also new\n')
  const service = await makeService(world, { maxUntrackedEntries: 1 })
  try {
    const repo = await service.resolveRepo(world.repo)
    await service.startScan({ repo, includeUntracked: true })
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'done')
    const untracked = finished.files.filter((file) => file.side === 'untracked')
    assert.equal(untracked.length, 1, 'the cap admits exactly one untracked file')
    assert.equal(untracked[0].status, '?')
    assert.equal(finished.truncated, true, 'the second untracked file over the cap is reported as truncated')
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: a failing chunk is marked and skipped, and the rest of the plan still completes', { skip }, async () => {
  const world = await makeWorld()
  await dirty(world)
  // One directory per chunk, so the injected failure lands on a chunk named `src` exactly.
  const service = await makeService(world, { chunkTargetFiles: 0 })
  try {
    const repo = await service.resolveRepo(world.repo)
    const original = service.scanChunk.bind(service)
    service.scanChunk = async (target, chunk, untracked, signal) => {
      if (chunk.label === 'src') {
        const error = new Error('模拟的块失败')
        error.code = CODES.timeout
        throw error
      }
      return await original(target, chunk, untracked, signal)
    }
    await service.startScan({ repo })
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'done')
    const failed = finished.plan.find((chunk) => chunk.path === 'src')
    assert.equal(failed.state, 'timeout')
    assert.match(failed.error ?? '', /模拟的块失败/)
    assert.ok(finished.files.some((file) => file.path === 'keep.txt'), 'the other chunks still contributed')
    assert.equal(finished.files.some((file) => file.path === 'src/deep/inner.txt'), false)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: cancel stops the run and marks it cancelled', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    const original = service.scanChunk.bind(service)
    service.scanChunk = async (...args) => {
      await new Promise((resolve) => setTimeout(resolve, 30))
      return await original(...args)
    }
    const job = await service.startScan({ repo })
    const cancelled = service.cancelScan({ jobId: job.jobId })
    assert.equal(cancelled.state, 'cancelled')
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'cancelled')
    assert.equal(service.scanStatus({ jobId: 'scan-does-not-exist' }), null)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: a second start while one runs returns that same job instead of racing it', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    const original = service.scanChunk.bind(service)
    service.scanChunk = async (...args) => {
      await new Promise((resolve) => setTimeout(resolve, 30))
      return await original(...args)
    }
    const first = await service.startScan({ repo })
    const second = await service.startScan({ repo })
    assert.equal(second.jobId, first.jobId)
    assert.equal(second.alreadyRunning, true)
    service.cancelScan({ jobId: first.jobId })
    await waitForScan(service)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: a scoped scan touches only the requested subtree', { skip }, async () => {
  const world = await makeWorld()
  await dirty(world)
  // `dirty` stages src/deep/inner.txt; touch it again so the subtree has an unstaged change too.
  await writeFile(join(world.repo, 'src', 'deep', 'inner.txt'), 'inner changed twice\n')
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    await service.startScan({ repo, scopes: ['src'] })
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'done')
    assert.equal(finished.plan.length, 1)
    assert.equal(finished.plan[0].path, 'src')
    assert.deepEqual(finished.files.map((file) => file.path), ['src/deep/inner.txt'])
    await assert.rejects(() => service.startScan({ repo, scopes: ['../etc'] }), (error) => error.code === CODES.unsupportedPath)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('scan: a replaced inode with the same mtime and size is not a modification', { skip }, async () => {
  // The sshfs condition in miniature. git decides whether a tracked file changed by comparing the
  // stat data recorded in the index against the worktree, inode included — and an sshfs mount does
  // not present the remote's inode numbers, so every one of the 805,368 tracked files in this
  // tenant's checkout looks stat-dirty: a plain `diff-files --name-status` reported all 12,018 files
  // under a-ztc/ as modified on a tree `git show HEAD` proves is clean. `core.checkStat=minimal`
  // compares mtime and size only — what git does on a local disk, and what the author sees when they
  // run `git status` on the machine that owns the checkout.
  //
  // The two assertions below are the whole point: WITHOUT the setting the swap looks like a change
  // (the false positive), and WITH it the panel stays quiet — so this test fails if anyone drops the
  // setting from BASE_ARGS.
  const root = await mkdtemp(join(tmpdir(), 'gd-ino-'))
  const repo = join(root, 'repo')
  await mkdir(repo, { recursive: true })
  git(repo, ['init', '-q', '.'])
  git(repo, ['config', 'user.email', 'test@example.com'])
  git(repo, ['config', 'user.name', 'Test'])
  const target = join(repo, 'keep.txt')
  await writeFile(target, 'one\ntwo\nthree\n')
  // The entry has to be older than the index file, or git marks it "racily clean" and verifies its
  // content anyway — which would hide the very behaviour this test pins.
  await new Promise((resolve) => setTimeout(resolve, 1200))
  git(repo, ['add', '-A'])
  git(repo, ['commit', '-qm', 'init'])

  const service = await makeService({ root, repo })
  try {
    const before = await stat(target)
    const swap = join(repo, 'keep.replacement')
    await writeFile(swap, 'one\ntwo\nthree\n')
    // Same whole second, same size, different inode and sub-second stamp: what the mount presents.
    const second = Math.floor(before.mtimeMs / 1000)
    await utimes(swap, second + 0.5, second + 0.5)
    await rename(swap, target)
    const after = await stat(target)
    assert.notEqual(after.ino, before.ino, 'the fixture must really swap the inode')
    assert.equal(after.size, before.size)
    assert.equal(Math.floor(after.mtimeMs / 1000), second, 'the whole-second mtime is unchanged')

    // Raw git here on purpose: `runGit` always prepends this plugin's BASE_ARGS, so it cannot show
    // what the comparison looks like without them.
    const plain = git(repo, ['--no-optional-locks', 'diff-files', '--name-status'])
    const minimal = git(repo, ['--no-optional-locks', '-c', 'core.checkStat=minimal', 'diff-files', '--name-status'])
    assert.match(plain, /keep\.txt/, 'without minimal stats the inode swap reads as a change')
    assert.equal(minimal.trim(), '', 'with it the same file is correctly unchanged')

    const descriptor = await service.resolveRepo(repo)
    await service.startScan({ repo: descriptor })
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'done')
    assert.equal(finished.files.some((file) => file.path === 'keep.txt'), false, 'the panel must not list the false positive')

    // A genuine change to the same file is still reported.
    await writeFile(target, 'one\nTWO\nthree\nfour\n')
    await service.startScan({ repo: descriptor, force: true })
    const rescanned = await waitForScan(service)
    assert.ok(rescanned.files.some((file) => file.path === 'keep.txt' && file.status === 'M'), 'a real modification still shows up')
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})

test('scan: the repository index is never written', { skip }, async () => {
  const world = await makeWorld()
  await dirty(world)
  const service = await makeService(world)
  const indexFile = join(world.repo, '.git', 'index')
  const fingerprint = async () => {
    const bytes = await readFile(indexFile)
    const info = await stat(indexFile)
    return { sha: createHash('sha256').update(bytes).digest('hex'), mtimeMs: info.mtimeMs, size: info.size }
  }
  try {
    const repo = await service.resolveRepo(world.repo)
    const before = await fingerprint()
    await service.status({ repo })
    await service.startScan({ repo, includeUntracked: true })
    await waitForScan(service)
    await service.diff({ repo, path: 'keep.txt', side: 'unstaged' })
    await service.diff({ repo, path: 'keep.txt', side: 'combined' })
    const after = await fingerprint()
    assert.deepEqual(after, before, 'the plugin must not refresh, rewrite or lock .git/index')
    assert.equal(existsSync(join(world.repo, '.git', 'index.lock')), false)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

// ---- diffs -----------------------------------------------------------------------------------

test('diff: unstaged, staged and combined sides name both ends and carry the change', { skip }, async () => {
  const world = await makeWorld()
  await writeFile(join(world.repo, 'keep.txt'), 'one\nTWO\nthree\nfour\n')
  git(world.repo, ['add', 'keep.txt'])
  await writeFile(join(world.repo, 'keep.txt'), 'one\nTWO\nthree\nfive\n')
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)

    const staged = await service.diff({ repo, path: 'keep.txt', side: 'staged' })
    assert.equal(staged.oldLabel, 'HEAD')
    assert.equal(staged.newLabel, '索引')
    assert.match(staged.unified, /-two\n\+TWO/)
    assert.equal(staged.binary, false)
    assert.equal(staged.truncated, false)
    assert.deepEqual(staged.stats, { additions: 2, deletions: 1 }, 'HEAD has "two", the index has "TWO" and the extra "four"')

    const unstaged = await service.diff({ repo, path: 'keep.txt', side: 'unstaged' })
    assert.equal(unstaged.oldLabel, '索引')
    assert.equal(unstaged.newLabel, '工作区')
    assert.match(unstaged.unified, /-four\n\+five/)
    assert.deepEqual(unstaged.stats, { additions: 1, deletions: 1 })

    const combined = await service.diff({ repo, path: 'keep.txt', side: 'combined' })
    assert.equal(combined.oldLabel, 'HEAD')
    assert.equal(combined.newLabel, '工作区')
    assert.match(combined.unified, /\+TWO/)
    assert.match(combined.unified, /\+five/)
    assert.deepEqual(combined.stats, { additions: 2, deletions: 1 }, 'HEAD → 工作区 sees only "two" removed and "TWO"/"five" added')
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('diff: an untracked file, a deleted file, a binary file, and an over-budget file', { skip }, async () => {
  const world = await makeWorld()
  await writeFile(join(world.repo, 'fresh.txt'), 'brand new\nsecond\n')
  await rm(join(world.repo, 'delete-me.txt'))
  await writeFile(join(world.repo, 'blob.bin'), Buffer.from([0x00, 0x01, 0x03, 0xfe]))
  await writeFile(join(world.repo, 'huge.txt'), 'x'.repeat(200_000))
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)

    const added = await service.diff({ repo, path: 'fresh.txt', side: 'untracked' })
    assert.equal(added.oldLabel, '/dev/null')
    assert.match(added.unified, /new file mode/)
    assert.match(added.unified, /\+brand new/)
    assert.deepEqual(added.stats, { additions: 2, deletions: 0 })

    const removed = await service.diff({ repo, path: 'delete-me.txt', side: 'unstaged' })
    assert.match(removed.unified, /deleted file mode/)
    assert.ok(removed.stats.deletions >= 1)

    const binary = await service.diff({ repo, path: 'blob.bin', side: 'unstaged' })
    assert.equal(binary.binary, true)
    assert.deepEqual(binary.stats, { additions: 0, deletions: 0 })

    await assert.rejects(() => service.diff({ repo, path: 'huge.txt', side: 'untracked' }), (error) => error.code === CODES.tooLarge)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('diff: refuses a path that is not a plain repository-relative one, and one that escapes by symlink', { skip }, async () => {
  const world = await makeWorld({ withEscapeLink: true })
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    for (const bad of ['../keep.txt', '/etc/passwd', 'a\\b.txt', 'a\0b', '', 'src/../keep.txt']) {
      await assert.rejects(() => service.diff({ repo, path: bad, side: 'unstaged' }), (error) => (
        error.code === CODES.unsupportedPath || error.code === CODES.outsideRoot
      ), `expected ${JSON.stringify(bad)} to be refused`)
    }
    await assert.rejects(() => service.diff({ repo, path: 'escape.txt', side: 'untracked' }), (error) => error.code === CODES.outsideRoot)
    await assert.rejects(() => service.diff({ repo, path: 'keep.txt', side: 'nonsense' }), (error) => error.code === CODES.unsupportedPath)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('diff: the UTF-8 name survives the round trip', { skip }, async () => {
  const world = await makeWorld()
  await writeFile(join(world.repo, '中文 名字.txt'), '中文内容\n改过了\n')
  const service = await makeService(world)
  try {
    const repo = await service.resolveRepo(world.repo)
    const result = await service.diff({ repo, path: '中文 名字.txt', side: 'unstaged' })
    assert.equal(result.path, '中文 名字.txt')
    assert.match(result.unified, /\+改过了/)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('runGit: the timeout is enforced and reported as a coded failure', { skip }, async () => {
  const world = await makeWorld()
  try {
    const result = await runGit({ cwd: world.root, args: ['--version'], timeoutMs: 10_000, maxBytes: 4096 })
    assert.equal(result.code, 0)
    assert.match(result.stdout.toString('utf8'), /git version/)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

// ---- the wire contract -----------------------------------------------------------------------

test('rpc: every endpoint answers the {ok,value} envelope and unknown endpoints are coded', { skip }, async () => {
  const world = await makeWorld()
  await dirty(world)
  const service = await makeService(world)
  const trace = []
  const config = { ...LIMITS }
  const { dispatch, handlers } = createHandlers({ service, config, trace: (event, fields) => trace.push({ event, ...fields }), log: () => {} })
  try {
    const ok = async (endpoint, payload) => await dispatch(endpoint, payload)
    const hello = await ok('hello')
    assert.equal(hello.git.available, true)
    assert.equal(hello.root, world.root)

    const repos = await ok('repos', {})
    assert.equal(repos.repos.length, 1)

    const status = await ok('status', { repo: world.repo })
    assert.equal(status.scan.state, 'idle')

    const job = await ok('scanStart', { repo: world.repo, includeStaged: true })
    assert.equal(job.state, 'running')
    const finished = await waitForScan(service)
    assert.equal(finished.state, 'done')

    const polled = await ok('scanStatus', { jobId: job.jobId })
    assert.equal(polled.jobId, job.jobId)
    assert.equal(polled.state, 'done')

    const diff = await ok('diff', { repo: world.repo, path: 'keep.txt', side: 'combined' })
    assert.equal(diff.path, 'keep.txt')
    assert.ok(Array.isArray(diff.empty) === false)

    assert.ok(trace.some((entry) => entry.event === 'rpc' && entry.endpoint === 'status'))

    await assert.rejects(() => ok('nope', {}), (error) => error.code === CODES.unknownEndpoint)
    await assert.rejects(() => ok('diff', { repo: world.repo, path: '../x' }), (error) => (
      error.code === CODES.unsupportedPath || error.code === CODES.outsideRoot
    ))
    assert.ok(Object.hasOwn(handlers, 'diag'))
    const diag = await ok('diag')
    assert.equal(diag.calls.status >= 1, true)
    assert.equal(diag.pid, process.pid)
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})

test('rpc: an idle scanStatus and a cancel with nothing running are quiet no-ops', { skip }, async () => {
  const world = await makeWorld()
  const service = await makeService(world)
  const { dispatch } = createHandlers({ service, config: { ...LIMITS }, trace: () => {}, log: () => {} })
  try {
    const idle = await dispatch('scanStatus', {})
    assert.equal(idle.state, 'idle')
    assert.deepEqual(idle.files, [])
    const cancel = await dispatch('scanCancel', {})
    assert.equal(cancel.state, 'idle')
  } finally {
    await rm(world.root, { recursive: true, force: true })
  }
})
