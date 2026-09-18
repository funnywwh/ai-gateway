import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

let client
vm.runInNewContext(await readFile(new URL('./client.js', import.meta.url), 'utf8'), {
  window: { __ModuleLoader__: { load({ factory }) { client = factory(() => ({})) } } },
  TextEncoder, Uint8Array, atob, btoa, AbortController, setTimeout, clearTimeout,
})
test('Go omitted data represents a zero-byte write without extending file', async () => {
  let writes = 0
  const root = {
    queryPermission: async () => 'granted',
    getDirectoryHandle: async () => { throw Object.assign(new Error('file'), { name: 'TypeMismatchError' }) },
    getFileHandle: async () => ({ kind: 'file', createWritable: async () => { writes++; throw new Error('unexpected write') } }),
  }
  const execute = client.createExecutor(root, { writable: true })
  assert.equal((await execute({ op: 'write', path: 'file', offset: 500 })).bytes, 0)
  assert.equal(writes, 0)
})
test('list bounds encoded JSON bytes below HTTP body limit without truncating', async () => {
  let visited = 0
  const root = {
    kind: 'directory', queryPermission: async () => 'granted',
    async *entries() {
      // Less than 10000 entries, but enough escaped/UTF-8 bytes to exceed 1 MiB.
      for (let n = 0; n < 5000; n++) { visited++; yield [`${n}-${'界'.repeat(80)}`, { kind: 'directory' }] }
    },
  }
  const execute = client.createExecutor(root)
  await assert.rejects(execute({ op: 'list', path: '' }), { code: 'EOVERFLOW' })
  assert.ok(visited < 5000)
})
