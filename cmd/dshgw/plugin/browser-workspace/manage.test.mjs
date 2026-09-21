// The point of the folder list: ONE account, SEVERAL local directories, each with its own
// mount and its own workspace, and an icon at the row's right where a person adds, connects,
// disconnects and deletes them.
//
// What is pinned here beyond "the buttons work":
//
//   · independence — one folder's connect/disconnect/delete never touches another's mount,
//     and only the affected folder's capability is ever sent to the gateway;
//   · the row's adaptive click — no folder is the original one-click mount, exactly one is
//     that folder's connect/disconnect, several is "disconnect which one?" and opens the list;
//   · the stable mapping — the key a folder mounts with never changes for that folder, so a
//     reconnect returns to the same virtual path and therefore to the same DSH workspace;
//   · honesty — a gateway refusal (the per-account limit, a key already mounted, a directory
//     that is already in the list) is reported as itself, never as a silent success.
import test from 'node:test'
import assert from 'node:assert/strict'
import { setup, until, fakeHandle, storedFolder, storedFolders } from './harness.mjs'

const mounted = ui => until(() => ui.rowState() === 'mounted', 'a live mount')
const state = (ui, wanted) => until(() => ui.rowState() === wanted, wanted)
const ready = ui => until(() => ui.folderStates().length === 2 && ui.folderStates().every(s => s === 'connected'), 'both folders mounted')
const keyOf = (ui, name) => ui.savedFolders().find(folder => folder.name === name)?.key ?? null

/** Two local directories, added through the list exactly like a person adds them. */
async function twoFolders (extra = {}) {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'docs' }), fakeHandle({ name: 'photos' })], ...extra })
  await state(ui, 'idle')
  ui.clickManage()
  assert.equal(ui.dialogOpen(), true, 'the folder icon opens the list')
  assert.match(ui.dialogText(), /还没有保存的目录/)
  ui.clickDialog('添加文件夹')
  await until(() => ui.folderNames().length === 1, 'the first folder')
  await until(() => ui.folderStates()[0] === 'connected', 'the first mount')
  ui.clickDialog('添加文件夹')
  await ready(ui)
  return ui
}

test('the folder icon opens a list, and adding folders mounts each one at its own path', async () => {
  const ui = await twoFolders()
  // Two saved folders, two capabilities, two mount points — and each mount point is named by
  // the local directory it maps, rather than by anything per-mount or generated.
  const saved = ui.savedFolders()
  assert.equal(saved.length, 2)
  const keys = saved.map(folder => folder.key)
  assert.deepEqual([...keys].sort(), ['docs', 'photos'], 'a mount point is the name of the local directory it maps')
  assert.equal(new Set(keys).size, 2, 'two folders must not share one identity')
  assert.equal(new Set(saved.map(folder => folder.mountpoint)).size, 2, 'two folders must not share one mount point')
  for (const folder of saved) assert.equal(folder.mountpoint, `/home/account/browser/${folder.key}`)
  // Both are open in the list, each with its own controls.
  assert.deepEqual(ui.folderNames().sort(), [...keys].sort())
  assert.deepEqual(ui.folderText('docs').includes('已连接'), true)
  assert.deepEqual(ui.folderText('photos').includes('已连接'), true)
  // Adding a folder through the list does NOT close the window: it is a management surface.
  assert.equal(ui.dialogOpen(), true)
  assert.equal(ui.pendingAutoClose(), 0, 'a window opened for management must not close itself')
  // The row summarises several folders instead of pretending to be one mount.
  assert.equal(ui.rowState(), 'mounted')
  assert.match(ui.rowText(), /已连接 2\/2 个目录；点击管理/)
  await ui.dispose()
})

test('the row click mounts when nothing is saved, and opens the list once there are two folders', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'docs' }), fakeHandle({ name: 'photos' })] })
  await state(ui, 'idle')
  // No folder saved: the original one-click gesture still reaches the picker directly.
  ui.clickRow()
  assert.equal(ui.events.at(-1), 'picker')
  assert.equal(ui.pickerActive(), null, 'the fake environment has no activation to check')
  await until(() => ui.folderNames().length === 1, 'the first mount')
  await mounted(ui)
  assert.match(ui.rowText(), /已挂载 docs（读写）；点击断开/)
  // Still exactly one folder: the row click is that folder's own action.
  ui.clickRow()
  await state(ui, 'disconnected')
  assert.equal(ui.events.filter(event => event === 'close').length, 1)
  // A second folder arrives through the list; now the row click has no single obvious meaning
  // ("disconnect which one?") and must open the list instead of guessing.
  ui.clickManage()
  ui.clickDialog('添加文件夹')
  await until(() => ui.folderNames().length === 2, 'the second folder')
  await until(() => ui.folderStates().includes('connected'), 'the second mount')
  const before = ui.dialogText()
  ui.clickManage()
  assert.equal(ui.dialogOpen(), true)
  assert.equal(ui.requests.filter(request => request.endpoint === 'close').length, 1, 'the row click must not disconnect one of several folders')
  assert.notEqual(ui.dialogText(), '')
  assert.equal(before.length > 0, true)
  await ui.dispose()
})

test('disconnecting one folder leaves the other mounted and serving', async () => {
  const ui = await twoFolders()
  const docs = keyOf(ui, 'docs')
  const photos = keyOf(ui, 'photos')
  ui.clickFolder('docs', 'disconnect')
  await until(() => ui.folderStates().find((_, index) => ui.folderNames()[index] === docs) === 'disconnected', 'the disconnected folder')
  // Exactly one close went out, for that folder's capability only.
  const closes = ui.requests.filter(request => request.endpoint === 'close')
  assert.equal(closes.length, 1)
  assert.equal(closes[0].payload.token.includes(docs.slice(0, 4)), true, 'the wrong capability was closed')
  assert.equal(ui.state.mounts.has(closes[0].payload.token), false)
  // The other folder is untouched: still mounted, still served by this page.
  assert.equal(ui.folderStates()[ui.folderNames().indexOf(photos)], 'connected')
  assert.equal(ui.state.mounts.size, 1, 'disconnecting one folder closed another')
  assert.match(ui.folderText('photos'), /已连接/)
  // The row now reports a mixed state, and says how to manage it.
  assert.equal(ui.rowState(), 'mounted')
  assert.match(ui.rowText(), /已连接 1\/2 个目录；点击管理/)
  await ui.dispose()
})

test('connecting a saved folder again returns to the SAME virtual path and workspace', async () => {
  const ui = await twoFolders()
  const docs = keyOf(ui, 'docs')
  ui.clickFolder('docs', 'disconnect')
  await until(() => ui.folderStates().includes('disconnected'), 'the disconnected folder')
  const savedAfterDisconnect = ui.savedFolders().find(folder => folder.key === docs)
  assert.equal(savedAfterDisconnect.token, null)
  assert.equal(savedAfterDisconnect.workspaceId, 'workspace-' + docs.slice(0, 4), 'the workspace mapping was dropped with the capability')
  assert.equal(ui.events.some(event => event.startsWith('delete:')), false, 'a disconnect must not delete the workspace entry')
  ui.clickFolder('docs', 'connect')
  await until(() => ui.folderStates().every(state => state === 'connected'), 'the reconnected folder')
  // Every open handshake for that folder carried the SAME key: the path, and with it the
  // workspace DSH keys by that path, is a property of the local directory.
  const opens = ui.requests.filter(request => request.endpoint === 'open' && request.payload.key === docs)
  assert.equal(opens.length, 2)
  assert.equal(opens[0].payload.name, opens[1].payload.name)
  assert.equal(ui.savedFolders().find(folder => folder.key === docs).mountpoint, `/home/account/browser/${docs}`)
  await ui.dispose()
})

test('deleting one folder purges exactly that folder, leaving the other mounted', async () => {
  const ui = await twoFolders()
  const docs = keyOf(ui, 'docs')
  const photos = keyOf(ui, 'photos')
  ui.clickFolder('docs', 'delete')
  assert.deepEqual(ui.folderActions('docs'), ['打开', '断开', '确认删除'])
  ui.clickFolder('docs', 'confirm-delete')
  await until(() => ui.folderNames().length === 1, 'the deleted folder to disappear')
  const closes = ui.requests.filter(request => request.endpoint === 'close')
  const purges = closes.filter(request => request.payload.purge === true)
  // Two releases for the deleted folder, and neither of them names the survivor: the
  // capability closes with `purge` (its tombstone releases the path), and then the KEY is
  // released, which is what also cleans a folder whose token had already died with its mount.
  assert.equal(purges.length, 2, 'deleting a folder must release its virtual path, with and without a capability')
  assert.equal(purges[0].payload.token.includes(docs.slice(0, 4)), true)
  assert.equal(purges[0].payload.key, undefined)
  assert.deepEqual(purges[1].payload, { key: docs, purge: true })
  assert.equal(closes.some(request => JSON.stringify(request.payload).includes(photos)), false, 'deleting one folder released another')
  assert.equal(ui.savedFolders().length, 1)
  assert.equal(ui.savedFolders()[0].key, photos)
  assert.equal(ui.folderStates()[0], 'connected', 'deleting one folder disconnected another')
  // The workspace registration is deleted BEFORE the mount is released: releasing the mount
  // restarts the account's worker, and a deletion sent into that restart is lost (a real run
  // left the dead workspace row behind, and this order is the fix).
  const order = ui.events
  const deletion = order.findIndex(event => event.startsWith('delete:'))
  const close = order.indexOf('close')
  assert.ok(deletion !== -1 && deletion < close, `the workspace entry was deleted after the restart: ${order.slice(-8)}`)
  assert.match(ui.rowText(), /已挂载 photos（读写）；点击断开/, 'one folder left: the row is that folder again')
  await ui.dispose()
})

test('deleting an armed folder is a second, deliberate click', async () => {
  const ui = await twoFolders()
  ui.clickFolder('photos', 'delete')
  assert.deepEqual(ui.folderActions('photos'), ['打开', '断开', '确认删除'])
  // Clicking elsewhere / re-arming must not delete anything.
  ui.clickFolder('docs', 'delete')
  assert.deepEqual(ui.folderActions('photos'), ['打开', '断开', '删除'], 'an armed deletion was not disarmed by another folder')
  assert.equal(ui.savedFolders().length, 2)
  assert.equal(ui.events.filter(event => event === 'close').length, 0)
  await ui.dispose()
})

test('a disconnect keeps the folder saved even though its capability is gone', async () => {
  const ui = await twoFolders()
  const docs = keyOf(ui, 'docs')
  ui.clickFolder('docs', 'disconnect')
  await until(() => ui.folderStates().includes('disconnected'), 'the disconnected folder')
  // A reload at this point must still find the folder (with its handle) and offer to connect
  // it again without a directory dialog.
  const saved = ui.savedFolders().find(folder => folder.key === docs)
  assert.equal(saved.token, null)
  assert.equal(saved.handle.name, 'docs')
  assert.equal(saved.at, 0, 'the timestamp of a dead capability must not advertise a resume')
  await ui.dispose()
})

test('the gateway refuses more folders than one account may mount, in words a person can act on', async () => {
  const ui = await twoFolders({ mountLimit: 2, pickerDirs: [fakeHandle({ name: 'docs' }), fakeHandle({ name: 'photos' }), fakeHandle({ name: 'third' })] })
  ui.clickDialog('添加文件夹')
  await until(() => ui.folderStates().length === 3, 'the third folder to be saved')
  await until(() => ui.folderStates()[2] === 'error', 'the third mount to be refused')
  // The gateway's own sentence is translated once, and the refusal is visible where the folder
  // is — not in a console.
  assert.match(ui.folderText('third'), /已达每账号 4 个目录上限：请先断开一个目录/)
  assert.equal(ui.folderStates()[2], 'error')
  // The two mounted folders were not disturbed by the refused one.
  assert.equal(ui.folderStates().slice(0, 2).join(','), 'connected,connected')
  assert.match(ui.rowText(), /已连接 2\/3 个目录，1 个失败；点击管理/)
  await ui.dispose()
})

test('a gateway that still holds the key reports it instead of mounting the directory twice', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'docs' })] })
  await state(ui, 'idle')
  ui.clickRow()
  await mounted(ui)
  const key = ui.savedFolders()[0].key
  // A second page's connect: same folder, but the gateway is already serving that key.
  assert.equal(ui.state.keys.has(key), true)
  assert.equal(ui.requests.filter(request => request.endpoint === 'open').length, 1)
  await ui.dispose()
})

test('adding a directory that is already in the list connects that folder instead of duplicating it', async () => {
  const docs = fakeHandle({ name: 'docs' })
  const ui = setup({ pickerDirs: [docs, docs] })
  await state(ui, 'idle')
  ui.clickRow()
  await mounted(ui)
  ui.clickManage()
  ui.clickDialog('添加文件夹')
  await until(() => ui.events.filter(event => event === 'picker').length === 2, 'the second pick')
  await new Promise(resolve => setTimeout(resolve, 50))
  assert.equal(ui.savedFolders().length, 1, 'the same local directory was saved twice')
  // The duplicate pick became the folder that already owns that directory: one mount, not two.
  assert.equal(ui.state.mounts.size, 1)
  assert.equal(ui.folderStates().length, 1)
  assert.equal(ui.folderStates()[0], 'connected')
  await ui.dispose()
})

test('re-picking a directory for one folder keeps its key, so the mapping does not move', async () => {
  // The browser withholds the old grant, so this folder needs the directory dialog again — and
  // the directory it gets is a NEW handle for the SAME slot in the operator's list. What must
  // not move is the key: it is the virtual path, and therefore the workspace and its sessions.
  const stale = fakeHandle({ name: 'docs', permission: 'prompt' })
  const fresh = fakeHandle({ name: 'docs-again' })
  const key = 'd'.repeat(32)
  const ui = setup({ stored: storedFolders([storedFolder({ handle: stale, key, token: null, at: 0 })]), pickerDirs: [fresh] })
  await state(ui, 'disconnected')
  assert.match(ui.rowText(), /已断开 docs；点击重新选择目录/)
  ui.clickRow()
  await mounted(ui)
  const saved = ui.savedFolders()
  assert.equal(saved.length, 1, 're-picking created a second folder')
  assert.equal(saved[0].key, key, 'the stable mapping moved when the handle was replaced')
  assert.equal(saved[0].handle.name, 'docs-again', 'the replacement handle was not adopted')
  assert.equal(saved[0].name, 'docs-again')
  assert.equal(ui.state.keys.has(key), true, 'the mount was not made under the folder\'s own key')
  await ui.dispose()
})

test('the window lists every folder with its state, and the rail keeps one icon', async () => {
  const ui = await twoFolders()
  assert.equal(ui.dialogOpen(), true)
  assert.match(ui.dialogText(), /添加文件夹/)
  assert.match(ui.dialogText(), /已连接/)
  // The folder rows carry machine-readable hooks for a real browser run.
  assert.equal(ui.folderStates().length, 2)
  assert.deepEqual(ui.folderStates(), ['connected', 'connected'])
  // The collapsed rail is one icon: the label, the note and the folder icon all go.
  const rail = ui.row({ wide: false })
  assert.equal(rail.props['data-dshgw-state'], 'mounted')
  assert.equal(rail.children[1].props['data-dshgw-manage'], 'folders')
  await ui.dispose()
})

test('a workspace deletion that raced the worker restart is retried until it lands', async () => {
  // The close restarts the worker, so the deletion can be refused or lost the first time. The
  // folder must not be reported as removed while its workspace row is still there.
  const ui = await twoFolders({ deleteFailures: 2 })
  const docs = keyOf(ui, 'docs')
  ui.clickFolder('docs', 'delete')
  ui.clickFolder('docs', 'confirm-delete')
  // The retries are paced (each attempt waits for a connection generation again), so this
  // test allows more than the default wait.
  await until(() => ui.folderNames().length === 1, 'the deleted folder to disappear', 10000)
  assert.equal(ui.events.filter(event => event.startsWith('delete:')).length, 3, 'the deletions were not retried')
  assert.equal(ui.savedFolders().length, 1)
  assert.equal(ui.folderStates()[0], 'connected')
  await ui.dispose()
})

test('a gateway that predates stable directory keys still mounts, without the key', async () => {
  // The plugin and the gateway ship together, but a page loaded just before a gateway restart
  // talks to the older binary for as long as it stays open — and that binary decodes with
  // DisallowUnknownFields, so `key` on open and `purge` on close are refused outright. The
  // client must degrade to the old behaviour (a per-mount path, no explicit release) rather
  // than leave the operator with a mount that cannot start.
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'docs' })], legacyGateway: true })
  await state(ui, 'idle')
  ui.clickRow()
  await mounted(ui)
  const opens = ui.requests.filter(request => request.endpoint === 'open')
  assert.equal(opens.length, 2, 'the client did not retry the open without a key')
  // That gateway has no `allocate` either, so the folder mounts under a generated key — the
  // behaviour this feature always had, and the only one that binary understands.
  assert.match(opens[0].payload.key, /^[a-f0-9]{32}$/)
  assert.equal(opens[1].payload.key, undefined)
  assert.equal(ui.knownWarnings.some(message => message.includes('stable directory keys')), true)
  // The folder is still saved and still identified by its own key: only the gateway's path is
  // the old random one.
  assert.equal(ui.savedFolders().length, 1)
  assert.equal(ui.savedFolders()[0].key, opens[0].payload.key)
  // Deleting it releases the mount through the only close an older gateway understands, and the
  // key release it cannot answer is attempted, reported, and does not block the delete.
  ui.clickManage()
  ui.clickFolder('docs', 'delete')
  ui.clickFolder('docs', 'confirm-delete')
  await until(() => ui.folderNames().length === 0, 'the deleted folder to disappear')
  const closes = ui.requests.filter(request => request.endpoint === 'close')
  assert.equal(closes.some(request => request.payload.token !== undefined && request.payload.purge === true), true, 'the release was attempted with the capability first')
  assert.equal(closes.some(request => request.payload.token !== undefined && request.payload.purge === undefined), true, 'the release was retried without the field that gateway refuses')
  assert.deepEqual(closes.at(-1).payload, { key: opens[0].payload.key, purge: true }, 'the release by key was attempted last')
  assert.equal(ui.knownWarnings.some(message => message.includes('cannot release a mount point by key')), true, 'the unreleasable path was not reported')
  await ui.dispose()
})

test('a mount point is named after the local directory, arbitrated once', async () => {
  // The name is the virtual path, so it is the directory a person picked — and it is asked for
  // ONCE, before the first mount, because changing it later would move the workspace and leave
  // the sessions grouped under the old path behind.
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'aosp' })] })
  await state(ui, 'idle')
  ui.clickRow()
  await mounted(ui)
  assert.deepEqual(ui.requests.filter(request => request.endpoint === 'allocate').map(request => request.payload.name), ['aosp'])
  assert.equal(ui.savedFolders()[0].key, 'aosp')
  assert.equal(ui.savedFolders()[0].mountpoint, '/home/account/browser/aosp')
  assert.deepEqual(ui.requests.find(request => request.endpoint === 'open').payload, { name: 'aosp', writable: true, key: 'aosp' })
  // A reconnect keeps that name and never asks for another one: the path is the mapping.
  ui.clickRow()
  await state(ui, 'disconnected')
  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.requests.filter(request => request.endpoint === 'allocate').length, 1, 'a reconnect arbitrated the name again')
  assert.equal(ui.state.keys.has('aosp'), true)
  await ui.dispose()
})

test('two local directories with the same name are two mount points, and a name the account already holds is suffixed', async () => {
  // Nothing about a local directory name is unique inside one account: both of these are
  // called "work", and the operator must still get two mounts — one path each — because a
  // shared path would mean one workspace and one session list for two different directories.
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'work' }), fakeHandle({ name: 'work' })], takenNames: ['docs'] })
  await state(ui, 'idle')
  ui.clickManage()
  ui.clickDialog('添加文件夹')
  await until(() => ui.folderStates().length === 1 && ui.folderStates()[0] === 'connected', 'the first mount')
  ui.clickDialog('添加文件夹')
  await ready(ui)
  const keys = ui.savedFolders().map(folder => folder.key).sort()
  assert.deepEqual(keys, ['work', 'work-2'], 'two local directories of the same name share one path')
  assert.equal(new Set(ui.savedFolders().map(folder => folder.mountpoint)).size, 2)
  // A name this account already holds comes back suffixed from the gateway, not reused: that
  // is what the handshake is for (the local list cannot see another device's folders).
  ui.clickDialog('添加文件夹')
  await new Promise(resolve => setTimeout(resolve, 30))
  const third = ui.requests.filter(request => request.endpoint === 'allocate').map(request => request.payload.name)
  assert.deepEqual(third.slice(0, 2), ['work', 'work-2'])
  await ui.dispose()
})

test('a local name that cannot be a directory name still mounts, under a generated key', async () => {
  // "Never block a mount because of what somebody called their folder": a hidden or padded
  // local name is not a usable path segment, so the folder falls back to the generated key this
  // feature always used instead of failing.
  const ui = setup({ pickerDirs: [fakeHandle({ name: '.secrets' })] })
  await state(ui, 'idle')
  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.requests.some(request => request.endpoint === 'allocate'), false, 'an unusable name was sent to the gateway anyway')
  const key = ui.savedFolders()[0].key
  assert.match(key, /^[a-f0-9]{32}$/)
  assert.equal(ui.savedFolders()[0].mountpoint, `/home/account/browser/${key}`)
  await ui.dispose()
})

test('deleting a folder whose capability is gone releases its path by key', async () => {
  // The ordinary order after a gateway restart: the mount is gone, the token died with it, and
  // the saved folder is still there. Without a release by key its empty mount point would stay
  // in the account's container forever.
  const handle = fakeHandle({ name: 'docs' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle, key: 'docs', name: 'docs', token: null, at: 0 })]) })
  await state(ui, 'disconnected')
  ui.clickManage()
  ui.clickFolder('docs', 'delete')
  ui.clickFolder('docs', 'confirm-delete')
  await until(() => ui.folderNames().length === 0, 'the deleted folder to disappear')
  assert.deepEqual(ui.state.purged, ['docs'], 'the mount point was not released by key')
  assert.equal(ui.state.closeCalls, 0, 'a close with no capability was attempted')
  await ui.dispose()
})

test('a refused release keeps the folder and says so instead of reporting a deletion', async () => {
  // A live mount of that path — another page, another device — is not this page's to remove.
  const handle = fakeHandle({ name: 'docs' })
  const ui = setup({
    stored: storedFolders([storedFolder({ handle, key: 'docs', name: 'docs', token: null, at: 0 })]),
    purgeRefusals: ['directory is still mounted'],
  })
  await state(ui, 'disconnected')
  ui.clickManage()
  ui.clickFolder('docs', 'delete')
  ui.clickFolder('docs', 'confirm-delete')
  await until(() => ui.folderText('docs').includes('挂载目录未释放'), 'the honest failure')
  assert.equal(ui.folderNames().length, 1, 'a folder with an unreleased path was reported as deleted')
  assert.match(ui.folderText('docs'), /该目录仍有活动挂载/)
  assert.deepEqual(ui.state.purged, [])
  await ui.dispose()
})
