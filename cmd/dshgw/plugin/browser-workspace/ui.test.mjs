import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

const source = await readFile(new URL('./client.js', import.meta.url), 'utf8')
const tick = () => new Promise(resolve => setImmediate(resolve))
// The bundle renders a small tree of spans; tests want the row's readable text.
function textOf(node) {
  if (node === null || node === undefined || node === false) return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textOf).join('')
  return textOf(node.children)
}
function setup({ activateFails = false, closeFailures = 0, createFailures = 0, pickerCancels = false } = {}) {
  const events = [], requests = [], warnings = [], effects = [], styles = []
  let client, Action, options, confirmText, generation = 1, count = 0, closed = 0, cleanup
  // The gateway creates the mount point at `open` and removes it again on close.
  let mountpointPresent = false
  let readyResolve
  const rootReady = new Promise(resolve => { readyResolve = resolve })
  const root = { name: 'local', kind: 'directory', queryPermission: async () => 'granted' }
  const json = value => ({ ok: true, json: async () => ({ ok: true, value }) })
  const window = {
    isSecureContext: true,
    confirm(text) { confirmText = text; events.push('confirm'); return true },
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
      if (endpoint === 'open') { mountpointPresent = true; return json({ token: 'private-token', mountpoint: '/home/account/browser/id', id: 'id' }) }
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
        mountpointPresent = false
        closed++
        if (closed <= closeFailures) throw new Error('temporary close failure')
        return json({ closed: true })
      }
      throw new Error('unexpected endpoint')
    },
    __ModuleLoader__: { load({ factory }) {
      client = factory(() => ({
        useState: () => [0, () => {}], useEffect: fn => effects.push(fn()),
        createElement: (type, props, ...children) => ({ type, props, children }),
      }))
    } },
  }
  const document = {
    querySelector: () => null,
    createElement: () => ({ dataset: {}, textContent: '' }),
    head: { appendChild: tag => styles.push(tag) },
  }
  vm.runInNewContext(source, { window, document, TextEncoder, Uint8Array, atob, btoa, AbortController, setTimeout, clearTimeout })
  const ctx = {
    logger: { warn: message => warnings.push(message) },
    connection: {
      state: { getSnapshot: () => 'connected' },
      generation: { getSnapshot: () => ({ id: generation }) },
      reconnect() { events.push('reconnect'); generation++ },
    },
    slots: {
      inject(name, fn) { assert.equal(name, 'sidebar.footer.action'); return fn() },
      register(entry, component) { options = entry; Action = component; return () => events.push('unregister') },
    },
    effect(fn, label) { const dispose = fn(); effects.push(dispose); if (label.endsWith('cleanup')) cleanup = dispose },
    remote: { workspace: {
      async create({ path }) {
        events.push('create'); assert.equal(path, '/home/account/browser/id')
        // Registration must happen while the mount point exists, i.e. before the
        // activation restart and before any teardown removed the directory.
        assert.ok(mountpointPresent, 'workspace registered after the mount point was removed')
        assert.equal(generation, 1, 'workspace registration must precede the activation restart')
        if (createFailures-- > 0) throw new Error('workspace registry busy')
        return { ok: true, value: { workspace: { workspaceId: 'workspace', title: 'id' } } }
      },
      async rename({ workspaceId, title }) { events.push('rename'); assert.equal(workspaceId, 'workspace'); assert.equal(title, '本地: local'); return { ok: true } },
      async delete(workspaceId) { events.push('delete'); assert.equal(workspaceId, 'workspace') },
    } },
    uiWorkspace: { async connectWorkspace(id) { events.push('connect'); assert.equal(id, 'workspace') } },
  }
  client.apply(ctx)
  return {
    events, requests, warnings, styles,
    click: () => Action().props.onClick(),
    element: () => Action(),
    text: () => textOf(Action()),
    title: () => Action().props.title,
    options: () => options,
    confirm: () => confirmText,
    dispose: async () => { cleanup(); await tick() },
  }
}
test('the sidebar entry is one ssh-style row above the ssh workspace, with no consent step', async () => {
  const ui = setup()
  assert.equal(ui.options().name, 'sidebar.footer.action')
  assert.equal(ui.options().id, 'browser-workspace')
  assert.equal(ui.options().label, '浏览器工作区')
  // The ssh-workspace entry registers the same slot with order 100; ascending order puts
  // this row above it.
  assert.ok(ui.options().order < 100, `order ${ui.options().order} must precede the ssh entry`)
  const element = ui.element()
  assert.equal(element.type, 'button')
  assert.equal(element.props.className, 'dshgw-bw-action')
  assert.equal(element.props['aria-pressed'], false)
  assert.equal(ui.text(), '🖥浏览器工作区')
  // The warning that used to be a separate click now travels with the row.
  assert.match(ui.title(), /AI 模型服务商/)
  assert.match(ui.title(), /重启该账号的 worker/)
  assert.equal(ui.styles.length, 1, 'the stylesheet is installed once')
  assert.equal(ui.styles[0].dataset.pluginCss, 'dshgw-browser-workspace/client.css')
  await ui.dispose()
})
test('one click runs picker/open/poll/activate/reconnect/register/connect, the next disconnects', async () => {
  const ui = setup()
  const pending = ui.click()
  // showDirectoryPicker must be invoked synchronously by the click: awaiting anything
  // first would let the browser's transient user activation expire (the reported
  // "Must be handling a user gesture" failure).
  assert.equal(ui.events.at(-1), 'picker')
  await pending
  assert.equal(ui.events.includes('confirm'), false)
  assert.equal(ui.confirm(), undefined)
  assert.ok(ui.events.indexOf('picker') < ui.events.indexOf('open'))
  // The mount point exists from `open` until a teardown removes it, so the
  // workspace is registered before the activation restart, not after it.
  assert.ok(ui.events.indexOf('open') < ui.events.indexOf('create'))
  assert.ok(ui.events.indexOf('create') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('poll') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('create') < ui.events.indexOf('reconnect'))
  assert.ok(ui.events.indexOf('reconnect') < ui.events.indexOf('rename'))
  assert.ok(ui.events.indexOf('reconnect') < ui.events.indexOf('connect'))
  assert.match(ui.text(), /已挂载 local（读写）；点击断开/)
  assert.equal(ui.element().props['aria-pressed'], true)
  assert.match(ui.title(), /当前：已挂载/)
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.text(), /已断开/)
  assert.equal(ui.element().props['aria-pressed'], false)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})
test('idempotent workspace creation retries a transient registration failure', async () => {
  const ui = setup({ createFailures: 1 })
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'create').length, 2)
  assert.equal(ui.events.filter(e => e === 'connect').length, 1)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})
test('activation failure releases the capability and forgets the workspace it registered', async () => {
  const ui = setup({ activateFails: true })
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'create').length, 1)
  assert.equal(ui.events.includes('connect'), false)
  assert.equal(ui.events.filter(e => e === 'delete').length, 1)
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.text(), /挂载失败：restart failed/)
  await ui.dispose()
})
test('failed stop retains token and offers cleanup retry without false success', async () => {
  const ui = setup({ closeFailures: 1 })
  await ui.click()
  await ui.click()
  assert.match(ui.text(), /清理未确认.*点击重试断开/)
  assert.doesNotMatch(ui.text(), /已断开/)
  assert.equal(ui.warnings.length, 1)
  await ui.click()
  assert.match(ui.text(), /已断开/)
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
  assert.match(ui.text(), /已断开/)
  assert.equal(ui.events.includes('connect'), false)
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
