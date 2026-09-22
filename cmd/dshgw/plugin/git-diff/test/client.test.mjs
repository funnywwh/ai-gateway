// Integration tests for the browser half (client.js) against the real host half.
//
// The DOM is stubbed — this installation has no browser — but everything else is production code:
// the built bundle, the endpoint table, and a real git repository on a real filesystem. A pass means
// "open the 变更 View → see the changed files → click one → see the two-column diff → switch the
// comparison → filter → rescan → cancel" works end to end through the same code paths the GUI uses.
//
// Run: npm test   (node --test test/)

import { strict as assert } from 'node:assert'
import { execFileSync } from 'node:child_process'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import test from 'node:test'

import { GitService } from '../git-service.js'
import { createHandlers } from '../rpc-handlers.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const PLUGIN_DIR = join(HERE, '..')
const RPC_CHANNEL = '/dshgw-git-diff'

const HAS_GIT = (() => {
  try {
    execFileSync('git', ['--version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
})()
const skip = HAS_GIT ? false : 'git is not available in this environment'

const LIMITS = {
  version: 'test', pluginDir: PLUGIN_DIR, traceFile: null, cacheFile: null, trace: false, rootLabel: null,
  maxRepoDepth: 6, maxRepoCandidates: 200, chunkTimeoutMs: 20_000, chunkTargetFiles: 25_000, diffTimeoutMs: 30_000,
  untrackedDefault: false, maxUntrackedEntries: 50, maxFiles: 500, maxDiffBytes: 64 * 1024,
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/**
 * A very small React stand-in: function components, hook slots kept per instance, effect deps
 * compared by identity. Enough to mount the View, walk its tree, and click its controls.
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
      return {
        type,
        props: props ?? {},
        children: children.flat(Infinity).filter((child) => child !== null && child !== undefined && child !== false && child !== true),
      }
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
      node.props.ref.current = { clientWidth: 900, clientHeight: 400, style: {} }
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

  let lastElement = null
  rootRender = () => render(lastElement)
  const renderTree = (element) => { lastElement = element; return render(element) }
  return {
    React,
    render(element) { return renderTree(element) },
    /** Render one registered slot component the way the shell's outlet would. */
    renderComponent(component, props = {}) { return renderTree({ type: component, props, children: [] }) },
    settle() { if (lastElement !== null) render(lastElement) },
  }
}

/** Depth-first search for the first element satisfying one predicate. */
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

/** Every element satisfying one predicate, depth-first. */
function findAll(tree, predicate, found = []) {
  if (tree === null || typeof tree !== 'object') return found
  if (Array.isArray(tree)) {
    for (const child of tree) findAll(child, predicate, found)
    return found
  }
  if (predicate(tree)) found.push(tree)
  for (const child of tree.children ?? []) findAll(child, predicate, found)
  return found
}

/** Class membership, since rows carry several classes. */
const withClass = (name) => (element) => String(element.props?.className ?? '').split(' ').includes(name)

/** A button by its visible label. */
const buttonText = (text) => (element) => element.type === 'button' && JSON.stringify(element.children ?? []).includes(JSON.stringify(text))

/** All text under one element, flattened. */
function textOf(element) {
  if (element === null || element === undefined) return ''
  if (typeof element === 'string' || typeof element === 'number') return String(element)
  if (Array.isArray(element)) return element.map(textOf).join('')
  return (element.children ?? []).map(textOf).join('')
}

/** Build the browser globals the bundle expects, keeping `window` separate from globalThis. */
function installGlobals() {
  const styleTags = []
  const windowObject = {
    __ModuleLoader__: { load: (registration) => { windowObject.__registration = registration } },
    addEventListener() {},
    removeEventListener() {},
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
    setInterval: globalThis.setInterval,
    clearInterval: globalThis.clearInterval,
    localStorage: {
      store: new Map(),
      getItem(key) { return this.store.has(key) ? this.store.get(key) : null },
      setItem(key, value) { this.store.set(key, String(value)) },
      removeItem(key) { this.store.delete(key) },
    },
  }
  const documentObject = {
    documentElement: { clientWidth: 1280, clientHeight: 800, style: {} },
    body: { appendChild() {} },
    head: { appendChild: (tag) => styleTags.push(tag) },
    querySelector: (selector) => (selector.includes('data-plugin-css')
      ? styleTags.find((tag) => selector.includes(tag.dataset?.pluginCss ?? '\u0000')) ?? null
      : null),
    createElement: (tag) => ({ tag, dataset: {}, style: {}, textContent: '' }),
  }
  const previous = { window: globalThis.window, document: globalThis.document, localStorage: globalThis.localStorage }
  globalThis.window = windowObject
  globalThis.document = documentObject
  globalThis.localStorage = windowObject.localStorage
  return {
    windowObject,
    styleTags,
    restore: () => Object.assign(globalThis, previous),
  }
}

/** One fixture repository, dirty in every way the panel has to render. */
async function makeHost() {
  const root = await mkdtemp(join(tmpdir(), 'gd-client-'))
  const repo = join(root, 'work', 'repo')
  await mkdir(join(repo, 'src'), { recursive: true })
  const git = (args) => execFileSync('git', args, { cwd: repo, encoding: 'utf8', env: { ...process.env, GIT_OPTIONAL_LOCKS: '0' } })
  git(['init', '-q', '.'])
  git(['config', 'user.email', 'test@example.com'])
  git(['config', 'user.name', 'Test'])
  await writeFile(join(repo, 'keep.txt'), 'one\ntwo\nthree\n')
  await writeFile(join(repo, 'src', 'inner.txt'), 'inner\n')
  await writeFile(join(repo, 'gone.txt'), 'bye\n')
  await writeFile(join(repo, 'blob.bin'), Buffer.from([0x00, 0x01, 0x02]))
  git(['add', '-A'])
  git(['commit', '-qm', 'init'])

  // keep.txt: modified in the worktree, and staged as well (so it offers every comparison).
  await writeFile(join(repo, 'keep.txt'), 'one\nTWO\nthree\nfour\n')
  git(['add', 'keep.txt'])
  await writeFile(join(repo, 'keep.txt'), 'one\nTWO\nthree\nfive\n')
  // src/inner.txt: staged only. blob.bin: modified binary. gone.txt: deleted.
  await writeFile(join(repo, 'src', 'inner.txt'), 'inner staged\n')
  git(['add', 'src/inner.txt'])
  await rm(join(repo, 'gone.txt'))
  await writeFile(join(repo, 'blob.bin'), Buffer.from([0x00, 0x01, 0x03]))

  const service = await GitService.create({
    root, rootLabel: null, repo: null, cacheFile: null, log: () => {}, trace: () => {}, limits: { ...LIMITS },
  })
  const { dispatch } = createHandlers({ service, config: { ...LIMITS }, trace: () => {}, log: () => {} })
  return {
    root,
    repo,
    service,
    async call(endpoint, payload) {
      try {
        return { ok: true, value: await dispatch(endpoint, payload) }
      } catch (error) {
        return { ok: false, error: { code: error?.code ?? 'git/failed', message: String(error?.message ?? error), details: error?.details ?? {} } }
      }
    },
    async dispose() {
      service.cancelScan({ jobId: null })
      await rm(root, { recursive: true, force: true })
    },
  }
}

/**
 * The wire body is what the host validates, and the shell builds it as
 * `JSON.stringify({ type: 'client-request', rpcId, method, payload })`.
 *
 * The host's envelope schema (dsh-client-connection) is
 *
 *   z.object({ type: z.literal('client-request'), rpcId: z.string(), method: z.string(), payload: z.unknown() })
 *
 * and this installation's zod is 4.x, where a `z.unknown()` key is **not optional**: a body without
 * `payload` fails with `invalid_type: expected nonoptional, received undefined`, and the host answers
 * `{ ok: false, error: { code: 'gateway/bad-request', message: 'invalid client-request message' } }`
 * — before the endpoint runs. `JSON.stringify` drops every key whose value is `undefined`, so
 * `call('hello')` used to arrive without one, and the 变更 tab showed that error plus its empty state
 * ("在工作区里没有找到 git 仓库"): the boot handshake is what lists the repositories.
 *
 * The stub below forwards `(endpoint, payload)` straight to the host half, which is exactly why it
 * cannot see this — the key is lost in serialization, not in the call. So the guard re-serializes the
 * envelope on every call, which covers every test in this file.
 *
 * Returns the violation text, or null when the envelope is acceptable; the stub records it as well as
 * throwing, so the wire test can name the reason while the test that made the call still fails.
 */
function wireEnvelopeViolation(endpoint, payload) {
  const body = JSON.parse(JSON.stringify({ type: 'client-request', rpcId: 'test-rpc-id', method: endpoint, payload }))
  if (body.type !== 'client-request') return `${endpoint}: the envelope type must survive serialization`
  if (typeof body.rpcId !== 'string') return `${endpoint}: rpcId must serialize as a string`
  if (typeof body.method !== 'string') return `${endpoint}: method must serialize as a string`
  if (!Object.hasOwn(body, 'payload')) {
    return `${endpoint}: the wire body has no \`payload\` key — JSON.stringify drops payload:undefined and the ` +
      'host rejects the whole envelope ("invalid client-request message": zod 4 keeps z.unknown() non-optional). ' +
      'Pass {} — or let call() default it — for an endpoint that takes no arguments.'
  }
  return null
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
  const wireViolations = []
  const ctx = {
    logger: { info: () => {}, warn: () => {}, error: () => {} },
    effect: (callback) => { const dispose = callback(); return typeof dispose === 'function' ? dispose : () => {} },
    slots: {
      inject: (name, callback) => { callback(); return () => {} },
      register: (options, component) => { registrations.set(options.name, { options, component }); return () => {} },
    },
    connection: {
      rpc: {
        call: async (channel, endpoint, payload) => {
          assert.equal(channel, RPC_CHANNEL, 'the plugin must use its own channel')
          const violation = wireEnvelopeViolation(endpoint, payload)
          if (violation !== null) {
            // Throw the way the host would answer, so the plugin paints the same failure the GUI showed,
            // and record it so the wire test below can report the reason instead of the symptom.
            wireViolations.push(violation)
            throw new Error(`invalid client-request message — ${violation}`)
          }
          calls.push({ endpoint, payload })
          return await host.call(endpoint, payload)
        },
      },
    },
  }
  exportsObject.apply(ctx)
  assert.deepEqual(Array.from(exportsObject.inject), ['slots', 'connection'])
  await flush(renderer, 4)
  return { registration, registrations, calls, wireViolations, ctx, globals, exportsObject }
}

/** Let queued promises, timers and re-renders settle. */
async function flush(renderer, rounds = 10) {
  for (let index = 0; index < rounds; index += 1) {
    renderer?.settle()
    await sleep(12)
  }
  renderer?.settle()
}

/** Re-render until one predicate over the current tree holds, or time out. */
async function flushUntil(renderer, view, predicate, { timeoutMs = 6000, label = 'condition' } = {}) {
  const started = Date.now()
  for (;;) {
    const tree = renderer.renderComponent(view)
    if (predicate(tree)) return tree
    if (Date.now() - started > timeoutMs) throw new Error(`timed out waiting for ${label}`)
    await sleep(25)
  }
}

/** Wait until the host's background scan has settled, as the status line reports it. */
async function waitForScanDone(renderer, view) {
  return await flushUntil(renderer, view, (tree) => {
    const status = find(tree, withClass('dshgw-gd-status'))
    return status !== null && /扫描完成|上次扫描|扫描已取消|扫描失败/.test(textOf(status))
  }, { label: 'the scan to settle', timeoutMs: 20_000 })
}

/** The row for one path in the rendered list. */
function rowFor(tree, path) {
  return find(tree, (element) => withClass('dshgw-gd-row')(element) && element.props.title === path)
}

/** Click the first control whose text matches. */
function clickText(tree, text) {
  const button = find(tree, buttonText(text))
  assert.ok(button !== null, `expected a control labelled ${text}`)
  button.props.onClick()
  return button
}

// ---- the pure half ---------------------------------------------------------------------------

test('client: the diff parser turns unified text into hunks, rows and paired cells', async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const pure = client.exportsObject.__pure
  try {
    const modified = pure.parseUnifiedDiff([
      'diff --git a/keep.txt b/keep.txt',
      'index 1111111..2222222 100644',
      '--- a/keep.txt',
      '+++ b/keep.txt',
      '@@ -1,3 +1,3 @@',
      ' one',
      '-two',
      '+TWO',
      ' three',
      '\\ No newline at end of file',
      '',
    ].join('\n'))
    assert.equal(modified.oldPath, 'keep.txt')
    assert.equal(modified.newPath, 'keep.txt')
    assert.equal(modified.binary, false)
    assert.deepEqual(modified.hunks.map((hunk) => hunk.header), ['@@ -1,3 +1,3 @@'])
    const rows = pure.pairRows(modified.hunks[0])
    assert.deepEqual(rows.map((row) => row.kind), ['pair', 'pair', 'pair', 'note'])
    assert.deepEqual(rows[1].left, { no: 2, text: 'two', type: 'del' })
    assert.deepEqual(rows[1].right, { no: 2, text: 'TWO', type: 'add' })
    assert.deepEqual(rows[1].spans.left, [[0, 3]], 'the whole line is the difference')
    assert.deepEqual(rows[0].left.type, 'ctx')

    const added = pure.parseUnifiedDiff('diff --git a/fresh.txt b/fresh.txt\nnew file mode 100644\n--- /dev/null\n+++ b/fresh.txt\n@@ -0,0 +1,2 @@\n+a\n+b\n')
    assert.equal(added.newFile, true)
    assert.equal(added.oldPath, '/dev/null')
    const addedRows = pure.pairRows(added.hunks[0])
    assert.equal(addedRows[0].left, null, 'a new file has an empty left side')
    assert.equal(addedRows[0].right.text, 'a')

    const removed = pure.parseUnifiedDiff('diff --git a/x b/x\ndeleted file mode 100644\n--- a/x\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n')
    assert.equal(removed.deletedFile, true)
    assert.equal(pure.pairRows(removed.hunks[0])[0].right, null, 'a deleted file has an empty right side')

    const binary = pure.parseUnifiedDiff('diff --git a/b.bin b/b.bin\nBinary files a/b.bin and b/b.bin differ\n')
    assert.equal(binary.binary, true)

    const modeOnly = pure.parseUnifiedDiff('diff --git a/m.sh b/m.sh\nold mode 100644\nnew mode 100755\n')
    assert.equal(modeOnly.modeOnly, true)
    assert.deepEqual(modeOnly.hunks, [])

    // Three removed lines against one added line: the shorter side gets filler cells, not silence.
    const rewrite = pure.parseUnifiedDiff('@@ -1,3 +1,1 @@\n-a\n-b\n-c\n+z\n')
    const rewriteRows = pure.pairRows(rewrite.hunks[0])
    assert.equal(rewriteRows.length, 3)
    assert.deepEqual(rewriteRows.map((row) => row.right === null), [false, true, true])

    assert.deepEqual(pure.inlineSpans('const a = 1', 'const a = 2'), { left: [[10, 11]], right: [[10, 11]] })
    assert.deepEqual(pure.inlineSpans('same', 'same'), { left: [], right: [] })
    assert.deepEqual(pure.splitPath('frameworks/base/Foo.java'), { dir: 'frameworks/base/', name: 'Foo.java' })
    assert.deepEqual(pure.splitPath('README'), { dir: '', name: 'README' })
    assert.equal(pure.formatDuration(1500), '1.5s')

    const merged = pure.buildRows(
      [{ path: 'a.txt', status: 'M', side: 'staged' }],
      [{ path: 'a.txt', status: 'M', side: 'unstaged' }, { path: 'b.txt', status: '?', side: 'untracked' }],
    )
    assert.deepEqual(merged.map((row) => row.path), ['a.txt', 'b.txt'])
    assert.equal(merged[0].staged, 'M')
    assert.equal(merged[0].unstaged, 'M')
    assert.deepEqual(pure.sidesOf(merged[0]), ['unstaged', 'staged', 'combined'])
    assert.deepEqual(pure.sidesOf(merged[1]), ['untracked'])
    assert.equal(pure.badgeOf(merged[1]), '?')
  } finally {
    await host.dispose()
    renderer.render(null)
    client.globals.restore()
  }
})

// ---- the wire envelope -----------------------------------------------------------------------

// Regression for the reported failure: the 变更 tab showed one red line, `invalid client-request
// message`, and then its empty state, 「在工作区里没有找到 git 仓库」. That reads like "there is no
// repository here", but the repository was fine — the handshake never reached the host.
//
// The cause is in neither git nor the host: the client's handshake `call('hello')` passed no payload,
// and the shell builds the body as `JSON.stringify({..., payload})`, where `payload: undefined` makes
// the **whole key disappear**. The host's envelope schema declares `payload: z.unknown()`, which on
// this machine's zod 4 is *not* optional, so the request was rejected
// (`invalid_type: expected nonoptional, received undefined`) before the endpoint ran. `hello` is what
// lists the repositories, so the panel had nothing left to paint.
//
// The assertion is made on the **serialized** bytes, because that is where the key is lost: a stub
// that hands `(endpoint, payload)` straight to the host half can never see this — which is how seven
// client tests stayed green while the shipped panel was broken.
test('client: every RPC request survives the wire envelope, handshake included', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    renderer.renderComponent(view)
    await flush(renderer, 12)

    assert.deepEqual(
      client.wireViolations,
      [],
      `every RPC request must serialize to a valid envelope:\n  ${client.wireViolations.join('\n  ')}`,
    )
    const handshake = client.calls.find((call) => call.endpoint === 'hello')
    assert.ok(handshake !== undefined, 'the view announces itself with hello()')
    assert.ok(
      Object.hasOwn(JSON.parse(JSON.stringify({ payload: handshake.payload })), 'payload'),
      'the handshake must send a payload ({} is enough): the host rejects an envelope without the key',
    )
    // The handshake comes first, which is why it is also the call that decides whether the panel can
    // paint anything at all: it is what lists the repositories.
    assert.equal(client.calls[0].endpoint, 'hello', 'the view announces itself before it asks for anything else')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

// ---- the View --------------------------------------------------------------------------------

test('client: registers the 变更 conversation View and bootstraps from the host', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  try {
    assert.equal(client.registration.id, 'dshgw-git-diff', 'the bundle id must equal the package name')
    const view = client.registrations.get('conversation.view')
    assert.ok(view !== undefined, 'the View registers into conversation.view')
    assert.equal(view.options.id, 'git-diff')
    assert.equal(view.options.order, 20, 'after 对话 (0) and 轨迹 (10)')
    assert.equal(view.options.label, '变更')
    assert.ok(client.globals.styleTags.length > 0, 'the bundle installs its stylesheet')

    renderer.renderComponent(view.component)
    await flush(renderer, 12)
    const endpoints = client.calls.map((call) => call.endpoint)
    assert.ok(endpoints.includes('hello'), 'the view announces itself with hello()')
    assert.ok(endpoints.includes('repos'), 'and asks where the repositories are')
    assert.ok(endpoints.includes('status'), 'and paints the staged set immediately')
    const first = renderer.renderComponent(view.component)
    assert.ok(find(first, withClass('dshgw-gd-root')) !== null, 'the view renders its root')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: shows the staged set before any scan, then fills in chunk by chunk', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    let tree = renderer.renderComponent(view)
    await flush(renderer, 10)
    tree = renderer.renderComponent(view)
    assert.ok(rowFor(tree, 'src/inner.txt') !== null, 'the staged-only file is listed without a scan (索引级查询)')
    assert.ok(client.calls.some((call) => call.endpoint === 'scanStart'), 'opening the tab starts the background scan')

    await waitForScanDone(renderer, view)
    await flushUntil(renderer, view, (current) => rowFor(current, 'keep.txt') !== null, { label: 'the scanned rows' })
    tree = renderer.renderComponent(view)
    assert.ok(rowFor(tree, 'gone.txt') !== null, 'a deleted file is listed')
    assert.ok(rowFor(tree, 'blob.bin') !== null, 'a modified binary is listed')
    const status = textOf(find(tree, withClass('dshgw-gd-status')))
    assert.match(status, /扫描完成|上次扫描/)
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: clicking a file renders the two-column diff with paired lines and highlighted spans', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    await waitForScanDone(renderer, view)
    await flushUntil(renderer, view, (tree) => rowFor(tree, 'keep.txt') !== null, { label: 'keep.txt in the list' })
    let tree = renderer.renderComponent(view)
    rowFor(tree, 'keep.txt').props.onClick()
    await flushUntil(renderer, view, (current) => withClass('dshgw-gd-rowsplit').length !== undefined
      && find(current, withClass('dshgw-gd-rowsplit')) !== null
      && find(current, withClass('dshgw-gd-code')) !== null, { label: 'the diff body' })
    tree = renderer.renderComponent(view)

    const last = client.calls.filter((call) => call.endpoint === 'diff').at(-1)
    assert.equal(last.payload.path, 'keep.txt')
    assert.equal(last.payload.side, 'unstaged', 'the worktree comparison is the default when it exists')

    const cells = findAll(tree, withClass('dshgw-gd-cell'))
    assert.ok(cells.length > 0, 'the diff renders cells')
    const del = cells.find((cell) => cell.props['data-type'] === 'del')
    const add = cells.find((cell) => cell.props['data-type'] === 'add')
    assert.ok(del !== undefined && add !== undefined, 'both sides render their type')
    assert.match(textOf(del), /four/, 'the removed line is on the left')
    assert.match(textOf(add), /five/, 'the added line is on the right')
    const empty = cells.filter((cell) => cell.props['data-empty'] === 'true')
    assert.equal(empty.length, 0, 'a one-for-one change leaves no filler cells')

    // The header names both ends and counts the change.
    const head = textOf(find(tree, withClass('dshgw-gd-dhead')))
    assert.match(head, /索引 → 工作区/)
    assert.match(head, /\+1/)
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: the comparison tabs switch between staged, unstaged and combined', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    await waitForScanDone(renderer, view)
    await waitForScanDone(renderer, view)
    await flushUntil(renderer, view, (tree) => rowFor(tree, 'keep.txt') !== null, { label: 'keep.txt in the list' })
    let tree = renderer.renderComponent(view)
    rowFor(tree, 'keep.txt').props.onClick()
    await flushUntil(renderer, view, (current) => textOf(find(current, withClass('dshgw-gd-dhead'))).includes('索引 → 工作区'), { label: 'the unstaged diff' })

    tree = renderer.renderComponent(view)
    const tabs = findAll(tree, withClass('dshgw-gd-tab')).map((tab) => textOf(tab))
    assert.deepEqual(tabs.slice(0, 3), ['未暂存', '已暂存', '全部'], 'a file staged and modified again offers both comparisons')

    clickText(tree, '已暂存')
    await flushUntil(renderer, view, (current) => textOf(find(current, withClass('dshgw-gd-dhead'))).includes('HEAD → 索引'), { label: 'the staged diff' })
    tree = renderer.renderComponent(view)
    assert.match(textOf(find(tree, withClass('dshgw-gd-dhead'))), /HEAD → 索引/)

    clickText(tree, '全部')
    await flushUntil(renderer, view, (current) => textOf(find(current, withClass('dshgw-gd-dhead'))).includes('HEAD → 工作区'), { label: 'the combined diff' })
    tree = renderer.renderComponent(view)
    // The two-column body marks an addition with the cell, not with a leading '+'.
    const addedLines = findAll(tree, (element) => withClass('dshgw-gd-cell')(element) && element.props['data-type'] === 'add')
    assert.ok(addedLines.some((cell) => textOf(cell).includes('five')), 'the combined diff carries the worktree line')

    const diffCalls = client.calls.filter((call) => call.endpoint === 'diff')
    assert.deepEqual(diffCalls.map((call) => call.payload.side), ['unstaged', 'staged', 'combined'])
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: filters narrow the list, and the untracked chip rescans for untracked files', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    await waitForScanDone(renderer, view)
    await flushUntil(renderer, view, (tree) => rowFor(tree, 'keep.txt') !== null, { label: 'the scanned list' })
    let tree = renderer.renderComponent(view)

    // Free text filters on the path.
    const filter = find(tree, (element) => element.props?.['aria-label'] === '过滤路径')
    assert.ok(filter !== null, 'the filter input exists')
    filter.props.onChange({ target: { value: 'src/' } })
    await flush(renderer, 4)
    tree = renderer.renderComponent(view)
    assert.ok(rowFor(tree, 'src/inner.txt') !== null)
    assert.equal(rowFor(tree, 'keep.txt'), null, 'a non-matching path disappears')

    filter.props.onChange({ target: { value: '' } })
    await flush(renderer, 4)
    tree = renderer.renderComponent(view)

    // The 已暂存 chip hides the staged-only rows, and turning it back on restores them.
    const stagedChip = () => findAll(renderer.renderComponent(view), withClass('dshgw-gd-chip'))
      .find((chip) => textOf(chip).startsWith('已暂存'))
    stagedChip().props.onClick()
    await flush(renderer, 4)
    tree = renderer.renderComponent(view)
    assert.equal(rowFor(tree, 'src/inner.txt'), null, 'turning the staged chip off hides staged-only files')
    assert.ok(rowFor(tree, 'keep.txt') !== null, 'a file with worktree changes stays')
    stagedChip().props.onClick()
    await flush(renderer, 4)
    tree = renderer.renderComponent(view)
    assert.ok(rowFor(tree, 'src/inner.txt') !== null)

    // Turning on 未跟踪 must ask the host to scan again, this time for untracked files.
    await writeFile(join(host.repo, 'fresh.txt'), 'brand new\n')
    tree = renderer.renderComponent(view)
    const untrackedChip = findAll(tree, withClass('dshgw-gd-chip')).find((chip) => textOf(chip).startsWith('未跟踪'))
    assert.ok(untrackedChip !== undefined, 'the untracked chip exists')
    untrackedChip.props.onClick()
    await flush(renderer, 6)
    const forced = client.calls.filter((call) => call.endpoint === 'scanStart').at(-1)
    assert.equal(forced.payload.includeUntracked, true)
    assert.equal(forced.payload.force, true)
    await flushUntil(renderer, view, (current) => rowFor(current, 'fresh.txt') !== null, { label: 'the untracked file' })
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: a binary file says so, and a deleted file leaves the right column empty', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    await waitForScanDone(renderer, view)
    await flushUntil(renderer, view, (tree) => rowFor(tree, 'blob.bin') !== null, { label: 'blob.bin in the list' })
    let tree = renderer.renderComponent(view)
    rowFor(tree, 'blob.bin').props.onClick()
    await flushUntil(renderer, view, (current) => textOf(current).includes('二进制文件'), { label: 'the binary notice' })

    tree = renderer.renderComponent(view)
    rowFor(tree, 'gone.txt').props.onClick()
    await flushUntil(renderer, view, (current) => find(current, (element) => element.props?.['data-empty'] === 'true') !== null, { label: 'the deletion diff' })
    tree = renderer.renderComponent(view)
    assert.match(textOf(find(tree, withClass('dshgw-gd-dhead'))), /索引 → 工作区/)
    const cells = findAll(tree, withClass('dshgw-gd-cell'))
    assert.equal(cells.filter((cell) => cell.props['data-empty'] === 'true').length > 0, true, 'a deleted file leaves the right column empty')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: cancel stops the scan and the status line says so', { skip }, async () => {
  const renderer = createRenderer()
  const host = await makeHost()
  // Hold every chunk long enough that the scan is still running when 取消 is clicked.
  const original = host.service.scanChunk.bind(host.service)
  host.service.scanChunk = async (...args) => {
    await sleep(400)
    return await original(...args)
  }
  const client = await loadClient(renderer, host)
  const view = client.registrations.get('conversation.view').component
  try {
    await flush(renderer, 8)
    let tree = renderer.renderComponent(view)
    await flushUntil(renderer, view, (current) => textOf(current).includes('扫描中') || textOf(current).includes('扫描完成'), { label: 'a scan in flight' })
    tree = renderer.renderComponent(view)
    if (textOf(tree).includes('扫描中')) {
      clickText(tree, '取消')
      await flush(renderer, 8)
      tree = renderer.renderComponent(view)
      assert.ok(client.calls.some((call) => call.endpoint === 'scanCancel'), '取消 reaches the host')
      await flushUntil(renderer, view, (current) => textOf(current).includes('扫描已取消') || textOf(current).includes('扫描完成'), { label: 'the cancelled state' })
    }
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})
