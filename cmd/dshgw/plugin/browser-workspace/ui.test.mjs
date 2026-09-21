// The sidebar row and the window it drives, for the case this feature was built for first:
// ONE local directory. What is asserted here is the contract a real browser run depends on —
// one click is the whole gesture that reaches the picker, the mount is registered before the
// worker restarts, the success window closes itself, a failure stays readable — plus the
// exact shape the real-browser probes key on (`data-dshgw-state`, the two controls in the row,
// the stacking rule that keeps this row above the ssh one).
import test from 'node:test'
import assert from 'node:assert/strict'
import { setup, until, fakeHandle, classOf, findNode, textOf } from './harness.mjs'

// A row click starts work that outlives the click (the picker is opened synchronously, the
// mount is not), so tests wait for the state they are about instead of for a returned promise.
const mounted = ui => until(() => ui.rowState() === 'mounted', 'the mount')
const closed = ui => until(() => ui.events.includes('close'), 'the close handshake')

test('the plugin declares every service it reads, including the parent of remote.workspace', async () => {
  // A real DSH ctx is a Cordis proxy: reading a property that is not in `inject` throws
  // `cannot get property "<name>" without inject`. This mock hands the plugin a finished
  // `remote` object, so only an explicit check on the declared list can catch the difference.
  const ui = setup()
  assert.ok(ui.inject().includes('remote'), 'ctx.remote is read, so "remote" must be injected')
  assert.ok(ui.inject().includes('remote.workspace'), 'the workspace namespace must be awaited too')
  assert.ok(ui.inject().includes('uiWorkspace'))
  await ui.dispose()
})

test('the sidebar entry is one row with a folder icon, stacked above the ssh workspace', async () => {
  const ui = setup()
  assert.equal(ui.options().name, 'sidebar.footer.action')
  assert.equal(ui.options().id, 'browser-workspace')
  assert.equal(ui.options().label, '浏览器工作区')
  // The ssh-workspace entry registers the same slot with order 100; ascending order puts
  // this row above it.
  assert.ok(ui.options().order < 100, `order ${ui.options().order} must precede the ssh entry`)
  const row = ui.row()
  assert.equal(row.type, 'div')
  assert.equal(classOf(row), 'dshgw-bw-row')
  // Two controls: the row body (the obvious single action) and the folder list at its right.
  assert.equal(row.children.length, 2, 'the row is the body plus the folder icon')
  const body = ui.body()
  assert.equal(body.type, 'button')
  assert.equal(classOf(body), 'dshgw-bw-action')
  assert.equal(body.props['aria-pressed'], false)
  assert.equal(textOf(body), '🖥浏览器工作区')
  const manage = ui.manage()
  assert.equal(manage.type, 'button')
  assert.equal(classOf(manage), 'dshgw-bw-manage')
  assert.equal(manage.props['data-dshgw-manage'], 'folders')
  assert.equal(manage.props['aria-label'], '管理文件夹')
  assert.equal(textOf(manage), '🗂')
  // The row's own status is a machine-readable phase on BOTH the container and the body, so a
  // real browser run can assert the state without parsing the note (and there is no dot).
  assert.equal(ui.rowState(), 'idle')
  assert.equal(row.props['data-dshgw-state'], 'idle')
  // The warning that used to be a separate click now travels with the row.
  assert.match(ui.rowTitle(), /AI 模型服务商/)
  assert.match(ui.rowTitle(), /重启该账号的 worker/)
  assert.equal(ui.registered.size, 2, 'the row and the window are two registrations')
  // The shell renders `sidebar.footer.action` as one flex row, which would squeeze both rows
  // into half the foot; each plugin ships the rule that stacks the container instead. The
  // browser row is a container (the list slot wraps every registration in a classless div, so
  // the shell's container is two levels up from the row).
  assert.equal(ui.styles().length, 1, 'the stylesheet is installed once')
  assert.equal(ui.styles()[0].dataset.pluginCss, 'dshgw-browser-workspace/client.css')
  const css = ui.styles()[0].textContent
  assert.match(css, /div:has\(> div > \.dshgw-bw-row\)[\s\S]*div:has\(> div > \.dshgw-ssh-action\)[\s\S]*flex-direction: column/)
  // The rule must match the row container from OUTSIDE. Matching the inner action button too
  // made the container itself a column, which put the folder icon under the label instead of
  // beside it (a real browser caught it); the shape is pinned here so it cannot come back.
  assert.doesNotMatch(css, /> \.dshgw-bw-action/)
  // The collapsed rail is one icon column: the text cannot fit there, and neither can the
  // folder icon (which needs a label to make sense).
  const rail = ui.row({ wide: false })
  assert.equal(classOf(rail), 'dshgw-bw-row dshgw-bw-row-rail')
  assert.equal(classOf(rail.children[0]), 'dshgw-bw-action dshgw-bw-action-rail')
  assert.equal(classOf(ui.row({ wide: true })), 'dshgw-bw-row')
  // The window is registered in the frame-wide overlay slot, closed until it is used.
  assert.equal(ui.dialogOptions().name, 'shell.overlay')
  assert.equal(ui.dialogOptions().id, 'browser-workspace-dialog')
  assert.ok(ui.dialogOptions().order > 200, 'the window must sit above the ssh-workspace one')
  assert.equal(ui.dialogOpen(), false, 'the window renders nothing while closed')
  await ui.dispose()
})

test('one click runs picker/open/poll/activate/reconnect/register/connect, the next disconnects', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })] })
  ui.clickRow()
  // showDirectoryPicker must be invoked synchronously by the click: awaiting anything first
  // would let the browser's transient user activation expire (the reported "Must be handling
  // a user gesture" failure). Opening the window is a synchronous store write, so it must not
  // have pushed the picker out of the click's own task either.
  assert.equal(ui.events.at(-1), 'picker')
  // The window is up and says which step it is on while the picker is still open.
  assert.equal(ui.rowState(), 'picking')
  assert.match(ui.dialogText(), /等待选择目录/)
  await mounted(ui)
  assert.equal(ui.events.includes('confirm'), false)
  assert.equal(ui.confirm(), undefined)
  assert.ok(ui.events.indexOf('picker') < ui.events.indexOf('open'))
  // The mount point exists from `open` until a teardown removes it, so the workspace is
  // registered before the activation restart, not after it.
  assert.ok(ui.events.indexOf('open') < ui.events.indexOf('create'))
  // The mount is visible inside the worker's sandbox, so the worker's own workspace.create()
  // realpath stats the mount point: polling has to be running first or that stat blocks for
  // the whole FUSE timeout.
  assert.ok(ui.events.indexOf('poll') < ui.events.indexOf('create'), 'poll must answer before the worker touches the mount')
  assert.ok(ui.events.indexOf('create') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('poll') < ui.events.indexOf('activate'))
  assert.ok(ui.events.indexOf('create') < ui.events.indexOf('reconnect'))
  assert.ok(ui.events.indexOf('reconnect') < ui.events.findIndex(e => e.startsWith('connect:')))
  // The open handshake carried the folder's stable key: the mount point is a mapping of the
  // LOCAL directory, not of one mount — and the key it carries is that directory's own name,
  // so the path reads as the directory the person picked.
  const opened = ui.requests.find(request => request.endpoint === 'open')
  assert.equal(opened.payload.key, 'local')
  assert.equal(opened.payload.name, 'local')
  assert.equal(opened.payload.writable, true)
  // The name was arbitrated with the gateway BEFORE the mount, exactly once: that handshake is
  // what keeps two local directories of one account from silently becoming one path.
  assert.deepEqual(ui.requests.filter(request => request.endpoint === 'allocate').map(request => request.payload.name), ['local'])
  assert.ok(ui.events.indexOf('allocate') < ui.events.indexOf('open'))
  assert.match(ui.rowText(), /已挂载 local（读写）；点击断开/)
  assert.equal(ui.body().props['aria-pressed'], true)
  assert.match(ui.rowTitle(), /当前：已挂载/)
  // The row reports the mounted phase, and the window says so before it closes itself.
  assert.equal(ui.rowState(), 'mounted')
  assert.match(ui.dialogText(), /已挂载 local/)
  // The workspace was renamed to the local directory's name and opened.
  assert.ok(ui.events.includes('rename:本地: local'))
  assert.equal(ui.pendingAutoClose(), 1, 'success schedules exactly one self-close')
  ui.clickRow()
  await until(() => ui.events.includes('close'), 'the disconnect')
  await until(() => ui.rowState() === 'disconnected', 'the disconnected phase')
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.rowText(), /已断开/)
  assert.equal(ui.body().props['aria-pressed'], false)
  // Disconnecting keeps the folder saved, so the row goes to its disconnected phase rather
  // than claiming a mount that no longer exists.
  assert.equal(ui.rowState(), 'disconnected')
  assert.equal(ui.savedFolders().length, 1, 'the disconnected folder is still saved')
  await until(() => ui.savedFolders()[0]?.token === null, 'the dead capability to be dropped')
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})

test('the success window closes itself and the row keeps reporting the mount', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })] })
  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.dialogOpen(), true, 'the window is open when the mount succeeds')
  ui.fireAutoClose()
  assert.equal(ui.dialogOpen(), false, 'no click closed it: the success path closed it itself')
  // Closing the window must not touch what the row reports.
  assert.equal(ui.rowState(), 'mounted')
  assert.match(ui.rowText(), /已挂载 local（读写）；点击断开/)
  assert.equal(ui.body().props['aria-pressed'], true)
  // A second attempt must not inherit the previous attempt's timer.
  assert.equal(ui.pendingAutoClose(), 0)
  await ui.dispose()
})

test('a cancelled picker closes the window and leaves the row exactly as it was', async () => {
  const ui = setup({ pickerDirs: [] })
  const before = ui.rowText(), beforeState = ui.rowState()
  ui.clickRow()
  await until(() => ui.rowState() === beforeState, 'the cancelled picker to leave the row alone')
  assert.equal(ui.events.includes('open'), false, 'a cancelled picker opens no capability')
  assert.equal(ui.dialogOpen(), false, 'a cancelled picker is not a failure to report')
  assert.equal(ui.pendingAutoClose(), 0, 'a cancelled picker schedules nothing')
  assert.equal(ui.rowText(), before)
  assert.equal(ui.rowState(), beforeState)
  assert.deepEqual(ui.savedFolders(), [], 'a cancelled picker saves no folder')
  await ui.dispose()
})

test('idempotent workspace creation retries a transient registration failure', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })], createFailures: 1 })
  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.events.filter(e => e === 'create').length, 2)
  assert.equal(ui.events.filter(e => e.startsWith('connect:')).length, 1)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
})

test('activation failure releases the capability and keeps the failure visible', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })], activateFails: true })
  ui.clickRow()
  await until(() => ui.rowState() === 'failed', 'the failed mount')
  assert.equal(ui.events.filter(e => e === 'create').length, 1)
  assert.equal(ui.events.some(e => e.startsWith('connect:')), false)
  // The mount failed, so the workspace THIS mount created is forgotten again — and the
  // capability is released instead of being left mounted behind a page that gave up.
  assert.equal(ui.events.filter(e => e.startsWith('delete:')).length, 1)
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.match(ui.rowText(), /挂载失败：restart failed/)
  assert.equal(ui.rowState(), 'failed')
  assert.equal(ui.pendingAutoClose(), 0, 'failure never schedules a self-close')
  const dialog = ui.dialogElement()
  assert.notEqual(dialog, null, 'the failure window stays open')
  assert.match(ui.dialogText(), /挂载失败：restart failed/)
  assert.notEqual(findNode(dialog, node => classOf(node) === 'dshgw-bw-error'), null)
  // The saved folder survives the failure: retrying must not need the directory picker again.
  assert.equal(ui.savedFolders().length, 1)
  assert.equal(ui.savedFolders()[0].handle.name, 'local')
  await ui.dispose()
})

test('a workspace this mount did NOT create is never deleted by a failed mount', async () => {
  // The stable key maps a local directory to a workspace that already exists — with sessions
  // in it. A failed mount must not throw that away, so only a workspace the mount itself
  // created is forgotten.
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })], activateFails: true, created: false })
  ui.clickRow()
  await until(() => ui.rowState() === 'failed', 'the failed mount')
  assert.equal(ui.events.filter(e => e.startsWith('delete:')).length, 0, 'an existing workspace was deleted')
  await ui.dispose()
})

test('failed disconnect retains the token and offers a cleanup retry without false success', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })], closeFailures: 1 })
  ui.clickRow()
  await mounted(ui)
  ui.clickRow()
  await until(() => ui.rowState() === 'failed', 'the unconfirmed cleanup')
  assert.match(ui.rowText(), /清理未确认.*点击重试断开/)
  assert.doesNotMatch(ui.rowText(), /已断开/)
  assert.equal(ui.rowState(), 'failed')
  assert.equal(ui.warnings.length, 1)
  ui.clickRow()
  await until(() => ui.rowState() === 'disconnected', 'the retried disconnect')
  assert.match(ui.rowText(), /已断开/)
  const closes = ui.requests.filter(r => r.endpoint === 'close')
  assert.equal(closes.length, 2)
  assert.equal(closes[0].payload.token, closes[1].payload.token)
  assert.equal(ui.events.filter(e => e === 'open').length, 1)
  await ui.dispose()
})

test('activation and cleanup failure preserves the retry state', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })], activateFails: true, closeFailures: 1 })
  ui.clickRow()
  await until(() => ui.rowState() === 'failed', 'the unconfirmed cleanup')
  assert.match(ui.rowText(), /清理未确认/)
  ui.clickRow()
  await until(() => ui.rowState() === 'disconnected', 'the retried disconnect')
  assert.match(ui.rowText(), /已断开/)
  assert.equal(ui.events.some(e => e.startsWith('connect:')), false)
  await ui.dispose()
})

test('dispose aborts poll and sends close; failed cleanup is reported honestly', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })], closeFailures: 1 })
  ui.clickRow()
  await mounted(ui)
  await ui.dispose()
  assert.equal(ui.events.filter(e => e === 'close').length, 1)
  assert.equal(ui.warnings.length, 1)
  // Disposal drops the pending self-close instead of firing it into a dead plugin.
  assert.equal(ui.pendingAutoClose(), 0)
})

test('a browser without the File System Access API fails on the row, and never mounts', async () => {
  // The guard runs before anything else: no picker means the feature cannot start, which is a
  // failure the row must show — but there is no mount to narrate, so nothing is served.
  const ui = setup({ noPicker: true })
  ui.clickRow()
  await until(() => ui.rowState() === 'failed', 'the unsupported browser to be reported')
  assert.equal(ui.rowState(), 'failed')
  assert.match(ui.dialogText(), /需要 HTTPS 和支持目录访问的浏览器/)
  assert.equal(ui.events.includes('open'), false)
  assert.deepEqual(ui.savedFolders(), [])
  await ui.dispose()
})
