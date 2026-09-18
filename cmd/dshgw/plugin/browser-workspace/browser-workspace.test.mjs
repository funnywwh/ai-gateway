import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { apply, inject, name } from './index.js'

test('tenant plugin activation does not create an obsolete RPC broker', () => {
  let logged = false
  apply({ logger: { info() { logged = true } } })
  assert.equal(logged, true)
  assert.equal(name, 'browser-workspace')
  assert.deepEqual(inject, [])
})
test('manifest declares workspace registration services and browser entrypoint', async () => {
  const pkg = JSON.parse(await readFile(new URL('./package.json', import.meta.url), 'utf8'))
  assert.equal(pkg.exports['./client'], './client.js')
  assert.equal(pkg.dsh.client.platform, 'web')
  assert.ok(pkg.dsh.client.inject.includes('@deepseek-ai/dsh-api-workspace-controller'))
  assert.ok(pkg.dsh.client.inject.includes('@deepseek-ai/dsh-client-ui-workspace'))
})
