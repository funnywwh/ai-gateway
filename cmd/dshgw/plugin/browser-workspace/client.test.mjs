import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

let client
vm.runInNewContext(await readFile(new URL('./client.js', import.meta.url), 'utf8'), {
  window: { __ModuleLoader__: { load({ id, factory }) { assert.equal(id, 'dshgw-browser-workspace'); client = factory(() => ({})) } } },
  TextEncoder, Uint8Array, atob, btoa, AbortController, setTimeout, clearTimeout,
})
const dom = name => Object.assign(new Error(name), { name })
function directory(name = 'root') {
  const children = new Map()
  return {
    kind: 'directory', name, children,
    async queryPermission() { return 'granted' },
    async getDirectoryHandle(name, { create = false } = {}) {
      if (!children.has(name) && create) children.set(name, directory(name))
      const child = children.get(name)
      if (!child) throw dom('NotFoundError')
      if (child.kind !== 'directory') throw dom('TypeMismatchError')
      return child
    },
    async getFileHandle(name, { create = false } = {}) {
      if (!children.has(name) && create) {
        let data = new Uint8Array()
        children.set(name, { kind: 'file', name,
          async getFile() { return Object.assign(new Blob([data]), { lastModified: 123 }) },
          async createWritable(options) {
            assert.equal(options.keepExistingData, true)
            let staged = data.slice()
            const resize = size => { const next = new Uint8Array(size); next.set(staged.subarray(0, size)); staged = next }
            return {
              async write({ type, position, data: block }) { assert.equal(type, 'write'); if (position + block.length > staged.length) resize(position + block.length); staged.set(block, position) },
              async truncate(size) { resize(size) },
              async close() { data = staged }, async abort() {},
            }
          },
        })
      }
      const child = children.get(name)
      if (!child) throw dom('NotFoundError')
      if (child.kind !== 'file') throw dom('TypeMismatchError')
      return child
    },
    async removeEntry(name, options) {
      assert.equal(options.recursive, false)
      const child = children.get(name)
      if (!child) throw dom('NotFoundError')
      if (child.kind === 'directory' && child.children.size) throw dom('InvalidModificationError')
      children.delete(name)
    },
    async *entries() { yield* children.entries() },
  }
}
const b64 = bytes => Buffer.from(bytes).toString('base64')
test('strict relative path validation rejects traversal before normalization', () => {
  for (const path of ['.', '..', '../x', '/x', 'a//b', 'a/', 'C:x', 'a\\b', 'a\0b', '😀'.repeat(70), undefined, 3]) assert.throws(() => client.checkRelativePath(path), { code: 'EINVAL' })
  for (const path of ['', 'a/b', '.git/config', '%2e%2e/x']) assert.equal(client.checkRelativePath(path), path)
  assert.throws(() => client.checkRelativePath('', false))
})
test('binary reads, offset writes, sparse extension, truncation and metadata', async () => {
  const root = directory()
  const execute = client.createExecutor(root, { writable: true })
  await execute({ op: 'mkdir', path: 'src' })
  await execute({ op: 'create', path: 'src/file' })
  await execute({ op: 'write', path: 'src/file', offset: 0, data: b64([0, 255, 128, 10]) })
  await execute({ op: 'write', path: 'src/file', offset: 1, data: b64([42]) })
  assert.equal((await execute({ op: 'read', path: 'src/file', offset: 0, size: 9 })).data, b64([0, 42, 128, 10]))
  assert.equal((await execute({ op: 'read', path: 'src/file', offset: 2, size: 1 })).data, b64([128]))
  assert.equal((await execute({ op: 'read', path: 'src/file', offset: 50, size: 5 })).bytes, 0)
  await execute({ op: 'write', path: 'src/file', offset: 6, data: b64([7]) })
  assert.equal((await execute({ op: 'read', path: 'src/file', offset: 4, size: 3 })).data, b64([0, 0, 7]))
  await execute({ op: 'truncate', path: 'src/file', size: 2 })
  const info = await execute({ op: 'stat', path: 'src/file' })
  assert.equal(info.size, 2); assert.equal(info.lastModified, 123)
  const entry = (await execute({ op: 'list', path: 'src' })).entries[0]
  assert.equal(entry.size, 2); assert.equal(entry.lastModified, 123)
  await execute({ op: 'flush', path: 'src/file' })
  await assert.rejects(execute({ op: 'rmdir', path: 'src' }), { name: 'InvalidModificationError' })
  await execute({ op: 'unlink', path: 'src/file' })
  await execute({ op: 'rmdir', path: 'src' })
  assert.equal(root.children.size, 0)
})
test('mutations serialize so parallel writes do not lose updates', async () => {
  const execute = client.createExecutor(directory(), { writable: true })
  await execute({ op: 'create', path: 'x' })
  await Promise.all([execute({ op: 'write', path: 'x', offset: 0, data: b64([1]) }), execute({ op: 'write', path: 'x', offset: 1, data: b64([2]) })])
  assert.equal((await execute({ op: 'read', path: 'x', size: 2 })).data, b64([1, 2]))
})
test('read-only, revoked permission, range/base64 errors fail before traversal', async () => {
  let traversals = 0
  const root = { getDirectoryHandle() { traversals++; throw Error('unexpected') }, queryPermission: async () => 'granted' }
  const readOnly = client.createExecutor(root)
  await assert.rejects(readOnly({ op: 'write', path: 'a/file', data: '' }), { code: 'EROFS' })
  const execute = client.createExecutor(root, { writable: true })
  for (const data of ['invalid', 'YQ=', 'YR==', b64(new Uint8Array(1024 * 1024 + 1))]) await assert.rejects(execute({ op: 'write', path: 'a/x', data }), { code: 'EINVAL' })
  for (const offset of [-1, 0.5, Number.MAX_SAFE_INTEGER + 1]) await assert.rejects(execute({ op: 'read', path: 'a/x', offset, size: 1 }), { code: 'EINVAL' })
  await assert.rejects(execute({ op: 'read', path: 'a/x', size: 1024 * 1024 + 1 }), { code: 'EINVAL' })
  await assert.rejects(execute({ op: 'rename', path: 'a/x', target: '../escape' }), { code: 'EINVAL' })
  root.queryPermission = async () => 'denied'
  await assert.rejects(execute({ op: 'list', path: 'a' }), { code: 'EACCES' })
  assert.equal(traversals, 0)
})
test('rename never falls back to copy-delete and unlink respects kind', async () => {
  const root = directory()
  const execute = client.createExecutor(root, { writable: true })
  await execute({ op: 'create', path: 'x' })
  await assert.rejects(execute({ op: 'create', path: 'x', exclusive: true }), { code: 'EEXIST' })
  await assert.rejects(execute({ op: 'rename', path: 'x', target: 'y' }), { code: 'ENOTSUP' })
  assert.equal(root.children.has('x'), true); assert.equal(root.children.has('y'), false)
  const handle = root.children.get('x')
  handle.move = async (parent, name) => { parent.children.set(name, handle); root.children.delete('x') }
  await execute({ op: 'rename', path: 'x', target: 'y' })
  assert.equal(root.children.has('y'), true)
  await execute({ op: 'mkdir', path: 'dir' })
  await assert.rejects(execute({ op: 'unlink', path: 'dir' }), { code: 'EISDIR' })
  await assert.rejects(execute({ op: 'rmdir', path: 'y' }), { code: 'ENOTDIR' })
})
test('create flags match Go defaults, preserve existing contents and support truncate', async () => {
  const execute = client.createExecutor(directory(), { writable: true })
  await execute({ op: 'create', path: 'x', exclusive: true })
  await execute({ op: 'write', path: 'x', data: b64([1, 2, 3]) })
  assert.equal((await execute({ op: 'create', path: 'x' })).size, 3)
  assert.equal((await execute({ op: 'create', path: 'x', exclusive: false, truncate: false })).size, 3)
  await assert.rejects(execute({ op: 'create', path: 'x', exclusive: true, truncate: true }), { code: 'EEXIST' })
  assert.equal((await execute({ op: 'stat', path: 'x' })).size, 3)
  assert.equal((await execute({ op: 'create', path: 'x', truncate: true })).size, 0)
  assert.equal((await execute({ op: 'create', path: 'new', truncate: true })).size, 0)
  await execute({ op: 'mkdir', path: 'dir' })
  await assert.rejects(execute({ op: 'create', path: 'dir' }), { code: 'EISDIR' })
  await assert.rejects(execute({ op: 'create', path: 'x', truncate: 'true' }), { code: 'EINVAL' })
})
test('HTTP adapter uses same-origin relative POST and validates envelopes', async () => {
  const call = client.createTransport(async (url, options) => {
    assert.equal(url, './browser-workspace/open'); assert.equal(options.credentials, 'same-origin'); assert.equal(options.redirect, 'error'); assert.equal(options.method, 'POST')
    assert.equal(JSON.parse(options.body).name, 'root')
    return { ok: true, json: async () => ({ ok: true, value: { token: 'secret' } }) }
  })
  assert.equal((await call('open', { name: 'root' })).token, 'secret')
  await assert.rejects(call('../evil', {}), { code: 'EINVAL' })
  await assert.rejects(client.createTransport(async () => ({ ok: false, status: 401 }))('poll', {}), /HTTP 401/)
  await assert.rejects(client.createTransport(async () => ({ ok: true, json: async () => ({ ok: false, error: { code: 'EACCES', message: 'denied' } }) }))('poll', {}), { code: 'EACCES' })
})
test('HTTP adapter aborts on timeout and maps DOM failures to errno', async () => {
  const call = client.createTransport((_url, { signal }) => new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(dom('AbortError')))))
  await assert.rejects(call('poll', {}, 5), { name: 'AbortError' })
  assert.equal(client.errorOf(dom('NotFoundError')).code, 'ENOENT')
  assert.equal(client.errorOf(dom('NotAllowedError')).code, 'EACCES')
})
