import { strict as assert } from 'node:assert'
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const runtime = process.env.DSHGW_DSH_ROOT || '/home/winger/.local/dsh-0.1.2-rc.1'
process.env.DSHGW_DSH_ANCHOR = join(runtime, 'package.json')
const { Context } = await import(pathToFileURL(join(runtime, 'node_modules/@deepseek-ai/cordis/lib/index.js')).href)
const plugin = process.env.DSHGW_PICKER_PLUGIN || join(process.cwd(), 'cmd/dshgw/plugin/picker-clamp.js')
const { default: Clamp } = await import(pathToFileURL(plugin).href + `?test=${Date.now()}`)
const rootPath = await mkdtemp(join(tmpdir(), 'dshgw-picker-'))
try {
  await mkdir(join(rootPath, 'zeta'))
  await mkdir(join(rootPath, '.hidden'))
  await mkdir(join(rootPath, 'alpha'))
  await writeFile(join(rootPath, 'file.txt'), 'not a directory')
  await symlink('/etc', join(rootPath, 'escape'))
  const ctx = new Context()
  await ctx.plugin(Clamp, { root: rootPath, maxEntries: 2 })
  const cap = ctx.directoryPicker.capability()
  assert.equal(cap.kind, 'browse')
  assert.equal(cap, ctx.directoryPicker.capability(), 'capability must be stable')
  const listing = await cap.list()
  assert.equal(listing.path, rootPath)
  assert.equal(listing.home, rootPath)
  assert.deepEqual(listing.crumbs, [{ name: rootPath, path: rootPath, hidden: false }])
  assert.deepEqual(listing.entries.map((x) => x.name), ['.hidden', 'alpha'])
  assert.equal(listing.entries[0].hidden, true)
  assert.equal(listing.truncated, true)
  // A vanished directory (a browser/SSH mount the gateway tore down) must not break
  // the dialog: the listing falls back to the nearest existing ancestor inside the root.
  const gone = await cap.list(join(rootPath, 'zeta', 'gone'))
  assert.equal(gone.path, join(rootPath, 'zeta'))
  assert.deepEqual(gone.crumbs.map((x) => x.path), [rootPath, join(rootPath, 'zeta')])
  const deepGone = await cap.list(join(rootPath, 'gone-a', 'gone-b'))
  assert.equal(deepGone.path, rootPath)
  assert.deepEqual(deepGone.entries.map((x) => x.name), ['.hidden', 'alpha'])
  await assert.rejects(() => cap.createDirectory(join(rootPath, 'gone-a'), 'x'), (e) => e.code === 'directory-create-failed')
  await assert.rejects(() => cap.list('/etc'), (e) => e.code === 'directory-unreadable')
  await assert.rejects(() => cap.list('/nonexistent-outside-root'), (e) => e.code === 'directory-unreadable')
  await assert.rejects(() => cap.list(join(rootPath, 'escape')), (e) => e.code === 'directory-unreadable')
  await assert.rejects(() => cap.createDirectory(join(rootPath, 'escape'), 'x'), (e) => e.code === 'directory-create-failed')
  assert.equal(await cap.createDirectory(rootPath, 'new'), join(rootPath, 'new'))
  await assert.rejects(() => cap.createDirectory(rootPath, 'new'), (e) => e.code === 'directory-exists')
  ctx.registry.delete(Clamp)

  const denied = new Context()
  await denied.plugin(Clamp, { root: join(rootPath, 'missing') })
  assert.equal(denied.directoryPicker.capability().kind, 'browse')
  await assert.rejects(() => denied.directoryPicker.capability().list(), (e) => e.code === 'directory-unreadable')
  denied.registry.delete(Clamp)
  console.log('picker-clamp: 23 assertions passed')
} finally {
  await rm(rootPath, { recursive: true, force: true })
}
