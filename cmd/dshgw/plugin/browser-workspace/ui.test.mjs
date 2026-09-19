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
// Depth-first search over the rendered tree: the dialog's error box is a node, not text.
function findNode(node, predicate) {
  if (node === null || node === undefined || typeof node !== 'object') return null
  if (Array.isArray(node)) {
    for (const child of node) { const hit = findNode(child, predicate); if (hit !== null) return hit }
    return null
  }
  if (predicate(node)) return node
  return findNode(node.children, predicate)
}
const classOf = node => (typeof node?.props?.className === 'string' ? node.props.className : '')
function setup({ activateFails = false, closeFailures = 0, createFailures = 0, pickerCancels = false, noPicker = false } = {}) {
  const events = [], requests = [], warnings = [], effects = [], styles = []
  // Both slots this plugin fills, by slot name: the sidebar row and the mount dialog are
  // separate registrations, so a mock that keeps only the last one cannot see the dialog.
  const registered = new Map()
  let client, confirmText, generation = 1, count = 0, closed = 0, cleanup
  // The gateway creates the mount point at `open` and removes it again on close.
  let mountpointPresent = false
  let readyResolve
  const rootReady = new Promise(resolve => { readyResolve = resolve })
  const root = { name: 'local', kind: 'directory', queryPermission: async () => 'granted' }
  const json = value => ({ ok: true, json: async () => ({ ok: true, value }) })
  const window = {
    isSecureContext: !noPicker,
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
  // The success dialog closes itself on a timer. That one timer is captured so the test can
  // fire it by hand, while the poll pacing and transport timeouts stay real timers — the
  // only delay the plugin uses that equals AUTO_CLOSE_MS is the auto-close itself.
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
    window, document, TextEncoder, Uint8Array, atob, btoa, AbortController,
    setTimeout: sandboxSetTimeout, clearTimeout: sandboxClearTimeout,
  })
  const ctx = {
    logger: { warn: message => warnings.push(message) },
    connection: {
      state: { getSnapshot: () => 'connected' },
      generation: { getSnapshot: () => ({ id: generation }) },
      reconnect() { events.push('reconnect'); generation++ },
    },
    slots: {
      inject(_name, fn) { return fn() },
      register(entry, component) { registered.set(entry.name, { options: entry, component }); return () => events.push('unregister') },
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
  const row = () => registered.get('sidebar.footer.action').component
  const dialog = () => registered.get('shell.overlay').component
  return {
    events, requests, warnings, styles, registered,
    inject: () => client.inject,
    click: () => row()().props.onClick(),
    element: (props) => row()(props),
    text: () => textOf(row()()),
    title: () => row()().props.title,
    state: () => row()().props['data-dshgw-state'],
    dialogElement: () => dialog()(),
    dialogText: () => textOf(dialog()()),
    options: () => registered.get('sidebar.footer.action').options,
    dialogOptions: () => registered.get('shell.overlay').options,
    confirm: () => confirmText,
    pendingAutoClose: () => autoTimers.size,
    fireAutoClose: () => { const pending = [...autoTimers.values()]; autoTimers.clear(); for (const fn of pending) fn() },
    dispose: async () => { cleanup(); await tick() },
  }
}
test('the plugin declares every service it reads, including the parent of remote.workspace', async () => {
  // A real DSH ctx is a Cordis proxy: reading a property that is not in `inject` throws
  // `cannot get property "<name>" without inject`. This mock hands the plugin a finished
  // `remote` object, so only an explicit check on the declared list can catch the
  // difference — the live GUI failed with exactly that error on the first click.
  const ui = setup()
  assert.ok(ui.inject().includes('remote'), 'ctx.remote is read, so "remote" must be injected')
  assert.ok(ui.inject().includes('remote.workspace'), 'the workspace namespace must be awaited too')
  assert.ok(ui.inject().includes('uiWorkspace'))
  await ui.dispose()
})
test('the sidebar entry is one ssh-style row stacked above the ssh workspace', async () => {
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
  // The row's own status is a machine-readable phase, so a real browser run can assert the
  // state without parsing the note (and the row carries no status dot).
  assert.equal(ui.state(), 'idle')
  assert.equal(ui.element().children.length, 3, 'the row is icon, label and optional note — no dot')
  // The warning that used to be a separate click now travels with the row.
  assert.match(ui.title(), /AI 模型服务商/)
  assert.match(ui.title(), /重启该账号的 worker/)
  assert.equal(ui.styles.length, 1, 'the stylesheet is installed once')
  assert.equal(ui.styles[0].dataset.pluginCss, 'dshgw-browser-workspace/client.css')
  // The shell renders `sidebar.footer.action` as one flex row, which would squeeze both
  // rows into half the foot; each plugin ships the rule that stacks the container instead.
  // The list slot wraps every registration in a classless div, so the shell's container is
  // two levels up from the row — the live DOM showed exactly that, and the direct shape is
  // covered as well in case the wrapper ever goes away.
  assert.match(ui.styles[0].textContent, /div:has\(> div > \.dshgw-bw-action\)[\s\S]*div:has\(> div > \.dshgw-ssh-action\)[\s\S]*flex-direction: column/)
  // The collapsed rail is one icon column: the dot stays, the text cannot fit.
  assert.equal(ui.element({ wide: false }).props.className, 'dshgw-bw-action dshgw-bw-action-rail')
  assert.equal(ui.element({ wide: true }).props.className, 'dshgw-bw-action')
  // The dialog is registered in the frame-wide overlay slot, closed until it is used.
  assert.equal(ui.dialogOptions().name, 'shell.overlay')
  assert.equal(ui.dialogOptions().id, 'browser-workspace-dialog')
  assert.ok(ui.dialogOptions().order > 200, 'the dialog must sit above the ssh-workspace one')
  assert.equal(ui.dialogElement(), null, 'the dialog renders nothing while closed')
  await ui.dispose()
})
test('one click runs picker/open/poll/activate/reconnect/register/connect, the next disconnects', async () => {
  const ui = setup()
  const pending = ui.click()
  // showDirectoryPicker must be invoked synchronously by the click: awaiting anything
  // first would let the browser's transient user activation expire (the reported
  // "Must be handling a user gesture" failure). Opening the dialog is a synchronous store
  // write, so it must not have pushed the picker out of the click's own task either.
  assert.equal(ui.events.at(-1), 'picker')
  // The dialog is up and says which step it is on while the picker is still open.
  assert.equal(ui.state(), 'picking')
  assert.match(ui.dialogText(), /等待选择目录/)
  await pending
  assert.equal(ui.events.includes('confirm'), false)
  assert.equal(ui.confirm(), undefined)
  assert.ok(ui.events.indexOf('picker') < ui.events.indexOf('open'))
  // The mount point exists from `open` until a teardown removes it, so the
  // workspace is registered before the activation restart, not after it.
  assert.ok(ui.events.indexOf('open') < ui.events.indexOf('create'))
  // The mount is visible inside the worker's sandbox, so the worker's own
  // workspace.create() realpath stats the mount point: polling has to be running
  // first or that stat blocks for the whole FUSE timeout.
  assert.ok(ui.events.indexOf('poll') < ui.events.indexOf('create'), 'poll must answer before the worker touches the mount')
  assert.ok(ui.events.indexOf('create') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('poll') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('create') < ui.events.indexOf('reconnect'))
  assert.ok(ui.events.indexOf('reconnect') < ui.events.indexOf('rename'))
  assert.ok(ui.events.indexOf('reconnect') < ui.events.indexOf('connect'))
  assert.match(ui.text(), /已挂载 local（读写）；点击断开/)
  assert.equal(ui.element().props['aria-pressed'], true)
  assert.match(ui.title(), /当前：已挂载/)
  // The row reports the mounted phase, and the dialog says so before it closes itself.
  assert.equal(ui.state(), 'mounted')
  assert.match(ui.dialogText(), /已挂载 local/)
  assert.match(ui.dialogText(), /不需要手动关闭/)
  assert.equal(ui.pendingAutoClose(), 1, 'success schedules exactly one self-close')
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.text(), /已断开/)
  assert.equal(ui.element().props['aria-pressed'], false)
  // A clean disconnect leaves the row idle again.
  assert.equal(ui.state(), 'idle')
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})
test('the success dialog closes itself and the row keeps reporting the mount', async () => {
  const ui = setup()
  await ui.click()
  assert.notEqual(ui.dialogElement(), null, 'the dialog is open when the mount succeeds')
  ui.fireAutoClose()
  assert.equal(ui.dialogElement(), null, 'no click closed it: the success path closed it itself')
  // Closing the dialog must not touch what the row reports.
  assert.equal(ui.state(), 'mounted')
  assert.match(ui.text(), /已挂载 local（读写）；点击断开/)
  assert.equal(ui.element().props['aria-pressed'], true)
  // A second attempt must not inherit the previous attempt's timer.
  assert.equal(ui.pendingAutoClose(), 0)
  await ui.dispose()
})
test('a cancelled picker closes the dialog and leaves the row exactly as it was', async () => {
  const ui = setup({ pickerCancels: true })
  const before = ui.text(), beforeState = ui.state()
  await ui.click()
  assert.equal(ui.events.includes('open'), false, 'a cancelled picker opens no capability')
  assert.equal(ui.dialogElement(), null, 'a cancelled picker is not a failure to report')
  assert.equal(ui.pendingAutoClose(), 0, 'a cancelled picker schedules nothing')
  assert.equal(ui.text(), before)
  assert.equal(ui.state(), beforeState)
  await ui.dispose()
})
test('idempotent workspace creation retries a transient registration failure', async () => {
  const ui = setup({ createFailures: 1 })
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'create').length, 2)
  assert.equal(ui.events.filter(e => e === 'connect').length, 1)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})
test('activation failure releases the capability, forgets the workspace and keeps the failure visible', async () => {
  const ui = setup({ activateFails: true })
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'create').length, 1)
  assert.equal(ui.events.includes('connect'), false)
  assert.equal(ui.events.filter(e => e === 'delete').length, 1)
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.text(), /挂载失败：restart failed/)
  // A failure is the one ending a person has to read, so the dialog stays open in red and
  // nothing closes it on its own.
  assert.equal(ui.state(), 'failed')
  assert.equal(ui.pendingAutoClose(), 0, 'failure never schedules a self-close')
  const dialog = ui.dialogElement()
  assert.notEqual(dialog, null, 'the failure dialog stays open')
  assert.match(ui.dialogText(), /挂载失败：restart failed/)
  assert.equal(classOf(findNode(dialog, node => classOf(node) === 'dshgw-bw-error')), 'dshgw-bw-error')
  await ui.dispose()
})
test('failed stop retains token and offers cleanup retry without false success', async () => {
  const ui = setup({ closeFailures: 1 })
  await ui.click()
  await ui.click()
  assert.match(ui.text(), /清理未确认.*点击重试断开/)
  assert.doesNotMatch(ui.text(), /已断开/)
  assert.equal(ui.state(), 'failed')
  assert.equal(ui.warnings.length, 1)
  await ui.click()
  assert.match(ui.text(), /已断开/)
  assert.equal(ui.state(), 'idle')
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
  assert.equal(ui.state(), 'failed')
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
  // Disposal drops the pending self-close instead of firing it into a dead plugin.
  assert.equal(ui.pendingAutoClose(), 0)
  await ui.click()
  assert.equal(ui.events.filter(e => e === 'open').length, 1)
})
test('a browser without the File System Access API fails on the row, without a dialog', async () => {
  // The guard runs before anything else: no picker means the feature cannot start, which is
  // a failure the row must show — but there is no mount to narrate, so no dialog either.
  const ui = setup({ noPicker: true })
  await ui.click()
  assert.equal(ui.state(), 'failed')
  assert.match(ui.text(), /需要 HTTPS 和支持目录访问的浏览器/)
  assert.equal(ui.dialogElement(), null)
  assert.equal(ui.events.includes('open'), false)
  await ui.dispose()
})
