// Integration test for the browser half (client.js) against the real host half.
//
// The renderer and xterm are stubbed — this installation has no browser and no canvas backend —
// but everything else is production code: the built bundle, the RPC endpoint table, the session
// registry, and a real node-pty behind it. A pass means "open → read → keystroke → read → resize
// → close" works end to end through the same code paths the GUI uses.
//
// Run: npm test   (node --test test/)

import { createRequire } from 'node:module'
import { strict as assert } from 'node:assert'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import test from 'node:test'

import { createHandlers } from '../rpc-handlers.js'
import { TtyRegistry } from '../tty-session.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const PLUGIN_DIR = join(HERE, '..')
const ANCHOR = process.env.DSHGW_DSH_ANCHOR ?? process.argv[1]
const pty = createRequire(ANCHOR)('node-pty')

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/** Let queued promises, timers and re-renders settle. */
async function flush(renderer, rounds = 12) {
  for (let index = 0; index < rounds; index += 1) {
    renderer?.settle()
    await sleep(15)
  }
  renderer?.settle()
}

/**
 * A very small React stand-in: function components, hook slots kept per instance, effect deps
 * compared by identity. Enough to mount the panel, walk its tree, and click its buttons.
 */
function createRenderer() {
  const instances = new Map()
  let current = null
  let hookIndex = 0
  let scheduled = false
  let rootRender = null

  const schedule = () => {
    if (scheduled) return
    scheduled = true
    setTimeout(() => { scheduled = false; rootRender?.() }, 0)
  }

  const useSlot = (kind, initialize) => {
    const slots = current.hooks
    const index = hookIndex++
    if (slots[index] === undefined) slots[index] = { kind, value: initialize(), deps: undefined, cleanup: null, fn: undefined }
    return slots[index]
  }

  const React = {
    createElement(type, props, ...children) {
      return { type, props: props ?? {}, children: children.flat(Infinity).filter((child) => child !== null && child !== undefined && child !== false && child !== true) }
    },
    useState(initial) {
      const slot = useSlot('state', () => (typeof initial === 'function' ? initial() : initial))
      return [slot.value, (next) => { slot.value = typeof next === 'function' ? next(slot.value) : next; schedule() }]
    },
    useRef(initial) { return useSlot('ref', () => ({ current: initial })).value },
    useEffect(fn, deps) {
      const slot = useSlot('effect', () => ({}))
      slot.fn = fn
      slot.deps = deps
    },
    useCallback(fn) { return fn },
  }

  const walk = (node, path) => {
    if (node === null || typeof node !== 'object') return node
    if (Array.isArray(node)) return node.map((child, index) => walk(child, `${path}[${index}]`))
    if (typeof node.type === 'function') {
      const key = `${path}|${node.props.key ?? ''}`
      let instance = instances.get(key)
      if (instance === undefined) {
        instance = { hooks: [], rendered: false }
        instances.set(key, instance)
      }
      instance.rendered = true
      const previous = current
      current = instance
      hookIndex = 0
      let output
      try {
        output = node.type(node.props)
      } finally {
        current = previous
      }
      instance.tree = walk(output, key)
      return instance.tree
    }
    const element = { type: node.type, props: node.props, children: [] }
    if (node.props.ref !== undefined && node.props.ref !== null && typeof node.props.ref === 'object') {
      const host = { clientWidth: 900, clientHeight: 300, classList: { add() {}, remove() {} } }
      node.props.ref.current = Object.assign(host, { __host: true })
      element.host = host
    }
    element.children = (node.children ?? []).map((child, index) => walk(child, `${path}/${String(node.type)}[${index}]`))
    return element
  }

  const runEffects = () => {
    for (const [key, instance] of instances) {
      if (instance.rendered !== true) {
        for (const slot of instance.hooks) {
          if (slot.kind === 'effect' && typeof slot.cleanup === 'function') slot.cleanup()
        }
        instances.delete(key)
        continue
      }
      for (const slot of instance.hooks) {
        if (slot.kind !== 'effect' || slot.fn === undefined) continue
        const deps = slot.deps
        const same = deps !== undefined && slot.previousDeps !== undefined && deps.length === slot.previousDeps.length
          && deps.every((dep, index) => dep === slot.previousDeps[index])
        if (same) { slot.fn = undefined; continue }
        if (typeof slot.cleanup === 'function') slot.cleanup()
        slot.cleanup = slot.fn() ?? null
        slot.previousDeps = deps
        slot.fn = undefined
      }
    }
  }

  const render = (element) => {
    for (const instance of instances.values()) instance.rendered = false
    const tree = walk(element, 'root')
    runEffects()
    return tree
  }

  rootRender = () => render(lastElement)
  let lastElement = null
  const renderTree = (element) => { lastElement = element; return render(element) }
  return {
    React,
    render(element) { return renderTree(element) },
    /** Render one registered slot component the way the shell's outlet would. */
    renderComponent(component, props = {}) { return renderTree({ type: component, props, children: [] }) },
    settle() { if (lastElement !== null) render(lastElement) },
  }
}

/** Depth-first search for the first element whose prop `name` equals `value`. */
function find(tree, predicate) {
  if (tree === null || typeof tree !== 'object') return null
  if (Array.isArray(tree)) {
    for (const child of tree) {
      const hit = find(child, predicate)
      if (hit !== null) return hit
    }
    return null
  }
  if (predicate(tree)) return tree
  for (const child of tree.children ?? []) {
    const hit = find(child, predicate)
    if (hit !== null) return hit
  }
  return null
}

const byClass = (className) => (element) => element.props?.className === className

/** A minimal xterm: records every write and exposes the emitters the client subscribes to. */
class FakeTerminal {
  constructor(options = {}) {
    this.options = options
    this.cols = 80
    this.rows = 24
    this.output = ''
    this.dataHandlers = []
    this.resizeHandlers = []
    this.disposed = false
  }

  loadAddon() {}
  open() {}
  focus() {}
  clear() { this.output = '' }
  reset() { this.output = '' }
  dispose() { this.disposed = true }
  write(data) { this.output += data }
  onData(handler) { this.dataHandlers.push(handler); return { dispose: () => { this.dataHandlers = this.dataHandlers.filter((entry) => entry !== handler) } } }
  onResize(handler) { this.resizeHandlers.push(handler); return { dispose: () => { this.resizeHandlers = this.resizeHandlers.filter((entry) => entry !== handler) } } }
  onExit() { return { dispose() {} } }
  resize(cols, rows) {
    this.cols = cols
    this.rows = rows
    for (const handler of this.resizeHandlers) handler({ cols, rows })
  }

  /** Pretend the user typed. */
  type(text) { for (const handler of this.dataHandlers) handler(text) }
}

/** Build the browser globals the bundle expects, keeping `window` separate from globalThis. */
function installGlobals() {
  const styleTags = []
  const terminals = []
  const windowObject = {
    __ModuleLoader__: { load: (registration) => { windowObject.__registration = registration } },
    __DSHGW_WEB_TTY_XTERM_CSS__: undefined,
    __terminals: terminals,
    addEventListener() {},
    removeEventListener() {},
    requestAnimationFrame: (callback) => setTimeout(callback, 0),
    cancelAnimationFrame: (handle) => clearTimeout(handle),
    localStorage: {
      store: new Map(),
      getItem(key) { return this.store.has(key) ? this.store.get(key) : null },
      setItem(key, value) { this.store.set(key, String(value)) },
      removeItem(key) { this.store.delete(key) },
    },
    // The pane must construct *this* class, not the real xterm the bundle also installs on
    // globalThis: no canvas backend exists here, so the renderer is stubbed and everything else
    // (RPC, sessions, PTY) stays production code.
    Terminal: class extends FakeTerminal {
      constructor(options) {
        super(options)
        terminals.push(this)
      }
    },
    FitAddon: { FitAddon: class { fit() {} } },
  }
  const documentObject = {
    documentElement: { clientWidth: 1280, clientHeight: 800, style: {} },
    head: { appendChild: (tag) => styleTags.push(tag) },
    querySelector: (selector) => (selector.includes('data-plugin-css') ? styleTags.find((tag) => selector.includes(tag.dataset?.pluginCss ?? '\u0000')) ?? null : null),
    createElement: (tag) => ({ tag, dataset: {}, style: {}, textContent: '' }),
  }
  const previous = { window: globalThis.window, document: globalThis.document, getComputedStyle: globalThis.getComputedStyle, localStorage: globalThis.localStorage, ResizeObserver: globalThis.ResizeObserver, requestAnimationFrame: globalThis.requestAnimationFrame, cancelAnimationFrame: globalThis.cancelAnimationFrame }
  globalThis.window = windowObject
  globalThis.document = documentObject
  globalThis.getComputedStyle = () => ({ getPropertyValue: () => '' })
  globalThis.localStorage = windowObject.localStorage
  globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} }
  globalThis.requestAnimationFrame = windowObject.requestAnimationFrame
  globalThis.cancelAnimationFrame = windowObject.cancelAnimationFrame
  return { windowObject, styleTags, restore: () => Object.assign(globalThis, previous) }
}

/** One host half: real registry, real PTY, real endpoint table, RPC-shaped transport. */
function startHost() {
  const sessions = new TtyRegistry({
    pty,
    defaults: {
      shell: '/bin/bash',
      args: ['--noprofile', '--norc', '-i'],
      cwd: PLUGIN_DIR,
      cols: 80,
      rows: 24,
      termName: 'xterm-256color',
      bufferChars: 4096,
      maxSessions: 4,
      env: { ...process.env, TERM: 'xterm-256color', PS1: 'wtt> ' },
    },
  })
  const config = {
    version: '0.1.0-test',
    pluginDir: PLUGIN_DIR,
    traceFile: null,
    trace: false,
    shell: '/bin/bash',
    args: ['--noprofile', '--norc', '-i'],
    cwd: PLUGIN_DIR,
    cwdRoot: '',
    termName: 'xterm-256color',
    maxSessions: 4,
    maxWaitMs: 25_000,
    maxReadChars: 262_144,
  }
  const { dispatch } = createHandlers({ registry: sessions, config, pty, trace: () => {}, log: () => {} })
  return {
    sessions,
    async call(endpoint, payload, signal) {
      try {
        return { ok: true, value: await dispatch(endpoint, payload, signal) }
      } catch (error) {
        return { ok: false, error: { code: error?.code ?? 'web-tty/failed', message: String(error?.message ?? error), details: {} } }
      }
    },
    async dispose() { await sessions.closeAll() },
  }
}

/** Load the built bundle and materialize its factory the way the shell's loader does. */
async function loadClient(renderer, host) {
  const globals = installGlobals()
  await import(`${pathToFileURL(join(PLUGIN_DIR, 'client.js')).href}?test=${Date.now()}`)
  const registration = globals.windowObject.__registration
  assert.ok(registration !== undefined, 'client.js must register itself through window.__ModuleLoader__.load')
  const exportsObject = registration.factory((name) => {
    if (name === 'react') return renderer.React
    throw new Error(`unexpected require(${JSON.stringify(name)}) from the bundle`)
  })
  const registrations = new Map()
  const calls = []
  const ctx = {
    logger: { info: () => {}, warn: () => {}, error: () => {} },
    effect: (callback) => { const dispose = callback(); return typeof dispose === 'function' ? dispose : () => {} },
    slots: {
      inject: (name, callback) => { callback(); return () => {} },
      register: (options, component) => { registrations.set(options.name, component); return () => {} },
    },
    connection: {
      rpc: {
        call: async (channel, endpoint, payload, signal) => {
          assert.equal(channel, '/dshgw-web-tty', 'the plugin must use its own channel')
          calls.push({ endpoint, payload })
          return await host.call(endpoint, payload, signal)
        },
      },
    },
  }
  exportsObject.apply(ctx)
  assert.deepEqual(Array.from(exportsObject.inject), ['slots', 'connection'])
  await flush(renderer, 4)
  return { registration, registrations, calls, ctx, globals, exportsObject }
}

test('client bundle: registers its channel, both slots, and self-identifies against the host', async () => {
  const renderer = createRenderer()
  const host = startHost()
  const client = await loadClient(renderer, host)
  try {
    assert.equal(client.registration.id, 'dshgw-web-tty', 'the bundle id must equal the package name')
    assert.ok(client.registrations.has('sidebar.footer.action'), 'the panel needs a sidebar affordance')
    assert.ok(client.registrations.has('shell.overlay'), 'the panel lives in the frame-wide overlay slot')
    assert.ok(client.globals.styleTags.length > 0, 'the bundle installs its stylesheet')
    assert.ok(client.globals.windowObject.__DSHGW_WEB_TTY_XTERM_CSS__ !== undefined, 'the vendored xterm CSS travels with the bundle')
    const hello = client.calls.find((call) => call.endpoint === 'hello')
    assert.ok(hello !== undefined, 'the panel announces itself with hello()')
  } finally {
    await host.dispose()
    client.globals.restore()
  }
})

test('client: opens a shell, echoes a keystroke, resizes, and closes it', async () => {
  const renderer = createRenderer()
  const host = startHost()
  const client = await loadClient(renderer, host)
  try {
    // The sidebar row is the panel's only affordance; clicking it opens the panel and one tab.
    let tree = renderer.renderComponent(client.registrations.get('sidebar.footer.action'))
    const entry = find(tree, byClass('dshgw-tty-entry'))
    assert.ok(entry !== null, 'the sidebar row renders a button')
    entry.props.onClick()
    await flush(renderer)

    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const panel = find(tree, byClass('dshgw-tty-panel'))
    assert.ok(panel !== null, 'the panel mounts once it is open')

    // The pane opens a PTY on mount; give the shell time to paint its prompt.
    await flush(renderer, 20)
    const tab = find(tree, byClass('dshgw-tty-tab'))
    assert.ok(tab !== null, 'the pane registers its tab once the PTY is up')
    const open = client.calls.find((call) => call.endpoint === 'open')
    assert.ok(open !== undefined, 'the pane asks the host for a session')
    const fake = client.globals.windowObject.__terminals.at(-1)
    assert.ok(fake instanceof FakeTerminal, 'the pane constructed one terminal')
    assert.ok((fake.options.theme?.background ?? '').length > 0, 'the terminal inherits a theme from the shell tokens')

    // A keystroke travels to the PTY and its echo comes back through the long-polling read.
    fake.type('echo INTEGRATION-$((6*7))\n')
    for (let attempt = 0; attempt < 60 && !fake.output.includes('INTEGRATION-42'); attempt += 1) await flush(renderer, 2)
    assert.match(fake.output, /INTEGRATION-42/, `the shell echo reached the terminal (${JSON.stringify(fake.output.slice(-200))})`)

    // A geometry change reaches the PTY, which the shell itself can confirm.
    fake.resize(100, 30)
    await flush(renderer, 4)
    const listed = await host.call('list', {})
    assert.equal(listed.value.sessions[0].cols, 100)
    assert.equal(listed.value.sessions[0].rows, 30)

    // Closing the tab closes the session.
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const tabRow = find(tree, byClass('dshgw-tty-tab'))
    const closeButton = find(tabRow, byClass('dshgw-tty-tab-x'))
    assert.ok(closeButton !== null, 'the tab offers a close button')
    closeButton.props.onClick({ stopPropagation() {} })
    await flush(renderer, 8)
    const after = await host.call('list', {})
    assert.equal(after.value.sessions.length, 0, 'closing the tab reaps the PTY')

    // Unmounting the panel must not leave a read loop holding a request open.
    renderer.render(null)
    await flush(renderer, 4)
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})
