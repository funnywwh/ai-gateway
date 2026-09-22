// Unit tests for the tenant-side ssh-workspace plugin (M64). They run without a network, a
// key or a real dsh: a fake `ssh` on PATH answers the three remote commands the plugin makes,
// and the mailbox is an ordinary temporary directory. Run by scripts/dshgw-test.
import { strict as assert } from 'node:assert'
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, stat, writeFile, symlink } from 'node:fs/promises'
import { createHash } from 'node:crypto'
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

// ── 「我的主机」: entry validation and config surgery (pure) ──────────────────────────────
deepEqual(plugin.checkAliasEntry({ hostname: '10.0.0.5' }), { name: '10.0.0.5', hostName: '10.0.0.5', user: '', port: 0 },
  'the alias defaults to the address')
deepEqual(plugin.checkAliasEntry({ name: ' myhost ', hostname: ' gpt001.iotalking.top ', user: ' root ', port: '2222' }),
  { name: 'myhost', hostName: 'gpt001.iotalking.top', user: 'root', port: 2222 }, 'one entry normalized')
equal(plugin.aliasSpec({ hostName: 'h', user: 'u', port: 22 }), 'u@h:22', 'an entry has one connection identity')
equal(plugin.aliasSpec({ hostName: 'h', user: '', port: 0 }), 'h', 'no user and no port is just the address')
for (const [payload, code] of [
  [{ hostname: '' }, 'ssh/invalid-hostname'],
  [{ hostname: 'has space' }, 'ssh/invalid-hostname'],
  [{ hostname: '-oProxyCommand=x' }, 'ssh/invalid-hostname'],
  [{ hostname: 'h', user: 'a b' }, 'ssh/invalid-user'],
  [{ hostname: 'h', port: '0' }, 'ssh/invalid-port'],
  [{ hostname: 'h', port: '65536' }, 'ssh/invalid-port'],
  [{ hostname: 'h', port: '22a' }, 'ssh/invalid-port'],
  [{ hostname: 'h', name: 'with space' }, 'ssh/invalid-alias'],
  [{ hostname: 'h', name: '-leading' }, 'ssh/invalid-alias'],
  [{ hostname: 'h', name: 'a'.repeat(65) }, 'ssh/invalid-alias'],
]) {
  assertions += 1
  assert.throws(() => plugin.checkAliasEntry(payload), (error) => error.code === code, `entry ${JSON.stringify(payload)} must be refused`)
}
equal(plugin.renderAliasBlock({ name: 'myhost', hostName: '10.0.0.5', user: 'ops', port: 2022 }),
  'Host myhost\n  HostName 10.0.0.5\n  User ops\n  Port 2022\n', 'a block is rendered like the gateway renders one')
equal(plugin.renderAliasBlock({ name: 'h', hostName: 'h', user: '', port: 0 }), 'Host h\n  HostName h\n',
  'empty fields are omitted, never written blank')

const gatewayRendered = '# Generated by dshgw: host aliases only. This account\'s identity is its own\n# ~/.ssh/id_rsa; per-host keys and other local settings are not carried over.\n\nHost aipc\n  HostName 192.168.140.252\n  User winger\n'
equal(plugin.spliceAlias('', { name: 'myhost', hostName: '10.0.0.5', user: '', port: 0 }),
  gatewayRendered.replace('Host aipc\n  HostName 192.168.140.252\n  User winger', 'Host myhost\n  HostName 10.0.0.5'),
  'a config created from nothing carries the gateway header')
const appended = plugin.spliceAlias(gatewayRendered, { name: 'myhost', hostName: '10.0.0.5', user: 'ops', port: 0 })
check(appended.startsWith(gatewayRendered) && appended.endsWith('\nHost myhost\n  HostName 10.0.0.5\n  User ops\n'), 'a new entry is appended')
deepEqual(plugin.parseSSHConfig(appended).map((host) => host.name), ['aipc', 'myhost'], 'both entries parse')
// A tenant's own directives and comments are preserved: only the four managed fields move.
const handEdited = 'Host aipc\n  HostName 192.168.140.252\n  HostKeyAlgorithms +ssh-rsa\n# keep me\n\nHost other\n  HostName 10.0.0.9\n'
const replaced = plugin.spliceAlias(handEdited, { name: 'aipc', hostName: '192.168.140.252', user: 'winger', port: 2222 })
check(replaced.includes('HostKeyAlgorithms +ssh-rsa') && replaced.includes('# keep me'), 'directives this plugin does not manage survive a rewrite')
check(replaced.includes('Host other') && replaced.includes('HostName 10.0.0.9'), 'other blocks are untouched')
deepEqual(plugin.parseSSHConfig(replaced).map((host) => host.name), ['aipc', 'other'], 'the rewritten config still parses')
const rewritten = plugin.parseSSHConfig(replaced)[0]
check(rewritten.hostName === '192.168.140.252' && rewritten.user === 'winger' && rewritten.port === 2222, 'the managed fields are the new ones')
const removed = plugin.removeAliasBlock(replaced, 'aipc')
equal(removed.removed, true, 'the named block is removed')
equal(removed.text, 'Host other\n  HostName 10.0.0.9\n', 'removal takes its block, its separator and nothing else')
equal(plugin.removeAliasBlock(removed.text, 'absent').removed, false, 'removing an unknown alias reports that nothing was removed')
equal(plugin.removeAliasBlock('# header\n\nHost a\n  HostName a\n', 'a').text, '# header\n', 'removing the last block leaves a header-only config')

// ── the plugin against a fake ssh and a real mailbox ─────────────────────────────────────
const root = await mkdtemp(join(tmpdir(), 'dshgw-ssh-'))
const home = join(root, 'workspace')
const dshHome = join(root, 'tenants', 'dsh-a', '.dsh')
const bin = join(root, 'bin')
// Only ssh is mocked; key validation exercises the system's real ssh-keygen.
const makeKey = async (name, passphrase = '') => {
  const path = join(root, name)
  const result = await plugin.runCommand('ssh-keygen', ['-q', '-t', 'ed25519', '-N', passphrase, '-f', path])
  assert.equal(result.error, null, result.stderr)
  return await readFile(path, 'utf8')
}
const keyA = await makeKey('key-a')
const keyB = await makeKey('key-b')
const encrypted = await makeKey('encrypted', 'secret')
const publicA = (await readFile(join(root, 'key-a.pub'), 'utf8')).split(' ')[1]
const fingerprintA = 'SHA256:' + createHash('sha256').update(Buffer.from(publicA, 'base64')).digest('base64').replace(/=+$/, '')
await mkdir(join(home, '.ssh'), { recursive: true })
await mkdir(dshHome, { recursive: true })
await mkdir(bin, { recursive: true })
await writeFile(join(home, '.ssh', 'id_rsa'), keyA, { mode: 0o600 })
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

  // 「我的主机」is refusal-first where the deployment restricts hosts: neither the alias nor the
  // identity it stands for may be outside the allow-list, an existing name is not silently
  // rewritten, and a refused add leaves the config byte for byte as it was.
  const seededConfig = await readFile(join(home, '.ssh', 'config'), 'utf8')
  equal((await call('addHost', { name: 'blocked', hostname: '10.0.0.5' })).error.code, 'ssh/host-unknown', 'an entry whose alias is not allow-listed is refused')
  equal((await call('addHost', { name: 'gpt001', hostname: '10.0.0.5' })).error.code, 'ssh/host-unknown', 'an entry whose address is not allow-listed is refused')
  equal((await call('addHost', { name: 'gpt001', hostname: 'gpt001' })).error.code, 'ssh/alias-exists', 'an existing alias is not silently rewritten')
  equal(await readFile(join(home, '.ssh', 'config'), 'utf8'), seededConfig, 'a refused add writes nothing')

  // An account whose ~/.ssh has only a key must still work: ssh refuses to start when handed a
  // user config that does not exist ("Can't open user config file …"), which on the live
  // deployment turned everyBrowse click into that message.
  await rm(join(home, '.ssh', 'config'))
  const withoutConfig = await call('probe', { host: 'gpt001' })
  equal(withoutConfig.ok, true, 'probe works without ~/.ssh/config')
  const argsWithoutConfig = (await readFile(join(root, 'ssh-args.txt'), 'utf8')).trim().split('\n')
  equal(argsWithoutConfig[argsWithoutConfig.indexOf('-F') + 1], '/dev/null', 'SSH config cannot add identities')
  const withoutKeyDir = await call('probe', { host: 'gpt001' })
  equal(withoutKeyDir.ok, true, 'probe still works with an empty ~/.ssh')
  await rm(join(home, '.ssh'), { recursive: true, force: true })
  await mkdir(join(home, '.ssh'), { recursive: true })
  await writeFile(join(home, '.ssh', 'id_rsa'), keyA, { mode: 0o600 })
  const bare = await call('probe', { host: 'gpt001' })
  equal(bare.ok, true, 'probe works with a bare ~/.ssh containing only a key')
  const bareArgs = (await readFile(join(root, 'ssh-args.txt'), 'utf8')).trim().split('\n')
  equal(bareArgs[bareArgs.indexOf('-F') + 1], '/dev/null', 'empty config explicitly selected')
  equal(bareArgs.includes('UserKnownHostsFile=' + join(home, '.ssh', 'known_hosts')) || bareArgs.some((arg) => arg.startsWith('UserKnownHostsFile=')), true,
    'known_hosts is still named once the directory exists')

  // Configuration is validated at activation: a mount container that would escape the
  // account's workspace must never reach a profile.
  assertions += 1
  assert.throws(() => plugin.apply(applyCtx, { mountSubdir: '../escape' }), /mountSubdir/, 'a traversing mount container is refused')
  assertions += 1
  assert.throws(() => plugin.apply(applyCtx, { hosts: ['-oProxyCommand=x'] }), /not an ssh alias/, 'a hostile allow-list entry is refused')

  // Re-activate without allow-list restrictions to exercise full host specifications.
  plugin.apply(applyCtx, {})
  for (const endpoint of ['constructor', 'toString', '__proto__', 'hasOwnProperty']) {
    equal((await call(endpoint, {})).ok, false, 'prototype endpoints cannot execute')
  }

  // 「我的主机」: an added host becomes an alias in THIS account's own config, and deleting it
  // takes the alias and that host's dedicated key away again. The config file is the single
  // source of truth for the list.
  const aliasKeyDir = (name) => join(home, '.ssh', 'host_keys', createHash('sha256').update(name, 'utf8').digest('hex'))
  const configText = () => readFile(join(home, '.ssh', 'config'), 'utf8')
  deepEqual((await call('hosts', {})).value.entries, [], 'no hosts before one is added')
  const added = await call('addHost', { hostname: '10.0.0.5', user: 'ops', port: '2022' })
  equal(added.ok, true, 'a host is added')
  deepEqual(added.value.map((entry) => [entry.name, entry.hostName, entry.user, entry.port, entry.key.configured, entry.mounted]),
    [['10.0.0.5', '10.0.0.5', 'ops', 2022, false, false]], 'the list reports the new entry and its key state')
  check((await configText()).includes('Host 10.0.0.5\n  HostName 10.0.0.5\n  User ops\n  Port 2022\n'), 'the alias is written into this account config')
  equal((await stat(join(home, '.ssh', 'config'))).mode & 0o777, 0o644, 'the account config keeps the documented mode')
  equal((await readdir(join(home, '.ssh'))).some((name) => name.startsWith('.config-')), false, 'no staging directory is left behind')
  equal((await call('addHost', { hostname: '10.0.0.5' })).error.code, 'ssh/alias-exists', 'a duplicate name is refused')
  const invalidKey = await call('addHost', { name: 'badkey', hostname: '10.0.0.9', privateKey: 'SECRET INVALID KEY' })
  equal(invalidKey.error.code, 'ssh/invalid-key', 'an unusable key is refused')
  check(!(await configText()).includes('badkey'), 'a refused key does not leave its alias behind')

  const withKey = await call('addHost', { name: 'myhost', hostname: '10.0.0.7', user: 'root', port: 22, privateKey: keyB })
  equal(withKey.ok, true, 'a host with its own key is added')
  equal(await readFile(join(aliasKeyDir('myhost'), 'id_rsa'), 'utf8'), keyB, 'the key is bound to the alias a request will carry')
  equal((await stat(join(aliasKeyDir('myhost'), 'id_rsa'))).mode & 0o777, 0o600, 'the entry key is private')
  const myhostStatus = await call('identityStatus', { host: 'myhost' })
  equal(myhostStatus.value.effective, 'host', 'the entry key is the one this alias connects with')
  check(myhostStatus.value.host.configured && myhostStatus.value.host.fingerprint !== fingerprintA, 'a distinct key was stored')
  equal((await call('hosts', {})).value.entries.find((entry) => entry.name === 'myhost').key.configured, true, 'the list reports the stored key')

  // A host that is currently mounted cannot lose its alias: the gateway re-mounts by that name.
  equal((await call('addHost', { name: 'gpt001', hostname: 'gpt001.example', user: 'root' })).ok, true, 'the mounted host can still be described')
  const busyDelete = await call('deleteHost', { name: 'gpt001' })
  equal(busyDelete.error.code, 'mount/busy', 'an alias a mount is using cannot be deleted')
  check((await configText()).includes('Host gpt001'), 'the refused delete kept the alias')

  const deleted = await call('deleteHost', { name: 'myhost' })
  equal(deleted.ok, true, 'an unused host is deleted')
  check(!(await configText()).includes('myhost'), 'its alias is gone from the config')
  equal((await call('hosts', {})).value.entries.some((entry) => entry.name === 'myhost'), false, 'the list no longer offers it')
  await stat(aliasKeyDir('myhost')).then(() => { throw new Error('the deleted host kept its key directory') }, (error) => {
    equal(error.code, 'ENOENT', 'its dedicated key is gone too')
  })
  check((await configText()).includes('Host 10.0.0.5'), 'the other entry is untouched')

  for (const [payload, code] of [
    [{ hostname: 'bad host' }, 'ssh/invalid-hostname'],
    [{ hostname: 'ok.test', port: '70000' }, 'ssh/invalid-port'],
    [{ hostname: 'ok.test', name: '../escape' }, 'ssh/invalid-alias'],
  ]) {
    equal((await call('addHost', payload)).error.code, code, 'an unusable entry is refused')
  }
  equal((await call('deleteHost', { name: '../escape' })).error.code, 'ssh/invalid-alias', 'a traversal cannot be deleted')
  equal((await call('deleteHost', { name: 'ghost' })).error.code, 'ssh/alias-missing', 'deleting an unknown alias reports it')
  await writeFile(join(home, '.ssh', 'config'), 'Host example.test\n  HostName resolved.example\n  User root\n  Port 2200\n  IdentityFile /unwanted/shared/key\n')
  const host = 'alice@example.test:2222'
  deepEqual(plugin.splitHostSpec(host), { target: 'alice@example.test', port: 2222 }, 'port parsed like Go SplitHostSpec')
  for (const bad of ['host:', 'host:0', 'host:65536', 'host:abc', 'host:22:33', 'host: 22']) {
    equal((await call('identityStatus', { host: bad })).ok, false, 'invalid port refused')
  }
  const absent = { configured: false, fingerprint: '' }
  const initial = await call('identityStatus', { host })
  deepEqual(initial.value, { default: { configured: true, fingerprint: fingerprintA }, host: absent, effective: 'default' }, 'existing key fingerprint derived from public key')
  let result = await call('identityUpload', { scope: 'host', host, privateKey: keyB })
  equal(result.ok, true, 'host key uploaded')
  equal(result.value.effective, 'host', 'host key has priority')
  check(result.value.host.fingerprint !== fingerprintA, 'distinct key has distinct fingerprint')
  const hostDir = join(home, '.ssh', 'host_keys', createHash('sha256').update(host, 'utf8').digest('hex'))
  const hostPath = join(hostDir, 'id_rsa')
  equal(await readFile(hostPath, 'utf8'), keyB, 'full host including user and port determines key path')
  for (const dir of [join(home, '.ssh'), join(home, '.ssh', 'host_keys'), hostDir]) equal((await stat(dir)).mode & 0o777, 0o700, 'key directories private')
  equal((await stat(hostPath)).mode & 0o777, 0o600, 'host key private')
  equal((await call('probe', { host })).ok, true, 'port host connects')
  let args = (await readFile(join(root, 'ssh-args.txt'), 'utf8')).trim().split('\n')
  equal(args[args.indexOf('-p') + 1], '2222', 'ssh port uses -p')
  equal(args[args.indexOf('--') + 1], 'alice@example.test', 'ssh target excludes port')
  equal(args[args.indexOf('-i') + 1], hostPath, 'host identity explicitly selected')
  equal(args[args.indexOf('-F') + 1], '/dev/null', 'untrusted IdentityFile config ignored')
  check(args.includes('HostName=resolved.example'), 'safe alias HostName preserved')
  equal(args.includes('User=root'), false, 'explicit user takes precedence over alias user')
  equal((await call('probe', { host: 'example.test' })).ok, true, 'alias default port connects')
  const aliasArgs = (await readFile(join(root, 'ssh-args.txt'), 'utf8')).trim().split('\n')
  equal(aliasArgs[aliasArgs.indexOf('-p') + 1], '2200', 'alias port used when no explicit port')
  check(aliasArgs.includes('User=root'), 'alias user used when no explicit user')
  for (const invalidPort of ['abc', '0', '65536', '-1', '2.2', '+22']) {
    await writeFile(join(home, '.ssh', 'config'), `Host example.test\n  Port ${invalidPort}\n`)
    equal((await call('probe', { host: 'example.test' })).error.code, 'ssh/host-unknown', 'invalid alias port fails without explicit override')
    equal((await call('probe', { host })).ok, true, 'explicit valid port overrides invalid alias port')
    const overrideArgs = (await readFile(join(root, 'ssh-args.txt'), 'utf8')).trim().split('\n')
    equal(overrideArgs[overrideArgs.indexOf('-p') + 1], '2222', 'explicit port remains authoritative')
  }
  await rm(join(home, '.ssh', 'config'))
  check(args.includes('IdentityAgent=none') && args.includes('IdentitiesOnly=yes'), 'agent identities disabled')
  result = await call('identityUpload', { scope: 'host', host, privateKey: keyA })
  equal(result.value.host.fingerprint, fingerprintA, 'replacement changes fingerprint')
  for (const privateKey of ['', 'SECRET INVALID KEY', encrypted, 'x'.repeat(65537), '密'.repeat(22000), null, 42]) {
    result = await call('identityUpload', { scope: 'host', host, privateKey })
    equal(result.ok, false, 'invalid, encrypted or oversized keys rejected')
    equal(result.error.code, 'ssh/invalid-key', 'stable validation failure')
    check(!JSON.stringify(result).includes('SECRET INVALID KEY'), 'private key not echoed')
    equal(await readFile(hostPath, 'utf8'), keyA, 'failed upload preserves previous key')
  }
  for (const payload of [{}, { scope: '__proto__' }, { scope: 'host' }, { scope: 'host', host: '../escape' }]) {
    equal((await call('identityDelete', payload)).ok, false, 'invalid mutation inputs rejected')
  }
  equal((await call('identityStatus', { host: 'bob@example.test:2222' })).value.host.configured, false, 'user isolates key')
  equal((await call('identityStatus', { host: 'alice@example.test:2223' })).value.host.configured, false, 'port isolates key')
  result = await call('identityDelete', { scope: 'default', host })
  equal(result.value.effective, 'host', 'deleting default preserves host')
  equal((await stat(join(home, '.ssh', 'identity-managed'))).mode & 0o777, 0o600, 'management marker private')
  result = await call('identityUpload', { scope: 'default', host, privateKey: keyA })
  equal(result.value.default.fingerprint, fingerprintA, 'default uploaded with expected fingerprint')
  result = await call('identityUpload', { scope: 'default', host, privateKey: keyB })
  equal(result.value.effective, 'host', 'uploading default preserves host priority')
  check(result.value.default.fingerprint !== fingerprintA, 'default replacement updates fingerprint')
  result = await call('identityDelete', { scope: 'host', host })
  equal(result.value.effective, 'default', 'deleting host falls back to default')
  equal(await readFile(join(home, '.ssh', 'id_rsa'), 'utf8'), keyB, 'host delete preserves default bytes')
  await call('identityDelete', { scope: 'default' })
  deepEqual((await call('identityStatus', {})).value, { default: absent, host: absent, effective: 'none' }, 'empty status exact contract')
  for (const endpoint of ['probe', 'list', 'mkdir', 'open']) {
    equal((await call(endpoint, { host, path: '/srv', remote: '/srv', name: 'new' })).error.code, 'ssh/auth-failed', 'no identity fails closed')
  }
  const outside = join(root, 'outside-key')
  await writeFile(outside, keyA)
  await symlink(outside, join(home, '.ssh', 'id_rsa'))
  for (const endpoint of ['identityStatus', 'identityUpload', 'identityDelete']) {
    equal((await call(endpoint, { scope: 'default', privateKey: keyB })).ok, false, 'key symlink refused')
    equal(await readFile(outside, 'utf8'), keyA, 'symlink destination unchanged')
  }
  await rm(join(home, '.ssh', 'id_rsa'))
  await symlink(outside, hostPath)
  equal((await call('identityDelete', { scope: 'host', host })).ok, false, 'host key symlink deletion refused')
  equal((await call('probe', { host })).ok, false, 'unsafe dedicated key fails closed')
  await rm(hostPath)
  await rm(join(home, '.ssh', 'host_keys'), { recursive: true })
  await symlink(root, join(home, '.ssh', 'host_keys'))
  equal((await call('identityUpload', { scope: 'host', host, privateKey: keyA })).ok, false, 'host directory symlink refused')
  await rm(join(home, '.ssh', 'host_keys'))
  equal((await readdir(join(home, '.ssh'))).some((name) => name.startsWith('.identity-')), false, 'validation staging files cleaned')
  await rm(join(home, '.ssh'), { recursive: true })
  const outsideDir = join(root, 'outside-dir')
  await mkdir(outsideDir)
  await symlink(outsideDir, join(home, '.ssh'))
  equal((await call('identityUpload', { scope: 'default', privateKey: keyA })).ok, false, '.ssh symlink refused')
  deepEqual(await readdir(outsideDir), [], 'no files written through .ssh symlink')
  await rm(join(home, '.ssh'))
  // A second tenant HOME never inherits this account's identity.
  await call('identityUpload', { scope: 'default', privateKey: keyA })
  process.env.HOME = join(root, 'second-home')
  await mkdir(process.env.HOME)
  plugin.apply(applyCtx, {})
  deepEqual((await call('identityStatus', { host })).value, { default: absent, host: absent, effective: 'none' }, 'account identities isolated')
  equal((await call('probe', { host })).error.code, 'ssh/auth-failed', 'second account cannot use first account key')

  console.log(`ssh-workspace: ${assertions} assertions passed`)
} finally {
  await rm(root, { recursive: true, force: true })
}
