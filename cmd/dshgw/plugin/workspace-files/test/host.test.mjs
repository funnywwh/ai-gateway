// Host-half tests: the workspace clamp, the read/write limits, and the endpoint table.
//
// Everything runs against a real temporary workspace on a real filesystem — the service has no dsh
// imports precisely so this is possible — and the endpoint table is driven through the same
// dispatcher the RPC channel calls, so a pass means the wire contract works, not just the helpers.
//
// Run: npm test   (node --test test/)

import { strict as assert } from 'node:assert'
import { mkdtemp, mkdir, readFile, rm, stat, symlink, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

import { CODES, WorkspaceFiles, looksBinary, normalizeName, normalizeRelPath } from '../fs-service.js'
import { readConfig } from '../index.js'
import { createHandlers } from '../rpc-handlers.js'

const PLUGIN_DIR = join(dirname(fileURLToPath(import.meta.url)), '..')

const LIMITS = {
  maxTextBytes: 64 * 1024,
  chunkBytes: 4096,
  maxListEntries: 500,
  maxUploadBytes: 1024 * 1024,
  maxSearchResults: 50,
  maxSearchDepth: 4,
  maxSearchVisits: 5000,
}

/** One populated workspace: a text file, a nested directory, a hidden file, and a binary blob. */
async function makeWorkspace({ withEscapeLink = true } = {}) {
  const root = await mkdtemp(join(tmpdir(), 'wsf-'))
  await writeFile(join(root, 'hello.txt'), 'hello 工作区\nsecond line\n')
  await writeFile(join(root, 'blob.bin'), Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe]))
  await writeFile(join(root, 'big.txt'), 'x'.repeat(200_000))
  await mkdir(join(root, '视频'), { recursive: true })
  await writeFile(join(root, '视频', 'clip.mp4'), Buffer.from('not really a video'))
  await mkdir(join(root, 'sub', 'deep'), { recursive: true })
  await writeFile(join(root, 'sub', 'deep', 'note.md'), '# note\n')
  await mkdir(join(root, '.hidden-dir'), { recursive: true })
  await writeFile(join(root, '.hidden-dir', 'secret.txt'), 'secret')
  if (withEscapeLink) {
    const outside = await mkdtemp(join(tmpdir(), 'wsf-outside-'))
    await writeFile(join(outside, 'escape.txt'), 'outside')
    await symlink(outside, join(root, 'escape-link'))
  }
  return root
}

/** One live service plus the endpoint table over it. */
async function makeHost(options = {}) {
  const root = options.root ?? await makeWorkspace()
  const service = await WorkspaceFiles.create({
    root,
    readOnly: options.readOnly === true,
    limits: { ...LIMITS, ...(options.limits ?? {}) },
    log: () => {},
  })
  const config = { version: 'test', pluginDir: '/tmp', traceFile: null, trace: false, ...LIMITS }
  const { dispatch } = createHandlers({ service, config, trace: () => {}, log: () => {} })
  return {
    root,
    service,
    dispatch,
    /** Exactly the shape the RPC channel returns to the browser. */
    async call(endpoint, payload) {
      try {
        return { ok: true, value: await dispatch(endpoint, payload) }
      } catch (error) {
        return { ok: false, error: { code: error?.code ?? 'files/failed', message: String(error?.message ?? error), details: error?.details ?? {} } }
      }
    },
  }
}

/** Assert one call failed with the expected code. */
async function expectFailure(host, endpoint, payload, code) {
  const result = await host.call(endpoint, payload)
  assert.equal(result.ok, false, `${endpoint} should have failed`)
  assert.equal(result.error.code, code, `${endpoint} failure code (${result.error.message})`)
  return result.error
}

test('path vocabulary: relative segments only, no traversal, no absolute paths', () => {
  assert.equal(normalizeRelPath(''), '')
  assert.equal(normalizeRelPath('a/b.txt'), 'a/b.txt')
  assert.equal(normalizeRelPath('a/b.txt/'), 'a/b.txt')
  assert.equal(normalizeRelPath(undefined), '')
  assert.throws(() => normalizeRelPath('..'), (error) => error.code === CODES.badPath)
  assert.throws(() => normalizeRelPath('a/../../etc/passwd'), (error) => error.code === CODES.badPath)
  assert.throws(() => normalizeRelPath('/etc/passwd'), (error) => error.code === CODES.badPath)
  assert.throws(() => normalizeRelPath('a//b'), (error) => error.code === CODES.badPath)
  assert.throws(() => normalizeRelPath('a\\b'), (error) => error.code === CODES.badPath)
  assert.throws(() => normalizeRelPath('a\u0000b'), (error) => error.code === CODES.badPath)
  assert.equal(normalizeName(' 新目录 '), '新目录')
  assert.throws(() => normalizeName('..'), (error) => error.code === CODES.badPath)
  assert.throws(() => normalizeName('a/b'), (error) => error.code === CODES.badPath)
  assert.equal(looksBinary(Buffer.from('plain text')), false)
  assert.equal(looksBinary(Buffer.from([0x41, 0x00, 0x42])), true)
  assert.equal(looksBinary(Buffer.from([0xff, 0xfe, 0xfd])), true)
})

test('the root clamp holds for reads, writes, and symlinks that leave the workspace', async () => {
  const host = await makeHost()
  try {
    // Reads outside the workspace are refused at the vocabulary level and at the symlink level.
    await expectFailure(host, 'list', { path: '../' }, CODES.badPath)
    await expectFailure(host, 'readText', { path: '/etc/hostname' }, CODES.badPath)
    const linkError = await expectFailure(host, 'list', { path: 'escape-link' }, CODES.outsideRoot)
    assert.match(linkError.message, /工作区/)
    await expectFailure(host, 'readText', { path: 'escape-link/escape.txt' }, CODES.outsideRoot)

    // Writes cannot create a path that resolves outside, and cannot touch the root itself.
    await expectFailure(host, 'writeText', { path: '../escape.txt', text: 'nope' }, CODES.badPath)
    await expectFailure(host, 'writeText', { path: '', text: 'nope' }, CODES.badPath)
    await expectFailure(host, 'mkdir', { path: '', name: '..' }, CODES.badPath)
    await expectFailure(host, 'remove', { path: '' }, CODES.badPath)
    await expectFailure(host, 'rename', { path: 'hello.txt', name: '../moved.txt' }, CODES.badPath)

    // A legitimate nested create still works, so the clamp is not simply refusing everything.
    const created = await host.call('mkdir', { path: 'sub', name: 'fresh' })
    assert.equal(created.ok, true)
    assert.equal(created.value.path, 'sub/fresh')
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('listing: directories first, hidden entries opt-in, sizes and kinds reported', async () => {
  const host = await makeHost()
  try {
    const listed = await host.call('list', { path: '' })
    assert.equal(listed.ok, true)
    const names = listed.value.entries.map((entry) => entry.name)
    assert.deepEqual(names.slice(0, 3), ['sub', '视频', 'escape-link'].sort((a, b) => a.localeCompare(b, 'zh-Hans-CN')), 'directories lead the listing')
    assert.equal(names.includes('hello.txt'), true)
    assert.equal(names.includes('.hidden-dir'), false, 'dot entries stay hidden by default')
    assert.equal(listed.value.hidden, 1)
    assert.equal(listed.value.parent, null)
    assert.equal(listed.value.path, '')
    const video = listed.value.entries.find((entry) => entry.name === '视频')
    assert.equal(video.kind, 'dir')
    assert.equal(video.size, 0)
    const text = listed.value.entries.find((entry) => entry.name === 'hello.txt')
    assert.equal(text.kind, 'file')
    assert.equal(text.size, Buffer.byteLength('hello 工作区\nsecond line\n'))
    const link = listed.value.entries.find((entry) => entry.name === 'escape-link')
    assert.equal(link.kind, 'dir')
    assert.equal(link.outside, true, 'a symlink leaving the workspace is flagged')

    const withHidden = await host.call('list', { path: '', showHidden: true })
    assert.equal(withHidden.value.entries.some((entry) => entry.name === '.hidden-dir'), true)

    await expectFailure(host, 'list', { path: 'hello.txt' }, CODES.notDirectory)
    await expectFailure(host, 'list', { path: 'nope' }, CODES.notFound)
    await expectFailure(host, 'stat', { path: 'nope' }, CODES.notFound)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('text reads are bounded and refuse binary files', async () => {
  const host = await makeHost()
  try {
    const page = await host.call('readText', { path: 'hello.txt' })
    assert.equal(page.ok, true)
    assert.equal(page.value.text, 'hello 工作区\nsecond line\n')
    assert.equal(page.value.size, Buffer.byteLength(page.value.text))
    await expectFailure(host, 'readText', { path: 'blob.bin' }, CODES.binary)
    const tooLarge = await expectFailure(host, 'readText', { path: 'big.txt' }, CODES.tooLarge)
    assert.equal(tooLarge.details.limit, LIMITS.maxTextBytes)
    await expectFailure(host, 'readText', { path: '视频' }, CODES.isDirectory)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('text writes create, update, and refuse to clobber an unseen version', async () => {
  const host = await makeHost()
  try {
    const created = await host.call('writeText', { path: 'sub/new.txt', text: '第一次' })
    assert.equal(created.ok, true)
    assert.equal(created.value.created, true)
    assert.equal(await readFile(join(host.root, 'sub', 'new.txt'), 'utf8'), '第一次')

    const updated = await host.call('writeText', { path: 'sub/new.txt', text: '第二次', expectedMtimeMs: created.value.mtimeMs })
    assert.equal(updated.ok, true)
    assert.equal(updated.value.created, false)
    assert.equal(await readFile(join(host.root, 'sub', 'new.txt'), 'utf8'), '第二次')

    // A stale editor (one that read the first version) must not overwrite the second.
    await expectFailure(host, 'writeText', { path: 'sub/new.txt', text: '旧的', expectedMtimeMs: created.value.mtimeMs }, CODES.exists)
    assert.equal(await readFile(join(host.root, 'sub', 'new.txt'), 'utf8'), '第二次')

    await expectFailure(host, 'writeText', { path: 'sub', text: 'x' }, CODES.isDirectory)
    await expectFailure(host, 'writeText', { path: 'gone/new.txt', text: 'x' }, CODES.notFound)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('chunked transfer: readChunk and writeChunk round-trip a multi-chunk file', async () => {
  const host = await makeHost()
  try {
    const payload = Buffer.alloc(9000)
    for (let index = 0; index < payload.length; index += 1) payload[index] = (index * 31) % 256
    await writeFile(join(host.root, 'payload.bin'), payload)

    const parts = []
    let offset = 0
    for (;;) {
      const page = await host.call('readChunk', { path: 'payload.bin', offset, length: LIMITS.chunkBytes })
      assert.equal(page.ok, true)
      assert.equal(page.value.offset, offset)
      const bytes = Buffer.from(page.value.bytes, 'base64')
      parts.push(bytes)
      offset = page.value.offset + bytes.length
      if (page.value.eof) break
      assert.ok(bytes.length > 0, 'a non-final chunk is never empty')
    }
    assert.deepEqual(Buffer.concat(parts), payload, 'the chunk loop reassembles the file byte for byte')

    // Writing back through chunks: offset 0 truncates, later offsets extend.
    const target = 'uploaded.bin'
    for (let cursor = 0; cursor < payload.length; cursor += LIMITS.chunkBytes) {
      const slice = payload.subarray(cursor, cursor + LIMITS.chunkBytes)
      const written = await host.call('writeChunk', { path: target, offset: cursor, data: slice.toString('base64') })
      assert.equal(written.ok, true)
      assert.equal(written.value.written, slice.length)
    }
    assert.deepEqual(await readFile(join(host.root, target)), payload)

    // Truncation is explicit: writing at offset 0 again shortens the file.
    await host.call('writeChunk', { path: target, offset: 0, data: Buffer.from('short').toString('base64') })
    assert.equal(await readFile(join(host.root, target), 'utf8'), 'short')

    await expectFailure(host, 'writeChunk', { path: target, offset: 0, data: 'A'.repeat(100_000) }, CODES.tooLarge)
    await expectFailure(host, 'readChunk', { path: '视频' }, CODES.isDirectory)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('mkdir, rename and remove keep the workspace consistent', async () => {
  const host = await makeHost()
  try {
    const made = await host.call('mkdir', { path: 'sub', name: 'one' })
    assert.equal(made.value.kind, 'dir')
    await expectFailure(host, 'mkdir', { path: 'sub', name: 'one' }, CODES.exists)
    await expectFailure(host, 'mkdir', { path: 'nope', name: 'two' }, CODES.notFound)

    const renamed = await host.call('rename', { path: 'sub/one', name: 'uno' })
    assert.equal(renamed.value.path, 'sub/uno')
    await expectFailure(host, 'rename', { path: 'sub/uno', name: 'deep' }, CODES.exists)
    await expectFailure(host, 'rename', { path: 'sub/nope', name: 'x' }, CODES.notFound)

    // A non-empty directory needs the explicit recursive flag.
    await writeFile(join(host.root, 'sub', 'uno', 'inside.txt'), 'x')
    await expectFailure(host, 'remove', { path: 'sub/uno' }, CODES.notEmpty)
    const removed = await host.call('remove', { path: 'sub/uno', recursive: true })
    assert.equal(removed.value.kind, 'dir')
    await expectFailure(host, 'stat', { path: 'sub/uno' }, CODES.notFound)

    const fileRemoved = await host.call('remove', { path: 'hello.txt' })
    assert.equal(fileRemoved.value.kind, 'file')
    await expectFailure(host, 'stat', { path: 'hello.txt' }, CODES.notFound)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('search walks the workspace breadth-first and stays bounded', async () => {
  const host = await makeHost()
  try {
    const notes = await host.call('find', { query: 'note' })
    assert.equal(notes.ok, true)
    assert.deepEqual(notes.value.matches.map((entry) => entry.path), ['sub/deep/note.md'])
    assert.equal(notes.value.matches[0].kind, 'file')
    const hidden = await host.call('find', { query: 'secret' })
    assert.deepEqual(hidden.value.matches, [], 'hidden directories are not descended into by default')
    const withHidden = await host.call('find', { query: 'secret', showHidden: true })
    assert.deepEqual(withHidden.value.matches.map((entry) => entry.path), ['.hidden-dir/secret.txt'])
    const empty = await host.call('find', { query: '   ' })
    assert.deepEqual(empty.value.matches, [])
    const capped = await host.call('find', { query: '.', limit: 1 })
    assert.equal(capped.value.matches.length, 1)
    assert.equal(capped.value.truncated, true)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('read-only mode refuses every mutation and says so in hello()', async () => {
  const host = await makeHost({ readOnly: true })
  try {
    const hello = await host.call('hello')
    assert.equal(hello.value.readOnly, true)
    assert.equal(hello.value.root, host.root)
    await expectFailure(host, 'writeText', { path: 'x.txt', text: 'x' }, CODES.readOnly)
    await expectFailure(host, 'mkdir', { path: '', name: 'x' }, CODES.readOnly)
    await expectFailure(host, 'rename', { path: 'hello.txt', name: 'x.txt' }, CODES.readOnly)
    await expectFailure(host, 'remove', { path: 'hello.txt' }, CODES.readOnly)
    const read = await host.call('readText', { path: 'hello.txt' })
    assert.equal(read.ok, true, 'reads still work in read-only mode')
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('the endpoint table rejects an unknown endpoint and reports diagnostics', async () => {
  const host = await makeHost()
  try {
    await expectFailure(host, 'nope', {}, 'files/unknown-endpoint')
    const diag = await host.call('diag')
    assert.equal(diag.ok, true)
    assert.equal(diag.value.config.root, host.root)
    assert.equal(diag.value.config.limits.chunkBytes, LIMITS.chunkBytes)
    assert.ok((diag.value.calls.hello ?? 0) >= 0)
  } finally {
    await rm(host.root, { recursive: true, force: true })
  }
})

test('activation fails loudly when the configured root does not exist', async () => {
  await assert.rejects(
    () => WorkspaceFiles.create({ root: join(tmpdir(), 'wsf-does-not-exist-9f8a7b6c'), limits: LIMITS }),
    (error) => error.code === CODES.io,
  )
  // A file as the root is refused too: the whole surface lists directories.
  const file = join(await mkdtemp(join(tmpdir(), 'wsf-rootfile-')), 'file.txt')
  await writeFile(file, 'x')
  const stats = await stat(file)
  assert.equal(stats.isFile(), true)
  await assert.rejects(() => WorkspaceFiles.create({ root: file, limits: LIMITS }), (error) => error.code === CODES.notDirectory)
})

test('config: the row may name the trace file, and an out-of-tree row keeps the module\'s own', () => {
  // Every tenant shares one installed copy (M75), so the gateway names a per-tenant trace; a row
  // that stays silent keeps the $DSH_HOME/plugins default, and a relative path is not a path.
  const own = join(PLUGIN_DIR, 'trace.jsonl')
  assert.equal(readConfig({}).traceFile, own)
  assert.equal(readConfig({ root: '/tmp' }).traceFile, own)
  assert.equal(readConfig({ traceFile: '/var/lib/dshgw/plugin-state/workspace-files.trace.jsonl' }).traceFile, '/var/lib/dshgw/plugin-state/workspace-files.trace.jsonl')
  assert.equal(readConfig({ traceFile: 'trace.jsonl' }).traceFile, own)
  assert.equal(readConfig({ traceFile: '' }).traceFile, own)
  // The root and its label are the row's, and the clamp is what the service gets.
  const named = readConfig({ root: '/tmp/workspace', rootLabel: '工作区' })
  assert.equal(named.root, '/tmp/workspace')
  assert.equal(named.rootLabel, '工作区')
})
