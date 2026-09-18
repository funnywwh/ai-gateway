// Unit tests for the tenant-side ssh-workspace plugin (M64). They run without a network, a
// key or a real dsh: a fake `ssh` on PATH answers the three remote commands the plugin makes,
// and the mailbox is an ordinary temporary directory. Run by scripts/dshgw-test.
import { strict as assert } from 'node:assert'
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const pluginPath = process.env.DSHGW_SSH_PLUGIN || join(process.cwd(), 'cmd/dshgw/plugin/ssh-workspace/index.js')
const plugin = await import(pathToFileURL(pluginPath).href + `?test=${Date.now()}`)

let assertions = 0
const check = (condition, message) => {
  assertions += 1
  assert.ok(condition, message)
}
const equal = (actual, expected, message) => {
  assertions += 1
  assert.equal(actual, expected, message)
}
const deepEqual = (actual, expected, message) => {
  assertions += 1
  assert.deepEqual(actual, expected, message)
}

// ── pure functions ───────────────────────────────────────────────────────────────────────
equal(plugin.shellQuote("it's"), `'it'\\''s'`, 'shellQuote escapes single quotes')
equal(plugin.shellQuote('/a b'), "'/a b'", 'shellQuote keeps a space in one word')
for (const host of ['gpt001', 'user@host', 'aipc.example.com']) {
  equal(plugin.checkHostSpec(host), host, 'valid host spec accepted')
}
for (const host of ['-oProxyCommand=x', 'host name', 'host/path', '', '../escape']) {
  assertions += 1
  assert.throws(() => plugin.checkHostSpec(host), (error) => error.code === 'ssh/host-unknown', `host ${host} must be refused`)
}
equal(plugin.checkRemotePath('/opt/app'), '/opt/app', 'valid remote path accepted')
for (const path of ['relative', '/a/../b', '/a//b', '/a\u0000b', '']) {
  assertions += 1
  assert.throws(() => plugin.checkRemotePath(path), (error) => error.code === 'ssh/invalid-path', `path ${path} must be refused`)
}
equal(plugin.checkSegment('logs'), 'logs', 'valid segment accepted')
for (const segment of ['', '.', '..', 'a/b', 'a\\b']) {
  assertions += 1
  assert.throws(() => plugin.checkSegment(segment), (error) => error.code === 'ssh/invalid-path', `segment ${segment} must be refused`)
}

const parsed = plugin.parseSSHConfig(`
# comment
Host *
  User nobody
Host gpt001
  HostName gpt001.iotalking.top
  Port 2222
  User root
Host aipc !skipme
  HostName 192.168.140.252
Host gpt001
  HostName ignored
`)
deepEqual(parsed.map((host) => host.name), ['gpt001', 'aipc'], 'only concrete aliases are listed')
equal(parsed[0].hostName, 'gpt001.iotalking.top', 'HostName parsed')
equal(parsed[0].port, 2222, 'Port parsed')
equal(parsed[0].user, 'root', 'User parsed')
equal(parsed[1].hostName, '192.168.140.252', 'second alias parsed')

equal(plugin.mountpointFor('/w/dsh-a', 'ssh', 'gpt001', '/opt/app'), '/w/dsh-a/ssh/gpt001/opt/app', 'mount point mirrors the remote path')
assertions += 1
assert.throws(() => plugin.mountpointFor('/w/dsh-a', 'ssh', 'gpt001', '/../etc'), (error) => error.code === 'ssh/invalid-path', 'a traversal is refused')
deepEqual(
  plugin.remoteFromMountpoint('/w/dsh-a', 'ssh', '/w/dsh-a/ssh/gpt001/opt/app'),
  { host: 'gpt001', remote: '/opt/app' },
  'a mount point maps back to its remote path',
)
equal(plugin.remoteFromMountpoint('/w/dsh-a', 'ssh', '/w/other/ssh/gpt001'), null, 'a foreign mount point is refused')

// ── the plugin against a fake ssh and a real mailbox ─────────────────────────────────────
const root = await mkdtemp(join(tmpdir(), 'dshgw-ssh-'))
const home = join(root, 'workspace')
const dshHome = join(root, 'tenants', 'dsh-a', '.dsh')
const bin = join(root, 'bin')
await mkdir(join(home, '.ssh'), { recursive: true })
await mkdir(dshHome, { recursive: true })
await mkdir(bin, { recursive: true })
await writeFile(join(home, '.ssh', 'id_rsa'), 'PRIVATE KEY\n', { mode: 0o600 })
await writeFile(join(home, '.ssh', 'config'), 'Host gpt001\n  HostName gpt001.example\n  User root\nHost aipc\n  HostName 10.0.0.9\n')
process.env.HOME = home
process.env.DSH_HOME = dshHome

// The fake ssh answers by the script it was handed, so the plugin's own argument assembly is
// what is exercised: a script it would never compose simply gets no answer.
const fakeSSH = `#!/bin/sh
printf '%s\\n' "$@" > "${root}/ssh-args.txt"
command="$*"
case "$command" in
  *'printf %s "$HOME"'*) printf %s /home/remote ;;
  *"pwd -P"*) printf '%s\\n' /srv/app ;;
  *"ls -1ap"*) printf './\\n../\\napp/\\nlogs/\\n.hidden/\\nreadme.txt\\n' ;;
  *"mkdir --"*) exit 0 ;;
  *) echo "unexpected script: $command" >&2; exit 9 ;;
esac
`
await writeFile(join(bin, 'ssh'), fakeSSH, { mode: 0o755 })
await chmod(join(bin, 'ssh'), 0o755)
process.env.PATH = `${bin}:${process.env.PATH}`

const handlers = new Map()
const applyCtx = {
  connection: { rpc: { handle: (channel, handler) => { handlers.set(channel, handler); return () => {} } } },
  effect: (fn) => fn(),
  logger: { info: () => {}, warn: () => {} },
}

try {
  plugin.apply(applyCtx, { mountSubdir: 'ssh', hosts: ['gpt001'], maxEntries: 2, connectTimeoutMs: 5000 })
  check(handlers.has('/ssh-workspace'), 'the plugin registers its RPC channel')
  const call = (endpoint, payload) => handlers.get('/ssh-workspace')(endpoint, payload, new AbortController().signal)

  const hosts = await call('hosts', {})
  equal(hosts.ok, true, 'hosts succeeds')
  deepEqual(hosts.value.aliases.map((host) => host.name), ['gpt001', 'aipc'], 'aliases come from the account config')
  deepEqual(hosts.value.allowList, ['gpt001'], 'the deployment allow-list is reported')
  equal(hosts.value.mountRoot, join(home, 'ssh'), 'the mount container is inside the account workspace')
  equal(hosts.value.identity, true, 'the provisioned identity is reported')

  const probe = await call('probe', { host: 'gpt001' })
  equal(probe.value.home, '/home/remote', 'probe reports the remote home')
  // The remote script reaches the host as one already-quoted word: ssh joins the arguments
  // and the remote shell parses them again, so a raw script would be re-split (this is what
  // the fake ssh records below, and what the gateway's own ssh calls do).
  const captured = (await readFile(join(root, 'ssh-args.txt'), 'utf8')).trim().split('\n')
  const remoteScript = captured[captured.length - 1]
  equal(remoteScript.startsWith("'") && remoteScript.endsWith("'"), true,
    `the remote script must arrive as one quoted word, got ${remoteScript}`)
  check(remoteScript.includes('printf %s "$HOME"'), 'the quoted word is the very script ssh was handed')

  const listing = await call('list', { host: 'gpt001', path: '/srv/app' })
  deepEqual(listing.value.entries.map((entry) => entry.name), ['app', 'logs'],
    'only real directories are listed: ".", ".." and files are dropped, and maxEntries bounds it')
  equal(listing.value.truncated, true, 'the bound is reported')
  equal(listing.value.entries[0].path, '/srv/app/app', 'entries carry absolute remote paths')

  const created = await call('mkdir', { host: 'gpt001', path: '/srv/app', name: 'logs2' })
  equal(created.ok, true, 'mkdir succeeds')
  equal(created.value.path, '/srv/app/logs2', 'mkdir reports the created path')
  const badName = await call('mkdir', { host: 'gpt001', path: '/srv/app', name: '../escape' })
  equal(badName.ok, false, 'mkdir refuses a traversal')
  equal(badName.error.code, 'ssh/invalid-path', 'mkdir reports the traversal code')

  const refusedHost = await call('open', { host: 'aipc', remote: '/srv/app' })
  equal(refusedHost.ok, false, 'a host outside the allow-list is refused')
  equal(refusedHost.error.code, 'ssh/host-unknown', 'the refusal carries the host code')
  const refusedPath = await call('open', { host: 'gpt001', remote: 'relative' })
  equal(refusedPath.error.code, 'ssh/invalid-path', 'a relative remote path is refused')

  const opened = await call('open', { host: 'gpt001', remote: '/srv/app' })
  equal(opened.ok, true, 'open succeeds')
  equal(opened.value.mountpoint, join(home, 'ssh', 'gpt001', 'srv', 'app'), 'open predicts the gateway mount point')
  equal(opened.value.pending, true, 'open reports a pending mount')
  const requests = await readdir(join(dshHome, 'ssh-requests'))
  equal(requests.length, 1, 'open left exactly one request in the mailbox')
  const request = JSON.parse(await readFile(join(dshHome, 'ssh-requests', requests[0]), 'utf8'))
  equal(request.op, 'open', 'the mailbox request asks for a mount')
  equal(request.host, 'gpt001', 'the request names the host')
  equal(request.remote, '/srv/app', 'the request names the remote directory')
  const requestMode = (await stat(join(dshHome, 'ssh-requests', requests[0]))).mode & 0o777
  equal(requestMode, 0o600, 'mailbox requests are private')

  // The gateway answers through the reply directory and records the mount in the account's
  // mirror, which is the only way to tell a mount from a parent directory of the layout.
  await mkdir(join(dshHome, 'ssh-replies'), { recursive: true })
  await writeFile(join(dshHome, 'ssh-replies', 'open-1.json'), `${JSON.stringify({ id: 'open-1', ok: true, mountpoint: opened.value.mountpoint })}\n`, { mode: 0o600 })
  equal((await call('mounts', {})).value.mirror, false, 'no mirror before the gateway writes one')
  await writeFile(join(dshHome, 'ssh-mounts.json'), `${JSON.stringify({
    version: 1,
    tenant: 'dsh-a',
    mounts: [{ tenant: 'dsh-a', host: 'gpt001', remote: '/srv/app', canonical_remote: '/srv/app', mountpoint: opened.value.mountpoint, created_at: '2026-09-18T12:00:00Z' }],
  })}\n`, { mode: 0o600 })
  const mounts = await call('mounts', {})
  equal(mounts.value.mirror, true, 'the mirror is reported')
  deepEqual(mounts.value.mounts.map((mount) => mount.remote), ['/srv/app'], 'the recorded mount is reported')
  equal(mounts.value.mounts[0].path, opened.value.mountpoint, 'the recorded mount carries its path')
  equal(mounts.value.replies.length, 1, 'a pending reply is surfaced')
  const acked = await call('ack', { id: 'open-1' })
  equal(acked.ok, true, 'a reply can be acknowledged')
  equal((await call('mounts', {})).value.replies.length, 0, 'an acknowledged reply is gone')
  const badAck = await call('ack', { id: '../../etc/passwd' })
  equal(badAck.ok, false, 'a hostile reply id is refused')

  const foreign = await call('close', { mountpoint: '/etc' })
  equal(foreign.error.code, 'mount/forbidden', 'a mount point outside the container cannot be detached')
  const closed = await call('close', { mountpoint: join(home, 'ssh', 'gpt001', 'srv', 'app') })
  equal(closed.ok, true, "close succeeds for one of this account's mounts")
  const closeRequest = JSON.parse(await readFile(join(dshHome, 'ssh-requests', closed.value.id + '.json'), 'utf8'))
  equal(closeRequest.op, 'close', 'the close request asks for a detach')
  equal(closeRequest.host, 'gpt001', 'the close request carries the host derived from the mount point')

  equal((await call('nonsense', {})).error.code, 'ssh/invalid-path', 'an unknown endpoint is refused')

  // Configuration is validated at activation: a mount container that would escape the
  // account's workspace must never reach a profile.
  assertions += 1
  assert.throws(() => plugin.apply(applyCtx, { mountSubdir: '../escape' }), /mountSubdir/, 'a traversing mount container is refused')
  assertions += 1
  assert.throws(() => plugin.apply(applyCtx, { hosts: ['-oProxyCommand=x'] }), /not an ssh alias/, 'a hostile allow-list entry is refused')

  console.log(`ssh-workspace: ${assertions} assertions passed`)
} finally {
  await rm(root, { recursive: true, force: true })
}
