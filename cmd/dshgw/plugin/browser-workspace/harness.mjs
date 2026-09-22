// The fake browser one browser-workspace test runs against.
//
// It is shared by ui.test.mjs (the row and the mount contract), manage.test.mjs (the folder
// list: add / connect / disconnect / delete, several at once) and reconnect.test.mjs (what a
// page does when its transport dies, and what the NEXT document does with the mounts the
// previous one left behind). All three need the same four browser services:
//
//   · IndexedDB, where the saved folder list lives between documents (and whose request
//     callbacks must run BEFORE the transaction completes — the order real IndexedDB
//     guarantees and the order the client resolves on);
//   · showDirectoryPicker, replaced by ONE handler that answers with whatever directory the
//     test wants next (a real directory dialog cannot be driven from a test);
//   · fetch against the plugin's own endpoints, answered by a stateful fake gateway that
//     hands out one capability per mounted key and remembers which key is mounted;
//   · the DSH services the plugin injects (slots, connection, remote.workspace, uiWorkspace).
//
// Nothing here asserts: it reports what happened (events, requests, warnings) and the test
// files decide what that means.
import { readFile } from 'node:fs/promises'
import vm from 'node:vm'

export const source = await readFile(new URL('./client.js', import.meta.url), 'utf8')
export const ORIGIN = 'http://127.0.0.1:13861'
export const tick = () => new Promise(resolve => setImmediate(resolve))

/** Wait for the page to reach a state instead of counting microtasks. */
export async function until (predicate, what = 'condition', timeout = 2000) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    if (predicate()) return
    await new Promise(resolve => setTimeout(resolve, 5))
  }
  throw new Error('timed out waiting for ' + what)
}

const abortError = () => Object.assign(new Error('abort'), { name: 'AbortError' })
export const rejection = (code, message) => ({ ok: true, json: async () => ({ ok: false, error: { code, message } }) })
export const ok = value => ({ ok: true, json: async () => ({ ok: true, value }) })

/** A small IndexedDB stand-in keyed by hand, exactly like the real one's two properties. */
export function fakeIndexedDB (initial = {}) {
  const data = new Map(Object.entries(initial))
  return {
    data,
    open () {
      const opening = { result: null, onsuccess: null, onerror: null, onupgradeneeded: null, onblocked: null }
      setImmediate(() => {
        opening.result = {
          objectStoreNames: { contains: () => true },
          createObjectStore: () => ({}),
          close () {},
          transaction () {
            const tx = { oncomplete: null, onerror: null, onabort: null, error: null }
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

/**
 * A directory handle as the browser hands one back: real enough for the executor and for the
 * permission questions the plugin asks, with the answers this test wants.
 */
export function fakeHandle ({ name = 'local', permission = 'granted', onRead } = {}) {
  return {
    name, kind: 'directory',
    queryPermission: async () => permission,
    requestPermission: async () => permission,
    async getDirectoryHandle () { throw Object.assign(new Error('missing'), { name: 'NotFoundError' }) },
    async getFileHandle () { throw Object.assign(new Error('missing'), { name: 'NotFoundError' }) },
    async entries () { onRead?.(); },
    // Distinct handles are different directories unless the test says otherwise.
    async isSameEntry (other) { return other === this },
  }
}

/** The saved folder list as a v2 record, the shape the client writes under 'folders:<origin>'. */
export function storedFolders (folders) {
  return { version: 2, folders }
}
export function storedFolder (overrides = {}) {
  return Object.assign({
    version: 2, key: 'a'.repeat(32), name: 'local', workspaceId: 'workspace',
    token: 'private-token', mountpoint: `/home/account/browser/${'a'.repeat(32)}`,
    at: Date.now(), handle: null,
  }, overrides)
}

/**
 * One page under test.
 *
 * @param options.stored       record written into IndexedDB before boot (v2 list or v1 record)
 * @param options.legacy       write the record under the v1 key ('mount:<origin>')
 * @param options.legacyGateway a gateway that predates stable directory keys: it refuses any
 *                             request carrying a field it does not know (the real one decodes
 *                             with DisallowUnknownFields), so the client's degradation is
 *                             exercised instead of assumed
 * @param options.mountAlive   whether the fake gateway still holds the mount a FRESH stored
 *                             token names (a page reload, the case resume exists for)
 * @param options.pickerDirs   directory handles showDirectoryPicker answers with, in order
 * @param options.permission   permission answer of handles this harness creates
 * @param options.noPicker     a browser without the File System Access API
 * @param options.mountLimit   the fake gateway refuses `open` beyond this many live mounts
 * @param options.openRefusals gateway messages `open` answers with, in order
 * @param options.activateFails `activate` answers with a business failure
 * @param options.closeFailures the first N closes fail (an unconfirmed cleanup)
 * @param options.resumeRefusals gateway messages `resume` answers with, in order
 * @param options.createFailures the first N workspace registrations fail
 * @param options.created       whether the workspace registration was newly created
 * @param options.deleteFailures the first N workspace deletions fail (a call that raced a
 *                              worker restart)
 * @param options.pollRequests  the request batch the first poll of each mount carries
 * @param options.pollIdleMs    how long an empty poll parks before answering
 * @param options.pollFailures  the first N polls of an ALREADY established mount fail (the
 *                              transport breaking under a live mount, not during the mount)
 * @param options.takenNames    mount directory names another local directory of this account
 *                              already owns (the fake gateway suffixes a proposal that hits one)
 * @param options.allocated     the exact key `allocate` answers with, instead of arbitrating
 * @param options.unnamedGateway a gateway that predates mount directory names: it has no
 *                              `allocate` endpoint at all, so the client must fall back to a
 *                              generated key and still mount
 * @param options.purgeRefusals gateway messages `close {key,purge}` answers with, in order (a
 *                              live mount, a directory this service did not create)
 */
export function setup (options = {}) {
  const {
    stored = null, legacy = false, legacyGateway = false, mountAlive = true, pickerDirs = [], permission = 'granted', noPicker = false,
    mountLimit = 8, openRefusals = [], activateFails = false, closeFailures = 0, resumeRefusals = [],
    createFailures = 0, deleteFailures = 0, created = true, pollRequests = [{ id: 'request-1', op: 'stat', path: '' }],
    pollIdleMs = 20, pollFailures = 0, takenNames = [], allocated = null, unnamedGateway = false, purgeRefusals = [],
  } = options
  const events = [], requests = [], warnings = [], registered = new Map(), pendingEffects = []
  const state = {
    mounts: new Map(),      // token -> { key, mountpoint }
    keys: new Map(),        // key -> token (one live mount per key)
    polled: new Set(),
    openCalls: 0, resumeCalls: 0, closeCalls: 0, closed: 0, pollFailures,
    allocateCalls: 0, purgeCalls: 0, purged: [], takenNames: [...takenNames],
    openRefusals: [...openRefusals], resumeRefusals: [...resumeRefusals], purgeRefusals: [...purgeRefusals],
    createFailures, deleteFailures, closeFailures, generation: 1,
  }
  const indexedDB = fakeIndexedDB()
  if (stored) indexedDB.data.set(legacy ? `mount:${ORIGIN}` : `folders:${ORIGIN}`, stored)
  // A page reload leaves a gateway that still holds the mount; the stored token is what names
  // it. Tests that want a forgotten mount either say so here or refuse the resume.
  if (stored && mountAlive) {
    const rows = Array.isArray(stored.folders) ? stored.folders : [stored]
    for (const row of rows) {
      if (!row?.token || typeof row.at !== 'number' || Date.now() - row.at > 45000) continue
      const key = row.key || row.id
      state.mounts.set(row.token, { key, mountpoint: row.mountpoint || `/home/account/browser/${key}` })
      state.keys.set(key, row.token)
    }
  }
  let client, picked = 0, confirmText, pickerActive = null
  const makeHandle = (name = 'local') => {
    const handle = fakeHandle({ name, permission })
    handle.name = name
    return handle
  }
  const keyListeners = {}
  const window = {
    isSecureContext: !noPicker,
    location: { origin: ORIGIN },
    indexedDB,
    // A minimal keydown bus: the plugin attaches its Escape listener here while the window
    // is open, so a test can drive that path exactly as a browser would.
    addEventListener (type, handler) { (keyListeners[type] ||= new Set()).add(handler) },
    removeEventListener (type, handler) { keyListeners[type]?.delete(handler) },
    confirm (text) { confirmText = text; events.push('confirm'); return true },
    showDirectoryPicker (pickerOptions) {
      events.push('picker')
      pickerActive = globalThis.navigator?.userActivation?.isActive ?? null
      if (noPicker) throw new Error('no picker')
      if (pickerOptions?.mode !== 'readwrite') throw new Error('picker must be readwrite')
      const asked = pickerDirs[picked]
      picked += 1
      if (asked === undefined) return Promise.reject(abortError())
      return Promise.resolve(typeof asked === 'function' ? asked(makeHandle) : asked)
    },
    async fetch (url, fetchOptions) {
      const endpoint = url.split('/').at(-1)
      const payload = JSON.parse(fetchOptions.body)
      events.push(endpoint)
      requests.push({ endpoint, payload })
      if (fetchOptions.credentials !== 'same-origin') throw new Error('cross-origin credentials')
      if (fetchOptions.signal.aborted) throw abortError()
      if (legacyGateway) {
        const known = ['token', 'name', 'writable', 'id', 'result']
        const unknown = Object.keys(payload).find(key => !known.includes(key))
        if (unknown !== undefined) return rejection('browser/failed', `json: unknown field ${JSON.stringify(unknown)}`)
      }
      if (endpoint === 'allocate') {
        state.allocateCalls += 1
        // A gateway that predates mount directory names has no such endpoint, and one that
        // predates the client's own fields would refuse the request outright.
        if (unnamedGateway || legacyGateway) return rejection('browser/failed', 'unknown endpoint')
        if (allocated !== null) return ok({ key: allocated })
        const desired = typeof payload.name === 'string' ? payload.name : ''
        // The real gateway's arbitration, in miniature: the first free "<name>"/"<name>-N" for
        // this account, where taken means a name the test declared or a live mount holds; an
        // unusable proposal is answered with a generated id rather than a refusal.
        if (desired === '') return ok({ key: 'f'.repeat(48) })
        const taken = name => state.takenNames.includes(name) || [...state.mounts.values()].some(mount => mount.key === name)
        let answer = desired
        for (let n = 2; taken(answer) && n <= 9; n++) answer = `${desired}-${n}`
        if (taken(answer)) answer = desired + '-0123abcd'
        return ok({ key: answer })
      }
      if (endpoint === 'open') {
        state.openCalls += 1
        if (state.openRefusals.length) return rejection('browser/failed', state.openRefusals.shift())
        if (state.keys.has(payload.key)) return rejection('browser/failed', 'directory key already mounted for this account')
        if (state.mounts.size >= mountLimit) return rejection('browser/failed', `per-account browser directory limit reached: ${mountLimit}`)
        // Without a key this is the older gateway's behaviour: the mount point is named by a
        // fresh id the client never chose (and never learns to reuse).
        const key = typeof payload.key === 'string' ? payload.key : `legacy${state.openCalls}${'0'.repeat(26)}`
        const token = `token-${key.slice(0, 4)}-${state.openCalls}`
        const mountpoint = `/home/account/browser/${key}`
        state.mounts.set(token, { key, mountpoint })
        if (typeof payload.key === 'string') state.keys.set(payload.key, token)
        return ok({ token, mountpoint, id: key })
      }
      if (endpoint === 'resume') {
        state.resumeCalls += 1
        if (state.resumeRefusals.length) return rejection('browser/failed', state.resumeRefusals.shift())
        const known = state.mounts.get(payload.token)
        if (!known) return rejection('browser/failed', 'unknown directory capability')
        return ok({ id: known.key, mountpoint: known.mountpoint, resumed: state.resumeCalls })
      }
      if (endpoint === 'poll') {
        if (!state.mounts.has(payload.token)) return rejection('browser/failed', 'unknown directory capability')
        // A broken transport under a LIVE mount: the first poll of a mount must succeed, or
        // the failure would be part of mounting rather than of staying mounted.
        if (state.polled.has(payload.token) && state.pollFailures-- > 0) throw new Error('gateway HTTP 502')
        if (!state.polled.has(payload.token) && pollRequests.length) {
          state.polled.add(payload.token)
          for (const request of pollRequests) events.push('served:' + request.op)
          return ok({ requests: pollRequests })
        }
        return new Promise((resolve, reject) => {
          const timer = setTimeout(() => resolve(ok({ requests: [] })), pollIdleMs)
          fetchOptions.signal.addEventListener('abort', () => {
            clearTimeout(timer)
            reject(abortError())
          }, { once: true })
        })
      }
      if (endpoint === 'respond') return ok({ accepted: true })
      if (endpoint === 'activate') {
        if (activateFails) return rejection('EIO', 'restart failed')
        const known = state.mounts.get(payload.token)
        return ok({ mountpoint: known?.mountpoint, id: known?.key })
      }
      if (endpoint === 'close') {
        if (!payload.token) {
          // Releasing a mount point by KEY: the delete path of a folder whose capability died
          // with its mount, so there is no token left to close through.
          state.purgeCalls += 1
          if (state.purgeRefusals.length) return rejection('browser/failed', state.purgeRefusals.shift())
          if (payload.purge && typeof payload.key === 'string') state.purged.push(payload.key)
          return ok({ closed: true })
        }
        state.closeCalls += 1
        if (state.closed++ < state.closeFailures) throw new Error('temporary close failure')
        const known = state.mounts.get(payload.token)
        if (known) { state.mounts.delete(payload.token); state.keys.delete(known.key) }
        return ok({ closed: true })
      }
      throw new Error('unexpected endpoint ' + endpoint)
    },
    __ModuleLoader__: { load ({ factory }) {
      client = factory(() => ({
        useState: () => [0, () => {}],
        useEffect: fn => { pendingEffects.push(fn()) },
        // React flattens an array child, and the plugin passes one: flattening here keeps the
        // fake tree shaped like the one the browser renders.
        createElement: (type, props, ...children) => ({ type, props, children: children.flat(Infinity) }),
      }))
    } },
  }
  const styles = []
  const document = {
    querySelector: () => null,
    createElement: () => ({ dataset: {}, textContent: '' }),
    head: { appendChild: tag => styles.push(tag) },
  }
  const autoTimers = new Map()
  let autoSeq = 0
  const sandboxSetTimeout = (fn, ms) => {
    if (client !== undefined && ms === client.AUTO_CLOSE_MS) { const id = `auto-${++autoSeq}`; autoTimers.set(id, fn); return id }
    return setTimeout(fn, ms)
  }
  const sandboxClearTimeout = handle => {
    if (typeof handle === 'string') { autoTimers.delete(handle); return }
    clearTimeout(handle)
  }
  vm.runInNewContext(source, {
    window, document, TextEncoder, Uint8Array, atob, btoa, AbortController, console,
    setTimeout: sandboxSetTimeout, clearTimeout: sandboxClearTimeout,
  })
  const ctx = {
    logger: { warn: message => warnings.push(message), info: () => {} },
    connection: {
      state: { getSnapshot: () => 'connected' },
      generation: { getSnapshot: () => ({ id: state.generation }) },
      reconnect () { events.push('reconnect'); state.generation += 1 },
    },
    slots: {
      inject: (_name, fn) => fn(),
      register (entry, component) { registered.set(entry.name, { options: entry, component }); return () => events.push('unregister') },
    },
    // ctx.effect REGISTERS an effect; it does not run it now. A harness that ran them
    // immediately would execute the plugin's cleanup effect during apply(), which sets
    // `disposed` — and then the boot lookup's own result would be thrown away as if the page
    // had been left. They are flushed after apply() instead, in registration order.
    effect (fn, label) { pendingEffects.push({ fn, label }) },
    remote: { workspace: {
      async create ({ path }) {
        events.push('create')
        if (state.createFailures > 0) { state.createFailures -= 1; throw new Error('workspace registry busy') }
        return { ok: true, value: { workspace: { workspaceId: 'workspace-' + path.split('/').at(-1).slice(0, 4), title: path.split('/').at(-1) }, created } }
      },
      async rename ({ workspaceId, title }) { events.push('rename:' + title); return { ok: true, value: { workspace: { workspaceId, title } } } },
      async delete (request) {
        // The generated remote is a REQUEST-OBJECT api: a bare id violates its schema, and the
        // host then refuses (or ignores) the call. This mock refuses the same way, so a wrong
        // call shape cannot pass a test and then fail in a real browser.
        if (request === null || typeof request !== 'object' || typeof request.workspaceId !== 'string') {
          throw new Error('workspace.delete expects { workspaceId }')
        }
        events.push('delete:' + request.workspaceId)
        if (state.deleteFailures > 0) { state.deleteFailures -= 1; throw new Error('the worker was restarting') }
        return { ok: true, value: { deleted: true } }
      },
      async list () { events.push('list'); return { ok: true, value: { workspaces: [] } } },
    } },
    uiWorkspace: { async connectWorkspace (id) { events.push('connect:' + id); return 'session' } },
  }
  client.apply(ctx)
  let cleanup
  for (const { fn, label } of pendingEffects) {
    const disposer = fn()
    if (typeof disposer === 'function' && label === 'browser-workspace: cleanup') cleanup = disposer
  }
  pendingEffects.length = 0

  // ── reading the surfaces ────────────────────────────────────────────────────────
  const findByProp = (node, key, value) => {
    let found = null
    walk(node, child => { if (found === null && child?.props?.[key] !== undefined && (value === undefined || child.props[key] === value)) found = child })
    return found
  }
  const findButton = (node, className) => {
    let found = null
    walk(node, child => {
      if (found === null && child?.type === 'button' && String(child.props?.className || '').includes(className)) found = child
    })
    return found
  }
  const row = () => registered.get('sidebar.footer.action').component
  const dialog = () => registered.get('shell.overlay').component
  const rowElement = (props = {}) => row()(props)
  // The row CONTAINER also carries data-dshgw-state (a real browser run reads it from either),
  // so the body is identified by being the row's own action button.
  const body = (props = {}) => findButton(rowElement(props), 'dshgw-bw-action')
  const manage = () => findByProp(rowElement(), 'data-dshgw-manage')
  const dialogElement = () => dialog()()
  const folderRows = () => {
    const rows = []
    walk(dialogElement(), node => { if (node?.props?.['data-dshgw-folder'] !== undefined) rows.push(node) })
    return rows
  }
  const folderRow = name => folderRows().find(node => textOf(node).includes(name)) || null
  const folderAction = (name, action) => {
    const container = folderRow(name)
    if (container === null) return null
    let button = null
    walk(container, node => { if (node?.props?.['data-dshgw-folder-action'] === action) button = node })
    return button
  }
  const dialogButton = label => {
    let button = null
    walk(dialogElement(), node => { if (button === null && node?.type === 'button' && textOf(node).trim() === label) button = node })
    return button
  }
  return {
    events, requests, warnings, registered, indexedDB, state, styles,
    knownWarnings: warnings,
    inject: () => client.inject,
    client: () => client,
    // row
    row: (props) => rowElement(props),
    body: (props) => body(props),
    manage,
    rowText: () => textOf(rowElement()),
    rowState: () => body()?.props?.['data-dshgw-state'] ?? null,
    rowTitle: () => body()?.props?.title ?? null,
    rowPressed: () => body()?.props?.['aria-pressed'] ?? null,
    rowChildren: () => body()?.children ?? [],
    styles: () => styles,
    keyListenerCount: () => Object.values(keyListeners).reduce((n, set) => n + set.size, 0),
    pressKey: (key) => {
      for (const handler of [...(keyListeners.keydown ?? [])]) handler({ key })
    },
    clickRow: () => body().props.onClick(),
    clickManage: () => manage().props.onClick(),
    dialogClose: () => {
      let button = null
      walk(dialogElement(), node => { if (button === null && node?.props?.['data-dshgw-close'] !== undefined) button = node })
      return button
    },
    clickDialogClose: () => {
      const button = dialogClose()
      if (button === null) throw new Error('no corner close button')
      return button.props.onClick()
    },
    // window
    dialogElement,
    dialogOpen: () => dialogElement() !== null,
    dialogText: () => textOf(dialogElement()),
    dialogButtons: () => {
      const labels = []
      walk(dialogElement(), node => { if (node?.type === 'button') labels.push(textOf(node).trim()) })
      return labels
    },
    clickDialog: label => { const button = dialogButton(label); if (button === null) throw new Error('no dialog button ' + label); return button.props.onClick() },
    folderNames: () => folderRows().map(node => node.props['data-dshgw-folder']),
    folderStates: () => folderRows().map(node => node.props['data-dshgw-folder-state']),
    folderText: name => { const row = folderRow(name); return row === null ? '' : textOf(row) },
    folderActions: name => {
      const labels = []
      walk(folderRow(name), node => { if (node?.type === 'button') labels.push(textOf(node).trim()) })
      return labels
    },
    clickFolder: (name, action) => {
      const button = folderAction(name, action)
      if (button === null) throw new Error(`no ${action} action on ${name}`)
      return button.props.onClick()
    },
    // records
    record: () => indexedDB.data.get(`folders:${ORIGIN}`) || null,
    // The stored list was built inside the vm the plugin runs in, so it is copied into this
    // realm: a test comparing it with a host array must compare VALUES, not prototypes.
    savedFolders: () => Array.from((indexedDB.data.get(`folders:${ORIGIN}`) || { folders: [] }).folders || []),
    legacyRecord: () => indexedDB.data.get(`mount:${ORIGIN}`) || null,
    confirm: () => confirmText,
    pickerActive: () => pickerActive,
    pendingAutoClose: () => autoTimers.size,
    fireAutoClose: () => { const pending = [...autoTimers.values()]; autoTimers.clear(); for (const fn of pending) fn() },
    options: () => registered.get('sidebar.footer.action').options,
    dialogOptions: () => registered.get('shell.overlay').options,
    dispose: async () => { cleanup?.(); await tick() },
  }
}

/** Depth-first walk over the element tree the fake React builds. */
export function walk (node, visit) {
  if (node === null || node === undefined || typeof node !== 'object') return
  if (Array.isArray(node)) { for (const child of node) walk(child, visit); return }
  visit(node)
  walk(node.children, visit)
}

export function textOf (node) {
  if (node === null || node === undefined || node === false) return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textOf).join('')
  return textOf(node.children)
}

export function findNode (node, predicate) {
  let found = null
  walk(node, child => { if (found === null && predicate(child)) found = child })
  return found
}

export const classOf = node => (typeof node?.props?.className === 'string' ? node.props.className : '')
