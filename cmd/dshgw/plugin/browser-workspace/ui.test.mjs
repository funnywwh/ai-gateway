import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

const source = await readFile(new URL('./client.js', import.meta.url), 'utf8')
const tick = () => new Promise(resolve => setImmediate(resolve))
function setup({ activateFails = false, closeFailures = 0, createFailures = 0, pickerCancels = false } = {}) {
  const events = [], requests = [], warnings = [], effects = []
  let client, Action, consent, generation = 1, count = 0, closed = 0, cleanup
  let readyResolve
  const rootReady = new Promise(resolve => { readyResolve = resolve })
  const root = { name: 'local', kind: 'directory', queryPermission: async () => 'granted' }
  const json = value => ({ ok: true, json: async () => ({ ok: true, value }) })
  const window = {
    isSecureContext: true,
    confirm(text) { consent = text; events.push('confirm'); return true },
    async showDirectoryPicker(options) {
      events.push('picker'); assert.equal(options.mode, 'readwrite')
      if (pickerCancels) throw Object.assign(new Error('cancel'), { name: 'AbortError' })
      return root
    },
    async fetch(url, options) {
      const endpoint = url.split('/').at(-1), payload = JSON.parse(options.body)
      events.push(endpoint); requests.push({ endpoint, payload })
      assert.equal(options.credentials, 'same-origin')
      if (options.signal.aborted) throw Object.assign(new Error('abort'), { name: 'AbortError' })
      if (endpoint === 'open') return json({ token: 'private-token', mountpoint: '/home/account/browser/id', id: 'id' })
      assert.equal(payload.token, 'private-token')
      if (endpoint === 'poll') {
        if (count++ === 0) return json({ requests: [{ id: 'request-1', op: 'stat', path: '' }] })
        return new Promise((_resolve, reject) => options.signal.addEventListener('abort', () => reject(Object.assign(new Error('abort'), { name: 'AbortError' })), { once: true }))
      }
      if (endpoint === 'respond') {
        assert.equal(payload.id, 'request-1'); assert.equal(payload.result.ok, true); assert.equal(payload.result.value.kind, 'directory')
        readyResolve(); return json({ accepted: true })
      }
      if (endpoint === 'activate') {
        await rootReady
        if (activateFails) return { ok: true, json: async () => ({ ok: false, error: { code: 'EIO', message: 'restart failed' } }) }
        return json({ mountpoint: '/home/account/browser/id', id: 'id' })
      }
      if (endpoint === 'close') {
        closed++
        if (closed <= closeFailures) throw new Error('temporary close failure')
        return json({ closed: true })
      }
      throw new Error('unexpected endpoint')
    },
    __ModuleLoader__: { load({ factory }) {
      client = factory(() => ({
        useState: () => [0, () => {}], useEffect: fn => effects.push(fn()),
        createElement: (tag, props, children) => ({ tag, props, children }),
      }))
    } },
  }
  vm.runInNewContext(source, { window, TextEncoder, Uint8Array, atob, btoa, AbortController, setTimeout, clearTimeout })
  const ctx = {
    logger: { warn: message => warnings.push(message) },
    connection: {
      state: { getSnapshot: () => 'connected' },
      generation: { getSnapshot: () => ({ id: generation }) },
      reconnect() { events.push('reconnect'); generation++ },
    },
    slots: {
      inject(name, fn) { assert.equal(name, 'sidebar.footer.action'); return fn() },
      register(options, component) { Action = component; return () => events.push('unregister') },
    },
    effect(fn, label) { const dispose = fn(); effects.push(dispose); if (label.endsWith('cleanup')) cleanup = dispose },
    remote: { workspace: {
      async create({ path }) {
        events.push('create'); assert.equal(path, '/home/account/browser/id'); assert.ok(generation > 1)
        if (createFailures-- > 0) throw new Error('worker reconnecting')
        return { ok: true, value: { workspace: { workspaceId: 'workspace', title: 'id' } } }
      },
      async rename({ workspaceId, title }) { events.push('rename'); assert.equal(workspaceId, 'workspace'); assert.equal(title, '本地: local'); return { ok: true } },
    } },
    uiWorkspace: { async connectWorkspace(id) { events.push('connect'); assert.equal(id, 'workspace') } },
  }
  client.apply(ctx)
  return {
    events, requests, warnings,
    click: () => Action().props.onClick(),
    text: () => Action().children,
    consent: () => consent,
    dispose: async () => { cleanup(); await tick() },
  }
}
test('UI performs consent/picker/open/poll/activate/reconnect/register/connect/close', async () => {
  const ui = setup()
  await ui.click()
  assert.match(ui.consent(), /AI 模型服务商/)
  assert.match(ui.consent(), /重启账户 worker/)
  assert.ok(ui.events.indexOf('picker') < ui.events.indexOf('open'))
  assert.ok(ui.events.indexOf('poll') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('respond') < ui.events.indexOf('create'))
  assert.ok(ui.events.indexOf('reconnect') < ui.events.indexOf('create'))
  assert.match(ui.text(), /断开 local/)
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.text(), /^已断开/)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})
test('idempotent workspace creation retries after worker reconnect failure', async () => {
  const ui = setup({ createFailures: 1 })
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'create').length, 2)
  assert.equal(ui.events.filter(e => e === 'connect').length, 1)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})
test('activation failure closes capability and never registers a workspace', async () => {
  const ui = setup({ activateFails: true })
  await ui.click()
  assert.equal(ui.events.includes('create'), false)
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.text(), /挂载失败：restart failed/)
  await ui.dispose()
})
test('failed stop retains token and offers cleanup retry without false success', async () => {
  const ui = setup({ closeFailures: 1 })
  await ui.click()
  await ui.click()
  assert.match(ui.text(), /清理未确认.*点击重试断开/)
  assert.doesNotMatch(ui.text(), /^已断开/)
  assert.equal(ui.warnings.length, 1)
  await ui.click()
  assert.match(ui.text(), /^已断开/)
  const closes = ui.requests.filter(r => r.endpoint === 'close')
  assert.equal(closes.length, 2)
  assert.equal(closes[0].payload.token, closes[1].payload.token)
  assert.equal(ui.events.filter(e => e === 'open').length, 1)
  await ui.dispose()
})
test('activation and cleanup failure preserves retry state', async () => {
  const ui = setup({ activateFails: true, closeFailures: 1 })
  await ui.click()
  assert.match(ui.text(), /清理未确认/)
  await ui.click()
  assert.match(ui.text(), /^已断开/)
  assert.equal(ui.events.includes('create'), false)
  await ui.dispose()
})
test('dispose aborts poll and sends close; failed cleanup is reported honestly', async () => {
  const ui = setup({ closeFailures: 1 })
  await ui.click()
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.equal(ui.warnings.length, 1)
  assert.match(ui.text(), /清理未确认/)
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'open').length, 1)
})
test('picker cancellation never opens a capability', async () => {
  const ui = setup({ pickerCancels: true })
  await ui.click()
  assert.equal(ui.events.includes('open'), false)
  await ui.dispose()
})
