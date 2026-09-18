import test from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import { readFile } from 'node:fs/promises'

let client
vm.runInNewContext(await readFile(new URL('./client.js', import.meta.url), 'utf8'), {
  window: { __ModuleLoader__: { load({ factory }) { client = factory(() => ({})) } } },
  TextEncoder, Uint8Array, atob, btoa, AbortController, setTimeout, clearTimeout,
})

test('maximum 1 MiB binary block decodes without regexp stack exhaustion', async () => {
  let written = 0
  const handle = {
    kind: 'file',
    async createWritable() { return {
      async write({ data }) { written = data.length }, async close() {}, async abort() {},
    } },
  }
  const root = {
    async queryPermission() { return 'granted' },
    async getDirectoryHandle() { throw Object.assign(new Error('file'), { name: 'TypeMismatchError' }) },
    async getFileHandle() { return handle },
  }
  const execute = client.createExecutor(root, { writable: true })
  const result = await execute({ op: 'write', path: 'large.bin', offset: 0, data: Buffer.alloc(1024 * 1024, 255).toString('base64') })
  assert.equal(result.bytes, 1024 * 1024)
  assert.equal(written, 1024 * 1024)
})
