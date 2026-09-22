// Integration test for the browser half (client.js) against the real host half.
//
// The DOM is stubbed — this installation has no browser — but everything else is production code:
// the built bundle, the endpoint table, and a real workspace on a real filesystem. A pass means
// "open panel → list → enter a directory → open a file → edit → save → upload → download → mkdir →
// rename → delete" works end to end through the same code paths the GUI uses.
//
// Run: npm test   (node --test test/)

import { strict as assert } from 'node:assert'
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import test from 'node:test'

import { WorkspaceFiles } from '../fs-service.js'
import { createHandlers } from '../rpc-handlers.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const PLUGIN_DIR = join(HERE, '..')
const RPC_CHANNEL = '/dshgw-workspace-files'

const LIMITS = {
  maxTextBytes: 64 * 1024,
  chunkBytes: 1024,
  maxListEntries: 500,
  maxUploadBytes: 1024 * 1024,
  maxSearchResults: 50,
  maxSearchDepth: 4,
  maxSearchVisits: 5000,
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/** Let queued promises, timers and re-renders settle. */
async function flush(renderer, rounds = 12) {
  for (let index = 0; index < rounds; index += 1) {
    renderer?.settle()
    await sleep(12)
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

/** The click event stub every handler expects. */
const clickEvent = () => ({ stopPropagation() {}, preventDefault() {}, button: 0, target: null })

/** Build the browser globals the bundle expects, keeping `window` separate from globalThis. */
function installGlobals() {
  const styleTags = []
  const downloads = []
  const objectUrls = []
  const anchors = []
  const windowObject = {
    __ModuleLoader__: { load: (registration) => { windowObject.__registration = registration } },
    addEventListener() {},
    removeEventListener() {},
    btoa: globalThis.btoa,
    atob: globalThis.atob,
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
    URL: {
      createObjectURL(blob) {
        const url = `blob:test/${objectUrls.length}`
        objectUrls.push({ url, blob })
        return url
      },
      revokeObjectURL() {},
    },
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
    createElement: (tag) => {
      if (tag === 'a') {
        const anchor = { tag, href: '', download: '', rel: '', clicked: 0, click() { this.clicked += 1; downloads.push(this) }, remove() {} }
        anchors.push(anchor)
        return anchor
      }
      return { tag, dataset: {}, style: {}, textContent: '' }
    },
  }
  const previous = { window: globalThis.window, document: globalThis.document, localStorage: globalThis.localStorage }
  globalThis.window = windowObject
  globalThis.document = documentObject
  globalThis.localStorage = windowObject.localStorage
  return {
    windowObject,
    styleTags,
    downloads,
    objectUrls,
    anchors,
    restore: () => Object.assign(globalThis, previous),
  }
}

/** One temporary workspace plus the endpoint table over it. */
async function startHost() {
  const root = await mkdtemp(join(tmpdir(), 'wsf-client-'))
  await writeFile(join(root, 'hello.txt'), 'hello 工作区\n')
  await writeFile(join(root, 'blob.bin'), Buffer.from([0x00, 0x01, 0x02, 0xff]))
  await mkdir(join(root, '视频'), { recursive: true })
  await writeFile(join(root, '视频', 'clip.mp4'), Buffer.from('video-bytes'))
  await mkdir(join(root, 'sub'), { recursive: true })
  await writeFile(join(root, 'sub', 'note.md'), '# note\n')
  const service = await WorkspaceFiles.create({ root, limits: LIMITS, log: () => {} })
  const config = { version: 'test', pluginDir: PLUGIN_DIR, traceFile: null, trace: false, ...LIMITS }
  const { dispatch } = createHandlers({ service, config, trace: () => {}, log: () => {} })
  return {
    root,
    async call(endpoint, payload) {
      try {
        return { ok: true, value: await dispatch(endpoint, payload) }
      } catch (error) {
        return { ok: false, error: { code: error?.code ?? 'files/failed', message: String(error?.message ?? error), details: error?.details ?? {} } }
      }
    },
    async dispose() { await rm(root, { recursive: true, force: true }) },
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
        call: async (channel, endpoint, payload) => {
          assert.equal(channel, RPC_CHANNEL, 'the plugin must use its own channel')
          calls.push({ endpoint, payload })
          return await host.call(endpoint, payload)
        },
      },
    },
  }
  exportsObject.apply(ctx)
  assert.deepEqual(Array.from(exportsObject.inject), ['slots', 'connection'])
  await flush(renderer, 4)
  return { registration, registrations, calls, ctx, globals, exportsObject }
}

/** Open the panel through its sidebar row and wait for the first listing. */
async function openPanel(renderer, client) {
  const entryTree = renderer.renderComponent(client.registrations.get('sidebar.footer.action'))
  const entry = find(entryTree, withClass('dshgw-wsf-entry'))
  assert.ok(entry !== null, 'the sidebar row renders a button')
  entry.props.onClick()
  await flush(renderer, 6)
  renderer.renderComponent(client.registrations.get('shell.overlay'))
  // The panel loads its directory in a mount effect; give that round trip time to land.
  await flush(renderer, 10)
  const tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
  const panel = find(tree, withClass('dshgw-wsf-panel'))
  assert.ok(panel !== null, 'the panel mounts once it is open')
  return tree
}

/** One rendered control by its aria-label, restricted to one element type. */
function controlByLabel(tree, type, label) {
  return find(tree, (element) => element.type === type && element.props?.['aria-label'] === label)
}

/** The open dialog's own subtree: row actions and dialog buttons can share a label. */
function dialogOf(tree) {
  const dialog = find(tree, (element) => String(element.props?.className ?? '').split(' ').includes('dshgw-wsf-dialog'))
  assert.ok(dialog !== null, 'a dialog is open')
  return dialog
}

/** One button inside the open dialog. */
function dialogButton(tree, text) {
  const button = find(dialogOf(tree), buttonText(text))
  assert.ok(button !== null, `the dialog owns a ${text} button`)
  return button
}

/** The rendered row whose title is one workspace path. */
function rowFor(tree, path) {
  return find(tree, (element) => withClass('dshgw-wsf-row')(element) && element.props.title === path)
}

test('client bundle: registers its channel, both slots, and self-identifies against the host', async () => {
  const renderer = createRenderer()
  const host = await startHost()
  const client = await loadClient(renderer, host)
  try {
    assert.equal(client.registration.id, 'dshgw-workspace-files', 'the bundle id must equal the package name')
    assert.ok(client.registrations.has('sidebar.footer.action'), 'the panel needs a sidebar affordance')
    assert.ok(client.registrations.has('shell.overlay'), 'the panel lives in the frame-wide overlay slot')
    assert.ok(client.globals.styleTags.length > 0, 'the bundle installs its stylesheet')
    const hello = client.calls.find((call) => call.endpoint === 'hello')
    assert.ok(hello !== undefined, 'the panel announces itself with hello()')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: browses directories, opens a text file, edits it, and saves', async () => {
  const renderer = createRenderer()
  const host = await startHost()
  const client = await loadClient(renderer, host)
  try {
    let tree = await openPanel(renderer, client)
    const listed = client.calls.filter((call) => call.endpoint === 'list')
    assert.ok(listed.length > 0, 'opening the panel lists the workspace root')
    assert.equal(listed[0].payload.path, '')

    // Directories lead and are clickable; entering one lists it.
    assert.ok(rowFor(tree, 'sub') !== null, 'the listing renders a row per entry')
    assert.ok(rowFor(tree, 'hello.txt') !== null, 'files are rows too')
    const subRow = rowFor(tree, 'sub')
    const subName = find(subRow, withClass('dshgw-wsf-name'))
    subName.props.onClick(clickEvent())
    await flush(renderer, 8)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(tree, 'sub/note.md') !== null, 'entering a directory lists its children')
    const crumbs = findAll(tree, withClass('dshgw-wsf-crumb')).map((crumb) => JSON.stringify(crumb.children))
    assert.ok(crumbs.some((label) => label.includes('sub')), 'the breadcrumb shows the current directory')

    // Back to the root, then open the text file: it opens in the editor.
    const rootCrumb = findAll(tree, withClass('dshgw-wsf-crumb'))[0]
    rootCrumb.props.onClick()
    await flush(renderer, 8)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    rowFor(tree, 'hello.txt').props.onDoubleClick()
    await flush(renderer, 8)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const editor = find(tree, withClass('dshgw-wsf-editor'))
    assert.ok(editor !== null, 'a text file opens in the editor')
    assert.equal(editor.props.value, 'hello 工作区\n')

    // Editing marks the buffer dirty, which is what enables Save.
    editor.props.onChange({ target: { value: 'hello 工作区\n第三行\n' } })
    await flush(renderer, 4)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const save = find(tree, buttonText('保存'))
    assert.ok(save !== null, 'the viewer offers Save')
    assert.equal(save.props.disabled, false, 'Save enables once the buffer differs')
    save.props.onClick()
    await flush(renderer, 10)
    assert.equal(await readFile(join(host.root, 'hello.txt'), 'utf8'), 'hello 工作区\n第三行\n', 'saving writes the file on the host')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: creates a folder, renames an entry, and deletes it again', async () => {
  const renderer = createRenderer()
  const host = await startHost()
  const client = await loadClient(renderer, host)
  try {
    let tree = await openPanel(renderer, client)

    // New folder: the dialog stages a name and commits it.
    find(tree, buttonText('＋ 文件夹')).props.onClick()
    await flush(renderer, 4)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const input = controlByLabel(tree, 'input', '新建文件夹')
    assert.ok(input !== null, 'the dialog asks for a folder name')
    input.props.onChange({ target: { value: '新目录' } })
    await flush(renderer, 4)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    dialogButton(tree, '创建').props.onClick()
    await flush(renderer, 10)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(tree, '新目录') !== null, 'the new folder appears in the refreshed listing')

    // Rename through the row action.
    const folderRow = rowFor(tree, '新目录')
    find(folderRow, buttonText('重命名')).props.onClick(clickEvent())
    await flush(renderer, 4)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const renameInput = controlByLabel(tree, 'input', '重命名')
    assert.equal(renameInput.props.value, '新目录', 'rename is prefilled with the current name')
    renameInput.props.onChange({ target: { value: '改名后' } })
    await flush(renderer, 4)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    dialogButton(tree, '重命名').props.onClick()
    await flush(renderer, 10)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(tree, '改名后') !== null, 'the renamed folder is listed')
    assert.ok(rowFor(tree, '新目录') === null, 'the old name is gone')

    // Delete it, with the confirmation dialog.
    find(rowFor(tree, '改名后'), buttonText('删除')).props.onClick(clickEvent())
    await flush(renderer, 4)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    dialogButton(tree, '删除').props.onClick()
    await flush(renderer, 10)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(tree, '改名后') === null, 'the folder is gone from the listing')
    await assert.rejects(() => readFile(join(host.root, '改名后'), 'utf8'))
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: uploads through chunked writes and downloads the same bytes back', async () => {
  const renderer = createRenderer()
  const host = await startHost()
  const client = await loadClient(renderer, host)
  try {
    let tree = await openPanel(renderer, client)

    // Upload two files: one larger than the 1 KiB chunk, so the loop runs more than once.
    const payload = Buffer.alloc(2500, 7)
    const file = new File([payload], 'large.bin')
    const small = new File([Buffer.from('tiny')], 'tiny.txt')
    const input = find(tree, (element) => element.props?.type === 'file')
    assert.ok(input !== null, 'the toolbar owns a file input')
    input.props.onChange({ target: { files: [file, small], value: '' } })
    await flush(renderer, 30)
    const writes = client.calls.filter((call) => call.endpoint === 'writeChunk')
    assert.ok(writes.length >= 4, `the upload loop chunks both files (${writes.length} writes)`)
    assert.deepEqual(await readFile(join(host.root, 'large.bin')), payload, 'the uploaded bytes land on disk')
    assert.equal(await readFile(join(host.root, 'tiny.txt'), 'utf8'), 'tiny')

    // Download it back: the client reassembles the chunks into one blob and hands it to the browser.
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const largeRow = rowFor(tree, 'large.bin')
    assert.ok(largeRow !== null, 'the uploaded file is listed')
    find(largeRow, buttonText('下载')).props.onClick(clickEvent())
    await flush(renderer, 30)
    const reads = client.calls.filter((call) => call.endpoint === 'readChunk')
    assert.ok(reads.length >= 3, `the download loop walks the file in chunks (${reads.length} reads)`)
    assert.equal(client.globals.downloads.length, 1, 'one download was handed to the browser')
    assert.equal(client.globals.downloads[0].download, 'large.bin')
    const blob = client.globals.objectUrls.at(-1).blob
    assert.deepEqual(Buffer.from(await blob.arrayBuffer()), payload, 'the downloaded blob matches the file')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: previews an image and a video, and refuses to edit a binary file', async () => {
  const renderer = createRenderer()
  const host = await startHost()
  const client = await loadClient(renderer, host)
  try {
    await writeFile(join(host.root, 'pixel.png'), Buffer.from([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]))
    let tree = await openPanel(renderer, client)
    find(tree, buttonText('⟳ 刷新')).props.onClick()
    await flush(renderer, 12)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(tree, 'pixel.png') !== null, 'refresh picks up a file created outside the panel')

    // An image previews as a blob URL rather than in the editor.
    rowFor(tree, 'pixel.png').props.onDoubleClick()
    await flush(renderer, 12)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    const image = find(tree, (element) => element.type === 'img')
    assert.ok(image !== null, 'an image opens as a blob preview')
    assert.match(image.props.src, /^blob:test\//)
    assert.ok(find(tree, withClass('dshgw-wsf-editor')) === null, 'an image is not opened as text')

    // Same for a video, one directory down.
    find(tree, buttonText('← 返回')).props.onClick()
    await flush(renderer, 8)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    rowFor(tree, '视频').props.onDoubleClick()
    await flush(renderer, 10)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(tree, '视频/clip.mp4') !== null, 'the nested directory lists its file')
    rowFor(tree, '视频/clip.mp4').props.onDoubleClick()
    await flush(renderer, 12)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(find(tree, (element) => element.type === 'video') !== null, 'a video opens as a blob preview')

    // A binary file with no preview: the viewer says so and offers only Download.
    find(tree, buttonText('← 返回')).props.onClick()
    await flush(renderer, 8)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    find(tree, buttonText('⌂ 根')).props.onClick()
    await flush(renderer, 10)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    rowFor(tree, 'blob.bin').props.onDoubleClick()
    await flush(renderer, 12)
    tree = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(find(tree, withClass('dshgw-wsf-editor')) === null, 'a binary file never opens in the editor')
    assert.ok(find(tree, buttonText('下载')) !== null, 'the binary viewer still offers a download')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})

test('client: searches file names and opens a hit', async () => {
  const renderer = createRenderer()
  const host = await startHost()
  const client = await loadClient(renderer, host)
  try {
    const tree = await openPanel(renderer, client)
    const search = controlByLabel(tree, 'input', '搜索文件名')
    assert.ok(search !== null, 'the toolbar owns a search field')
    search.props.onChange({ target: { value: 'note' } })
    await flush(renderer, 30) // the input debounces before it asks the host
    const found = client.calls.find((call) => call.endpoint === 'find')
    assert.ok(found !== undefined, 'the query reaches the host')
    const settled = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(rowFor(settled, 'sub/note.md') !== null, 'the hit is listed')
    rowFor(settled, 'sub/note.md').children[1].props.onClick()
    await flush(renderer, 10)
    const opened = renderer.renderComponent(client.registrations.get('shell.overlay'))
    assert.ok(find(opened, withClass('dshgw-wsf-editor')) !== null, 'opening a hit loads the file, not just its folder')
  } finally {
    renderer.render(null)
    await host.dispose()
    client.globals.restore()
  }
})
