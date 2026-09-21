// What a page does when its connection dies, and what the NEXT page does with the folders the
// previous one left behind. Both are the same mechanism seen from two sides:
//
//   · inside one page, a transport failure is recovered in place (resume, keep serving) —
//     no reload, no second directory choice, no lost mount;
//   · in a new document (a reload, or a tab that replaced the old one), the saved folders
//     come back with their handles, so one click takes the SAME mount back — it does not open
//     the picker and it does not rebuild anything.
//
// The third thing pinned here is what a DISCONNECT is not: it is not a removal. The folder
// stays saved, its capability goes, and the workspace mapping (the stable key, and with it the
// path DSH keys its own workspace entry by) survives.
import test from 'node:test'
import assert from 'node:assert/strict'
import { setup, until, fakeHandle, storedFolder, storedFolders, ORIGIN } from './harness.mjs'

const mounted = ui => until(() => ui.rowState() === 'mounted', 'a live mount')
const state = (ui, wanted) => until(() => ui.rowState() === wanted, wanted)

test('a page that finds a saved folder offers to restore it, without opening the picker', async () => {
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle, key: 'a'.repeat(32) })]) })
  await state(ui, 'resumable')
  // Boot only reads the saved folders: it must not resume anything by itself, because the
  // mount belongs to a click.
  assert.equal(ui.state.resumeCalls, 0, 'the page resumed on its own')
  assert.match(ui.rowText(), /刷新前挂载的是 local；点击恢复/)
  assert.equal(ui.events.includes('open'), false)

  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.state.resumeCalls, 1)
  assert.ok(!ui.events.includes('picker'), 'restoring must not ask for a directory again')
  assert.ok(!ui.events.includes('create'), 'restoring must not register a second workspace')
  assert.ok(!ui.events.includes('activate'), 'restoring must not restart the worker')
  assert.match(ui.rowText(), /已挂载 local（读写，已恢复）；点击断开/)
  // The workspace that the path already had is reopened, not duplicated.
  assert.ok(ui.events.includes('connect:workspace'), 'the restored workspace was not reopened')
  await ui.dispose()
})

test('a saved folder whose permission the browser wants re-confirmed asks for the directory again', async () => {
  const handle = fakeHandle({ name: 'local', permission: 'prompt' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle })]) })
  await state(ui, 'disconnected')
  // Nothing to resume on its own, but the FOLDER is not thrown away: it is the mapping, and
  // the person only has to point at it again.
  assert.equal(ui.rowText().includes('local'), true)
  assert.match(ui.rowText(), /已断开 local；点击重新选择目录/)
  assert.equal(ui.savedFolders().length, 1, 'the saved folder was dropped for a permission prompt')

  ui.clickRow()
  await until(() => ui.events.includes('picker'), 'the picker for the re-grant')
  assert.equal(ui.state.resumeCalls, 0)
  await ui.dispose()
})

test('a refused resume still mounts the SAME local directory on the same click', async () => {
  const handle = fakeHandle({ name: 'local' })
  const key = 'b'.repeat(32)
  const ui = setup({
    stored: storedFolders([storedFolder({ handle, key, mountpoint: `/home/account/browser/${key}` })]),
    // The gateway's grace window is over: it no longer holds that capability, and the key is
    // free for the fresh mount the same click has to produce.
    mountAlive: false,
    resumeRefusals: ['unknown directory capability'],
  })
  await state(ui, 'resumable')
  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.state.resumeCalls, 1)
  // "unknown directory capability" is final for that capability: the dead token must not
  // survive, but the folder must — and the fresh mount carries the SAME stable key, so the
  // virtual path (and with it the workspace entry) is the one this directory already had.
  const opened = ui.requests.filter(request => request.endpoint === 'open')
  assert.equal(opened.length, 1)
  assert.equal(opened[0].payload.key, key)
  assert.ok(!ui.events.includes('picker'), 'a re-granted handle needs no picker')
  assert.equal(ui.savedFolders()[0].token !== null, true)
  await ui.dispose()
})

test('an expired capability is dropped but the folder stays saved', async () => {
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle, at: Date.now() - 10 * 60 * 1000 })]) })
  await state(ui, 'disconnected')
  // The token is not offered — the gateway's grace window is long past — but the folder, its
  // handle and the mapping survive, so connecting again needs no directory dialog.
  assert.equal(ui.savedFolders().length, 1)
  assert.equal(ui.savedFolders()[0].token, null)
  assert.equal(ui.savedFolders()[0].handle.name, 'local')
  await ui.dispose()
})

test('a transport failure inside one page reconnects instead of tearing the mount down', async () => {
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle })]), pollFailures: 1 })
  await state(ui, 'resumable')
  ui.clickRow()
  await mounted(ui)
  await until(() => ui.state.resumeCalls === 2, 'the in-page reconnect')
  // The poll failed once, the page asked to resume, and it kept serving: no close, no
  // "请重新选择目录", and the mount is still owned by this page.
  assert.equal(ui.events.includes('close'), false, 'a recoverable failure closed the mount')
  assert.match(ui.rowText(), /已挂载 local（读写，已重连）；点击断开/)
  await ui.dispose()
})

test('the first successful mount saves the folder the next document needs', async () => {
  const ui = setup({ pickerDirs: [fakeHandle({ name: 'local' })] })
  await until(() => ui.rowState() === 'idle', 'the empty page')
  ui.clickRow()
  await mounted(ui)
  const saved = ui.savedFolders()
  assert.equal(saved.length, 1, 'the mount did not leave a saved folder')
  assert.equal(saved[0].key, 'local', 'the mapping key is the local directory name, and that is what names the mount point')
  assert.equal(saved[0].name, 'local')
  assert.equal(saved[0].token !== null, true)
  assert.ok(saved[0].handle, 'the record has no directory handle to resume with')
  // The key the record carries is the key the gateway was asked to mount.
  assert.equal(ui.requests.find(request => request.endpoint === 'open').payload.key, saved[0].key)
  await ui.dispose()
})

test('an explicit disconnect keeps the folder and its workspace mapping, dropping only the capability', async () => {
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle })]) })
  await state(ui, 'resumable')
  ui.clickRow()
  await mounted(ui)
  ui.clickRow()
  await state(ui, 'disconnected')
  assert.ok(ui.events.includes('close'))
  const saved = ui.savedFolders()
  assert.equal(saved.length, 1, 'the disconnected folder was removed')
  assert.equal(saved[0].token, null, 'the dead capability survived the disconnect')
  assert.ok(saved[0].handle, 'the directory handle was dropped with the capability')
  assert.equal(saved[0].key, 'a'.repeat(32), 'the stable mapping key changed')
  // The workspace entry is NOT deleted: it is the mapping this local directory keeps, and the
  // mount point at that path stays for the next connect instead of being rebuilt elsewhere.
  assert.deepEqual(ui.events.filter(event => event.startsWith('delete:')), [])
  const closes = ui.requests.filter(request => request.endpoint === 'close')
  assert.equal(closes.length, 1)
  // A plain disconnect carries no `purge` at all: the gateway keeps the mount point, which is
  // what keeps the workspace entry (and its sessions) mapped to this local directory.
  assert.equal(closes[0].payload.purge, undefined, 'a disconnect must not release the virtual path')
  await ui.dispose()
})

test('deleting a folder releases the virtual path and the workspace entry', async () => {
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle, workspaceId: 'workspace' })]) })
  await state(ui, 'resumable')
  ui.clickRow()
  await mounted(ui)
  assert.deepEqual(ui.folderActions('local'), ['打开', '断开', '删除'])
  ui.clickFolder('local', 'delete')
  assert.deepEqual(ui.folderActions('local'), ['打开', '断开', '确认删除'], 'deleting needs a second, deliberate click')
  ui.clickFolder('local', 'confirm-delete')
  await until(() => ui.folderNames().length === 0, 'the folder to be removed')
  const closes = ui.requests.filter(request => request.endpoint === 'close')
  assert.equal(closes.at(-1).payload.purge, true, 'a deletion must release the stable mount point')
  assert.equal(ui.events.includes('delete:workspace'), true, 'the workspace entry outlived its folder')
  assert.equal(ui.savedFolders().length, 0, 'the deleted folder is still saved')
  assert.match(ui.rowText(), /浏览器工作区/, 'the row goes back to plain 浏览器工作区')
  await ui.dispose()
})

test('a record written by the previous client version is adopted, key and all', async () => {
  // v1 kept exactly one mount per origin under `mount:<origin>`, with a random id that named
  // the mount point. It is adopted as a folder whose stable key IS that id, so the path the
  // workspace entry already points at is the path the next mount uses.
  const handle = fakeHandle({ name: 'local' })
  const legacyKey = 'c'.repeat(48)
  const ui = setup({
    legacy: true,
    stored: {
      version: 1, token: 'private-token', id: legacyKey, name: 'local', workspaceId: 'workspace',
      handle, at: Date.now(), mountpoint: `/home/account/browser/${legacyKey}`,
    },
  })
  await state(ui, 'resumable')
  assert.equal(ui.legacyRecord(), null, 'the v1 record was left behind for another tab to adopt')
  const saved = ui.savedFolders()
  assert.equal(saved.length, 1)
  assert.equal(saved[0].key, legacyKey, 'the adopted folder lost the path its workspace points at')
  assert.equal(saved[0].token, 'private-token')
  await ui.dispose()
})

test('a folder whose gateway mount is gone after the grace window reconnects through open, not resume', async () => {
  // The page kept the token, the gateway did not keep the mount: the resume is refused and the
  // click must still produce a working mount instead of a dead end.
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle })]), mountAlive: false, resumeRefusals: ['unknown directory capability', 'unknown directory capability'] })
  await state(ui, 'resumable')
  ui.clickRow()
  await mounted(ui)
  assert.equal(ui.requests.filter(request => request.endpoint === 'open').length, 1)
  assert.equal(ui.savedFolders().length, 1)
  // The folder is mounted, and the record carries the NEW capability rather than the dead one.
  assert.equal(ui.savedFolders()[0].token !== null, true)
  assert.equal(ui.savedFolders()[0].mountpoint, `/home/account/browser/${'a'.repeat(32)}`)
  await ui.dispose()
})

test('a folder served by another page is reported, never mounted twice', async () => {
  const handle = fakeHandle({ name: 'local' })
  const ui = setup({ stored: storedFolders([storedFolder({ handle })]), resumeRefusals: ['directory already served by another page'] })
  await state(ui, 'resumable')
  ui.clickRow()
  await state(ui, 'failed')
  // Two browsers writing one local directory through two FUSE mounts is the failure this
  // refusal prevents, so the click must NOT fall back to a fresh mount.
  assert.equal(ui.requests.filter(request => request.endpoint === 'open').length, 0)
  assert.match(ui.rowText(), /另一个页面服务/)
  assert.equal(ui.folderStates()[0], 'elsewhere')
  await ui.dispose()
})

test('the origin keys the saved folders, so another tenant never sees them', async () => {
  const ui = setup({ stored: { version: 2, folders: [] } })
  await state(ui, 'idle')
  assert.equal(ui.indexedDB.data.has(`folders:${ORIGIN}`), true)
  assert.equal(ui.indexedDB.data.has('folders:http://evil.test'), false)
  await ui.dispose()
})
