// Tenant-side half of the ssh-workspace feature (M64).
//
// This plugin runs inside the account's own dsh, which runs inside that account's bubblewrap
// sandbox. It does everything an ssh client can do there — list a remote directory, create
// one, ask where the remote home is — using the account's own key, because the sandbox has
// ssh (the release's /usr is bound read-only), a shared network namespace, and HOME pointing
// at the account workspace, so ~/.ssh/id_rsa is the account's own key.
//
// It does NOT mount. A tenant worker cannot: the profile has no /dev/fuse, and the host's
// root uid is unmapped inside its user namespace, so the setuid fusermount3 helper cannot
// work either (measured; see docs/design/m64-ssh-workspace.md §3). Mounting is the gateway's
// half, and the two halves talk through a mailbox inside this account's DSH home:
//
//   <DSH_HOME>/ssh-requests/<id>.json   this plugin writes, the gateway consumes
//   <DSH_HOME>/ssh-replies/<id>.json    the gateway writes, this plugin reads
//
// A file is the whole control channel on purpose: each side can already write only its own
// directory, so there is no new listening port, no token and nothing to authenticate.
//
// The browser half (client.js) calls these endpoints over the harness's own authenticated
// RPC channel (ctx.connection.rpc.handle), which is available to any plugin and needs no
// generated code.

import { execFile } from 'node:child_process'
import { mkdir, mkdtemp, readFile, readdir, rename, rm, stat, writeFile } from 'node:fs/promises'
import { homedir } from 'node:os'
import { join, relative, resolve, sep } from 'node:path'

/** Stable cordis plugin name. */
export const name = 'ssh-workspace'

/** Required service: the authenticated browser RPC carrier. */
export const inject = ['connection']

const RPC_CHANNEL = '/ssh-workspace'
const REQUEST_ID = /^[A-Za-z0-9_-]{1,64}$/
const HOST_SPEC = /^[A-Za-z0-9._@][A-Za-z0-9._@:-]{0,254}$/
const MAX_ENTRIES_DEFAULT = 1000
const CONNECT_TIMEOUT_DEFAULT_MS = 10000
const SSH_OUTPUT_LIMIT = 4 * 1024 * 1024

/** Business failure with a stable code, mirrored by the gateway's own codes. */
class Failure extends Error {
  constructor(code, message) {
    super(message)
    this.code = code
  }
}

const failure = (code, message) => new Failure(code, message)
const messageOf = (error) => (error instanceof Error ? error.message : String(error))

/** Plugin config, validated once at activation. */
function readConfig(raw) {
  const config = raw && typeof raw === 'object' ? raw : {}
  const mountSubdir = typeof config.mountSubdir === 'string' && config.mountSubdir !== '' ? config.mountSubdir : 'ssh'
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(mountSubdir)) {
    throw new Error(`ssh-workspace: mountSubdir ${JSON.stringify(mountSubdir)} is not one visible path segment`)
  }
  const hosts = Array.isArray(config.hosts) ? config.hosts.filter((host) => typeof host === 'string' && host.trim() !== '') : []
  for (const host of hosts) {
    if (!HOST_SPEC.test(host.trim())) throw new Error(`ssh-workspace: host ${JSON.stringify(host)} is not an ssh alias or user@host`)
  }
  const maxEntries = Number.isInteger(config.maxEntries) && config.maxEntries > 0 ? config.maxEntries : MAX_ENTRIES_DEFAULT
  const connectTimeoutMs = Number.isInteger(config.connectTimeoutMs) && config.connectTimeoutMs > 0
    ? config.connectTimeoutMs
    : CONNECT_TIMEOUT_DEFAULT_MS
  return { mountSubdir, hosts, maxEntries, connectTimeoutMs }
}

/** One POSIX shell word, so nothing a person types can become a second command. */
export function shellQuote(value) {
  return `'${String(value).split("'").join(`'\\''`)}'`
}

/** Reject anything that must never reach an ssh command line. */
export function checkHostSpec(spec) {
  if (typeof spec !== 'string' || !HOST_SPEC.test(spec) || spec.startsWith('-')) {
    throw failure('ssh/host-unknown', `${JSON.stringify(spec)} is not an ssh alias or user@host`)
  }
  return spec
}

/** Reject a remote path that is not a plain absolute path. */
export function checkRemotePath(value) {
  if (typeof value !== 'string' || value === '' || value.length > 4096) {
    throw failure('ssh/invalid-path', 'remote path must be a non-empty string')
  }
  if (!value.startsWith('/')) throw failure('ssh/invalid-path', `remote path ${JSON.stringify(value)} is not absolute`)
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(value)) throw failure('ssh/invalid-path', 'remote path contains a control character')
  if (value.split('/').includes('..')) throw failure('ssh/invalid-path', `remote path ${JSON.stringify(value)} must not contain ".."`)
  const normal = resolve(value)
  if (normal !== value) throw failure('ssh/invalid-path', `remote path ${JSON.stringify(value)} is not in normal form`)
  return value
}

/** One path segment a person asked to create. */
export function checkSegment(name) {
  if (typeof name !== 'string' || name === '' || name.length > 255) {
    throw failure('ssh/invalid-path', 'name must be 1..255 bytes')
  }
  if (name === '.' || name === '..' || /[/\\]/.test(name)) {
    throw failure('ssh/invalid-path', `${JSON.stringify(name)} is not a single path segment`)
  }
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(name)) throw failure('ssh/invalid-path', 'name contains a control character')
  return name
}

/** Parse the concrete aliases of one ssh config document. */
export function parseSSHConfig(text) {
  const hosts = []
  const seen = new Set()
  let current = null
  let candidate = null
  for (const raw of String(text).split('\n')) {
    const line = raw.trim()
    if (line === '' || line.startsWith('#')) continue
    const match = /^(\S+)[\s=]+(.+)$/.exec(line)
    if (match === null) continue
    const key = match[1].toLowerCase()
    let value = match[2].trim()
    if (key === 'host') {
      current = null
      candidate = null
      for (const pattern of value.split(/\s+/)) {
        if (pattern === '' || /[*?!]/.test(pattern) || seen.has(pattern)) continue
        seen.add(pattern)
        current = { name: pattern, hostName: '', user: '', port: 0 }
        hosts.push(current)
        break
      }
      continue
    }
    if (current === null) continue
    if (key === 'hostname' || key === 'user' || key === 'port') {
      const cut = value.search(/\s#/)
      if (cut >= 0) value = value.slice(0, cut).trim()
      if (key === 'hostname') current.hostName = value
      else if (key === 'user') current.user = value
      else if (/^\d+$/.test(value)) current.port = Number(value)
    }
    candidate = null
  }
  return hosts
}

/** Map one remote directory onto the account's own workspace, exactly like the gateway. */
export function mountpointFor(home, mountSubdir, host, remote) {
  checkHostSpec(host)
  checkRemotePath(remote)
  const mountpoint = join(home, mountSubdir, host, ...remote.split('/').filter((segment) => segment !== ''))
  const inside = relative(home, mountpoint)
  if (inside === '' || inside.startsWith(`..${sep}`) || inside === '..') {
    throw failure('mount/forbidden', `${mountpoint} would leave the account workspace`)
  }
  return mountpoint
}

/** Rebuild the remote path from a path inside the account's mount container. */
export function remoteFromMountpoint(home, mountSubdir, mountpoint) {
  const root = join(home, mountSubdir)
  const inside = relative(root, mountpoint)
  if (inside === '' || inside.startsWith(`..${sep}`) || inside === '..') return null
  const segments = inside.split(sep)
  const host = segments.shift()
  return { host, remote: `/${segments.join('/')}` }
}

/** Run one command and return its streams; never rejects on a non-zero exit. */
export function runCommand(command, args, options = {}) {
  return new Promise((resolvePromise) => {
    execFile(command, args, {
      timeout: options.timeoutMs ?? CONNECT_TIMEOUT_DEFAULT_MS,
      maxBuffer: SSH_OUTPUT_LIMIT,
      encoding: 'utf8',
      killSignal: 'SIGKILL',
    }, (error, stdout, stderr) => {
      resolvePromise({ stdout: stdout ?? '', stderr: stderr ?? '', error: error ?? null })
    })
  })
}

/** Classify an ssh failure the way the gateway does, by message. */
function classifySSH(result) {
  if (result.error === null) return null
  const text = String(result.stderr).toLowerCase()
  if (text.includes('permission denied') || text.includes('publickey') || text.includes('host key verification failed')) {
    return failure('ssh/auth-failed', `ssh authentication failed: ${result.stderr.trim()}`)
  }
  if (text.includes('could not resolve hostname')) {
    return failure('ssh/host-unknown', `ssh cannot resolve the host: ${result.stderr.trim()}`)
  }
  if (/connection (refused|timed out|closed)|no route to host|network is unreachable/.test(text)) {
    return failure('ssh/unreachable', `ssh cannot reach the host: ${result.stderr.trim()}`)
  }
  if (result.error.killed === true || result.error.signal === 'SIGKILL') {
    return failure('ssh/unreachable', 'ssh timed out')
  }
  return failure('ssh/command-failed', `ssh failed: ${result.stderr.trim() || messageOf(result.error)}`)
}

/** A `{ok:true,value}` / `{ok:false,error}` envelope, the shape the RPC carrier relays. */
const ok = (value) => ({ ok: true, value })
const failed = (error) => ({
  ok: false,
  error: {
    code: error instanceof Failure ? error.code : 'ssh/command-failed',
    message: messageOf(error),
    details: {},
  },
})

/** The plugin body. */
export function apply(ctx, rawConfig) {
  const config = readConfig(rawConfig)
  const home = homedir()
  const dshHome = process.env.DSH_HOME || join(home, '.dsh')
  const sshDir = join(home, '.ssh')
  const sshConfigPath = join(sshDir, 'config')
  const sshKeyPath = join(sshDir, 'id_rsa')
  const knownHostsPath = join(sshDir, 'known_hosts')
  const mountRoot = join(home, config.mountSubdir)
  const requestDir = join(dshHome, 'ssh-requests')
  const replyDir = join(dshHome, 'ssh-replies')

  const sshArgs = (host, script) => {
    const args = ['-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=accept-new', '-o', `UserKnownHostsFile=${knownHostsPath}`]
    args.push('-F', sshConfigPath)
    args.push('-i', sshKeyPath)
    args.push('-o', `ConnectTimeout=${Math.ceil(config.connectTimeoutMs / 1000)}`)
    return [...args, '--', host, 'sh', '-c', script]
  }

  const permits = (host) => {
    checkHostSpec(host)
    if (config.hosts.length > 0 && !config.hosts.some((allowed) => allowed.trim() === host)) {
      throw failure('ssh/host-unknown', `${host} is not in this deployment's host list`)
    }
    return host
  }

  const ssh = async (host, script) => {
    permits(host)
    const result = await runCommand('ssh', sshArgs(host, script), { timeoutMs: config.connectTimeoutMs + 20000 })
    const error = classifySSH(result)
    if (error !== null) throw error
    return result.stdout
  }

  const exists = async (path) => {
    try {
      await stat(path)
      return true
    } catch {
      return false
    }
  }

  // The alias list is the account's own ~/.ssh/config (its HOME is its workspace, so this
  // file belongs to the account and to nobody else).
  const readAliases = async () => {
    try {
      return parseSSHConfig(await readFile(sshConfigPath, 'utf8'))
    } catch {
      return []
    }
  }

  const writeMail = async (dir, id, payload) => {
    await mkdir(dir, { recursive: true, mode: 0o700 })
    const temporary = await mkdtemp(join(dir, '.tmp-'))
    const staging = join(temporary, `${id}.json`)
    await writeFile(staging, `${JSON.stringify(payload, null, 2)}\n`, { mode: 0o600 })
    await rename(staging, join(dir, `${id}.json`))
    await rm(temporary, { recursive: true, force: true })
  }

  const readMailbox = async (dir) => {
    let names = []
    try {
      names = await readdir(dir)
    } catch {
      return []
    }
    const entries = []
    for (const entry of names) {
      if (!entry.endsWith('.json')) continue
      try {
        entries.push(JSON.parse(await readFile(join(dir, entry), 'utf8')))
      } catch {
        // A half-written or foreign file is ignored: the mailbox must never wedge on one.
      }
    }
    return entries
  }

  // The account's own record of its mounts, written by the gateway next to this account's
  // DSH home. It is the only way to tell a real mount from a parent directory: the mirror
  // layout nests (ssh/<host>/<remote path>), so every level looks like a directory from in
  // here, and mounting is kernel state this sandbox cannot observe.
  const mirrorPath = join(dshHome, 'ssh-mounts.json')
  const readMirror = async () => {
    let document
    try {
      document = JSON.parse(await readFile(mirrorPath, 'utf8'))
    } catch {
      return []
    }
    const rows = Array.isArray(document?.mounts) ? document.mounts : []
    return rows
      .filter((row) => row && typeof row.mountpoint === 'string')
      .map((row) => ({
        host: String(row.host ?? ''),
        remote: String(row.canonical_remote || row.remote || ''),
        requested: String(row.remote ?? ''),
        path: row.mountpoint,
      }))
  }

  const handlers = {
    async hosts() {
      return {
        aliases: await readAliases(),
        allowList: config.hosts,
        mountSubdir: config.mountSubdir,
        mountRoot,
        home,
        sshConfig: await exists(sshConfigPath),
        identity: await exists(sshKeyPath),
      }
    },
    async probe(payload) {
      const host = permits(String(payload?.host ?? ''))
      const output = await ssh(host, 'printf %s "$HOME"')
      const remoteHome = output.trim()
      if (!remoteHome.startsWith('/')) throw failure('ssh/command-failed', 'the remote host reported no usable home directory')
      return { host, home: remoteHome }
    },
    async list(payload) {
      const host = permits(String(payload?.host ?? ''))
      const path = checkRemotePath(String(payload?.path ?? ''))
      const output = await ssh(host, `cd ${shellQuote(path)} || exit 3\nLC_ALL=C ls -1ap`)
      const entries = []
      let truncated = false
      for (const line of output.split('\n')) {
        const raw = line.replace(/\r$/, '')
        if (raw === '' || raw === '.' || raw === '..' || !raw.endsWith('/')) continue
        const name = raw.slice(0, -1)
        if (name === '') continue
        if (entries.length >= config.maxEntries) {
          truncated = true
          break
        }
        entries.push({ name, path: join(path, name), hidden: name.startsWith('.') })
      }
      return { path, entries, truncated }
    },
    async mkdir(payload) {
      const host = permits(String(payload?.host ?? ''))
      const parent = checkRemotePath(String(payload?.path ?? ''))
      const segment = checkSegment(String(payload?.name ?? ''))
      const target = join(parent, segment)
      try {
        await ssh(host, `mkdir -- ${shellQuote(target)}`)
      } catch (error) {
        if (error instanceof Failure && /file exists/i.test(error.message)) {
          throw failure('ssh/mkdir-exists', `${target} already exists on the remote host`)
        }
        if (error instanceof Failure && error.code === 'ssh/command-failed') {
          throw failure('ssh/mkdir-failed', `cannot create ${target} on the remote host: ${error.message}`)
        }
        throw error
      }
      return { path: target }
    },
    async open(payload) {
      const host = permits(String(payload?.host ?? ''))
      const remote = checkRemotePath(String(payload?.remote ?? ''))
      if (!(await exists(sshKeyPath))) {
        throw failure('ssh/auth-failed', `no ssh identity at ${sshKeyPath}: ask the operator to provision one`)
      }
      // The remote directory is checked here, where the account's key lives, so the mailbox
      // request the gateway receives is one that can actually succeed.
      await ssh(host, `cd ${shellQuote(remote)} || exit 3\npwd -P`)
      const id = `open-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`
      const mountpoint = mountpointFor(home, config.mountSubdir, host, remote)
      await writeMail(requestDir, id, { id, op: 'open', host, remote, createdAt: new Date().toISOString() })
      return { id, host, remote, mountpoint, pending: true }
    },
    async close(payload) {
      const mountpoint = String(payload?.mountpoint ?? '')
      const derived = remoteFromMountpoint(home, config.mountSubdir, mountpoint)
      if (derived === null) throw failure('mount/forbidden', `${mountpoint} is not inside this account's mount container`)
      const id = `close-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`
      await writeMail(requestDir, id, {
        id,
        op: 'close',
        host: derived.host,
        remote: derived.remote,
        mountpoint,
        createdAt: new Date().toISOString(),
      })
      return { id, mountpoint, pending: true }
    },
    async mounts() {
      return {
        mounts: await readMirror(),
        mirror: await exists(mirrorPath),
        replies: await readMailbox(replyDir),
        mountSubdir: config.mountSubdir,
      }
    },
    async ack(payload) {
      const id = String(payload?.id ?? '')
      if (!REQUEST_ID.test(id)) throw failure('ssh/invalid-path', `${JSON.stringify(id)} is not a request id`)
      await rm(join(replyDir, `${id}.json`), { force: true })
      return { acknowledged: id }
    },
  }

  ctx.effect(() => ctx.connection.rpc.handle(RPC_CHANNEL, async (endpoint, payload) => {
    const handler = handlers[endpoint]
    if (handler === undefined) return failed(failure('ssh/invalid-path', `unknown endpoint ${JSON.stringify(endpoint)}`))
    try {
      return ok(await handler(payload))
    } catch (error) {
      if (!(error instanceof Failure)) {
        ctx.logger?.warn?.(`ssh-workspace ${endpoint} failed: ${messageOf(error)}`)
      }
      return failed(error)
    }
  }), 'ssh-workspace: tenant ssh RPC channel')

  ctx.logger?.info?.(`ssh-workspace ready: mount container ${mountRoot}, identities ${sshKeyPath}`)
}

export default { name, inject, apply }
