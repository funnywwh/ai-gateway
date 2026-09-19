// What a page does when its connection dies, and what the NEXT page does with the mount the
// previous one left behind. Both are the same mechanism seen from two sides:
//
//   · inside one page, a transport failure is recovered in place (resume, keep serving) —
//     no reload, no second directory choice, no lost mount;
//   · in a new document (a reload, or a tab that replaced the old one), the stored record
//     puts the row into "可恢复" and one click takes the SAME mount back — it does not open
//     the picker and it does not rebuild anything.
//
// The harness mirrors ui.test.mjs, plus the two browser services this feature needs:
// IndexedDB (where the capability token and the directory handle live between documents) and
// window.location.origin (which keys the record).
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

const source = await readFile(new URL('./client.js', import.meta.url), 'utf8')
const tick = () => new Promise(resolve => setImmediate(resolve))
const textOf = node => {
  if (node === null || node === undefined || node === false) return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textOf).join('')
  return textOf(node.children)
}

// A small IndexedDB stand-in. It keeps the two properties the plugin depends on:
//
//   · a value written by one document is readable by the next (the fake is shared by the
//     tests that model exactly that);
//   · a request's onsuccess runs BEFORE its transaction's oncomplete — the order real
//     IndexedDB guarantees and the order `withStore` resolves on. A stand-in that fires
//     oncomplete first makes every read look like "no record" and every write look fine.
function fakeIndexedDB(initial = {}) {
  const data = new Map(Object.entries(initial))
  const request = work => {
    const handle = { result: undefined, onsuccess: null, onerror: null }
    setImmediate(() => { handle.result = work(); handle.onsuccess?.() })
    return handle
  }
  return {
    data,
    open() {
      const opening = { result: null, onsuccess: null, onerror: null, onupgradeneeded: null, onblocked: null }
      setImmediate(() => {
        opening.result = {
          objectStoreNames: { contains: () => true },
          createObjectStore: () => ({}),
          close() {},
          transaction() {
            const tx = { oncomplete: null, onerror: null, onabort: null, error: null }
            // The transaction completes only once nothing is outstanding. Every request routes
            // through `tx.done`, which re-checks that count when it finishes.
            let outstanding = 0
            const settle = () => { if (outstanding === 0) setImmediate(() => tx.oncomplete?.()) }
            tx.done = fn => {
              outstanding += 1
              setImmediate(() => { fn(); outstanding -= 1; settle() })
            }
            tx.objectStore = () => ({
              get: key => {
                const handle = { result: undefined, onsuccess: null, onerror: null }
                tx.done(() => { handle.result = data.get(key); handle.onsuccess?.() })
                return handle
              },
              put: (value, key) => {
                const handle = { result: key, onsuccess: null, onerror: null }
                tx.done(() => { data.set(key, value); handle.onsuccess?.() })
                return handle
              },
              delete: key => {
                const handle = { result: undefined, onsuccess: null, onerror: null }
                tx.done(() => { data.delete(key); handle.onsuccess?.() })
                return handle
              },
            })
            return tx
          },
        }
        opening.onsuccess?.()
      })
      return opening
    },
  }
}

// A directory handle as the browser hands one back in a NEW document: real enough for the
// executor (the plugin only calls the methods it uses), with the permission answer this test
// wants to exercise.
function fakeHandle({ permission = 'granted', onRead } = {}) {
  return {
    name: 'local', kind: 'directory',
    queryPermission: async () => permission,
    requestPermission: async () => permission,
    async getDirectoryHandle() { throw Object.assign(new Error('missing'), { name: 'NotFoundError' }) },
    async getFileHandle() { throw Object.assign(new Error('missing'), { name: 'NotFoundError' }) },
    async entries() { onRead?.() },
  }
}

function setup({
  record = null,
  permission = 'granted',
  resumeRefused = false,
  pollFails = 0,
  pollRequests = [{ id: 'request-1', op: 'stat', path: '' }],
} = {}) {
  const events = [], requests = [], warnings = [], effects = [], registered = new Map(), pendingEffects = []
  let client, generation = 1, resumeCalls = 0, pollCount = 0
  const indexedDB = fakeIndexedDB()
  let pollFailureBudget = pollFails, countedPolls = 0
  const pollArmed = pollFails > 0
  const handle = fakeHandle({ permission })
  // A real record carries the real handle: that is the whole reason a reload can reattach
  // without asking for the directory again.
  const stored = record ? Object.assign({}, record, { handle: record.handle || handle }) : null
  if (stored) indexedDB.data.set('mount:http://127.0.0.1:13861', stored)
  const json = value => ({ ok: true, json: async () => ({ ok: true, value }) })
  const rejection = (code, message) => ({ ok: true, json: async () => ({ ok: false, error: { code, message } }) })

  const window = {
    isSecureContext: true,
    location: { origin: 'http://127.0.0.1:13861' },
    indexedDB,
    async showDirectoryPicker(options) {
      events.push('picker')
      assert.equal(options.mode, 'readwrite')
      return handle
    },
    async fetch(url, options) {
      const endpoint = url.split('/').at(-1), payload = JSON.parse(options.body)
      events.push(endpoint)
      requests.push({ endpoint, payload })
      if (options.signal.aborted) throw Object.assign(new Error('abort'), { name: 'AbortError' })
      if (endpoint === 'open') {
        return json({ token: 'private-token', mountpoint: '/home/account/browser/id', id: 'id' })
      }
      if (endpoint === 'activate') {
        return json({ mountpoint: '/home/account/browser/id', id: 'id' })
      }
      if (endpoint === 'resume') {
        resumeCalls++
        if (resumeRefused) return rejection('browser/failed', 'unknown directory capability')
        return json({ id: stored?.id || 'id', mountpoint: stored?.mountpoint || '/home/account/browser/id', resumed: resumeCalls })
      }
      if (endpoint === 'poll') {
        pollCount++
        const armed = pollArmed && countedPolls++ > 1
        if (armed && pollFailureBudget-- > 0) throw new Error('gateway HTTP 502')
        for (const request of pollRequests) events.push('served:' + request.op)
        // Nothing to serve: the real gateway answers an empty poll (it holds the request for
        // a few seconds first, which the plugin paces with its own 100ms delay). Answering
        // immediately — and parking only on the signal — is what keeps the loop able to reach
        // its next call, which is where a test that wants a transport failure makes it fail.
        return new Promise((resolve, reject) => {
          const timer = setTimeout(() => resolve({ requests: [] }), 20)
          options.signal.addEventListener('abort', () => {
            clearTimeout(timer)
            reject(Object.assign(new Error('abort'), { name: 'AbortError' }))
          }, { once: true })
        })
      }
      if (endpoint === 'respond') { events.push('responded'); return json({ accepted: true }) }
      if (endpoint === 'close') return json({ closed: true })
      throw new Error('unexpected endpoint ' + endpoint)
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
    head: { appendChild: () => {} },
  }
  vm.runInNewContext(source, {
    window, document, TextEncoder, Uint8Array, atob, btoa, AbortController, console,
    setTimeout, clearTimeout,
  })
  const ctx = {
    logger: { warn: message => warnings.push(message) },
    connection: { state: { getSnapshot: () => 'connected' }, generation: { getSnapshot: () => ({ id: generation }) }, reconnect() { generation++ } },
    slots: { inject: (_n, fn) => fn(), register(entry, component) { registered.set(entry.name, { options: entry, component }); return () => {} } },
    // ctx.effect REGISTERS an effect; it does not run cleanup now. A harness that calls the
    // callback immediately would run the plugin's cleanup effect during apply(), which sets
    // `disposed` — and then the boot lookup's own result is thrown away as if the page had
    // been left. The effects are flushed after apply() instead, in registration order.
    effect(fn, label) { pendingEffects.push({ fn, label }) },
    remote: { workspace: {
      async create({ path }) { events.push('create'); return { ok: true, value: { workspace: { workspaceId: 'workspace', title: 'id' } } } },
      async rename() { events.push('rename'); return { ok: true } },
      async delete() { events.push('delete') },
      async list() { events.push('list'); return { value: { workspaces: stored?.workspaceId ? [{ workspaceId: stored.workspaceId, path: stored.mountpoint }] : [] } } },
    } },
    uiWorkspace: { async connectWorkspace(id) { events.push('connect:' + id) } },
  }
  client.apply(ctx)
  for (const { fn, label } of pendingEffects) {
    const dispose = fn()
    if (typeof dispose === 'function') effects.push(dispose)
    if (label?.endsWith('cleanup')) client.__cleanup = dispose
  }
  const row = () => registered.get('sidebar.footer.action').component
  return {
    events, requests, warnings, indexedDB,
    inject: () => client.inject,
    click: () => row()().props.onClick(),
    text: () => textOf(row()()),
    state: () => row()().props['data-dshgw-state'],
    dialogText: () => textOf(registered.get('shell.overlay').component()),
    resumeCalls: () => resumeCalls,
    // The stored record, read the way the plugin reads it (a Map is not key-enumerable).
    record: () => indexedDB.data.get('mount:http://127.0.0.1:13861') || null,
    pollCount: () => pollCount,
    dispose: async () => { client.__cleanup?.(); await tick() },
  }
}

// Wait for the page to reach a state instead of counting microtasks: the boot path is a
// chain of IndexedDB and permission promises whose length is not the test's business.
async function until(predicate, what = 'condition', timeout = 2000) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    if (predicate()) return
    await new Promise(resolve => setTimeout(resolve, 5))
  }
  throw new Error('timed out waiting for ' + what)
}

const storedRecord = (overrides = {}) => Object.assign({
  version: 1, token: 'private-token', id: 'id', name: 'local',
  workspaceId: 'workspace', handle: null, at: Date.now(), mountpoint: '/home/account/browser/id',
}, overrides)

test('a page that finds a stored mount offers to restore it, without opening the picker', async () => {
  const ui = setup({ record: storedRecord() })
  await until(() => ui.state() === 'resumable')
  // Boot only reads the record: it must not resume anything by itself, because the mount
  // belongs to a click.
  assert.equal(ui.resumeCalls(), 0, 'the page resumed on its own')
  assert.match(ui.text(), /点击恢复/)

  await ui.click()
  await until(() => ui.state() === 'mounted')
  assert.equal(ui.resumeCalls(), 1)
  assert.ok(!ui.events.includes('picker'), 'restoring must not ask for a directory again')
  assert.ok(!ui.events.includes('create'), 'restoring must not register a second workspace')
  assert.ok(!ui.events.includes('activate'), 'restoring must not restart the worker')
  assert.match(ui.text(), /已恢复/)
  // The workspace that the path already had is reopened, not duplicated.
  assert.ok(ui.events.includes('connect:workspace'), 'the restored workspace was not reopened')
  await ui.dispose()
})

test('a stored record whose permission the browser wants re-confirmed falls back to a fresh mount', async () => {
  const ui = setup({ record: storedRecord(), permission: 'prompt' })
  await until(() => ui.indexedDB.data.size === 0, 'the unusable record to be dropped')
  // Nothing to offer: a page cannot reattach on its own without the grant, and the record is
  // dropped so the row never advertises a resume that cannot work.
  assert.equal(ui.state(), 'idle')

  await ui.click()
  await until(() => ui.events.includes('picker'), 'the picker fallback')
  assert.equal(ui.resumeCalls(), 0)
  await ui.dispose()
})

test('a refused resume clears the record and still mounts on the same click', async () => {
  const ui = setup({ record: storedRecord(), resumeRefused: true })
  await until(() => ui.state() === 'resumable')
  await ui.click()
  await until(() => ui.events.includes('picker'), 'the fresh-mount fallback')
  assert.equal(ui.resumeCalls(), 1)
  // "unknown directory capability" is final: the dead record must not survive to be offered
  // again, and the operator's click must still produce a working mount.
  assert.equal(ui.record()?.token, 'private-token', 'the fresh mount did not replace the dead record')
  assert.ok(ui.events.includes('picker'), 'the click did not recover into a fresh mount')
  assert.equal(ui.state(), 'mounted')
  await ui.dispose()
})

test('an expired record is not offered at all', async () => {
  const ui = setup({ record: storedRecord({ at: Date.now() - 10 * 60 * 1000 }) })
  await until(() => ui.indexedDB.data.size === 0, 'the expired record to be dropped')
  assert.equal(ui.state(), 'idle')
  await ui.dispose()
})

test('a transport failure inside one page reconnects instead of tearing the mount down', async () => {
  const ui = setup({ record: storedRecord(), pollFails: 1 })
  await until(() => ui.state() === 'resumable')
  await ui.click()
  await until(() => ui.resumeCalls() === 2, 'the in-page reconnect')
  // The poll failed once, the page asked to resume, and it kept serving: no close, no
  // "请重新选择目录", and the mount is still owned by this page.
  assert.ok(!ui.events.includes('close'), 'a recoverable failure closed the mount')
  await until(() => ui.state() === 'mounted', 'the row to report a live mount')
  assert.match(ui.text(), /已重连/)
  await ui.dispose()
})

test('the first successful mount writes the record the next document needs', async () => {
  const ui = setup()
  await tick()
  assert.equal(ui.state(), 'idle', 'a page with no record must not claim anything to restore')
  await ui.click()

  await until(() => ui.indexedDB.data.size > 0, 'the record of the new mount')
  const saved = ui.record()
  assert.ok(saved, 'the mount did not leave a record')
  assert.equal(saved.token, 'private-token')
  assert.equal(saved.name, 'local')
  assert.ok(saved.handle, 'the record has no directory handle to resume with')
  await ui.dispose()
})

test('an explicit disconnect deletes the record, so no later page offers a dead mount', async () => {
  const ui = setup({ record: storedRecord() })
  await until(() => ui.state() === 'resumable')
  await ui.click()
  await until(() => ui.state() === 'mounted')
  assert.ok(ui.indexedDB.data.size > 0)
  await ui.click()
  await until(() => ui.state() === 'idle', 'the explicit unmount')
  assert.ok(ui.events.includes('close'))
  assert.equal(ui.indexedDB.data.size, 0, 'the record survived an explicit unmount')
  await ui.dispose()
})
