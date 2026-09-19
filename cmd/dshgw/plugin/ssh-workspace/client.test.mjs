// Structural tests for the ssh-workspace browser half (M64).
//
// The bundle is hand-written (this installation ships no bundler), so what needs pinning is
// the contract: the module id must be the package name, both slots must be list slots filled
// by an additive registration, and the dialog must render without a DOM. A minimal fake React
// and fake slot registry do that without a browser; the real rendering is exercised by the
// operator's manual acceptance run.
import { strict as assert } from 'node:assert'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import vm from 'node:vm'

let assertions = 0
const check = (condition, message) => {
  assertions += 1
  assert.ok(condition, message)
}
const equal = (actual, expected, message) => {
  assertions += 1
  assert.equal(actual, expected, message)
}
// Compared through JSON: the bundle runs in its own vm realm, so its arrays carry that
// realm's prototypes and a strict deep-equal would fail on identity rather than content.
const deepEqual = (actual, expected, message) => {
  assertions += 1
  assert.equal(JSON.stringify(actual), JSON.stringify(expected), message)
}

// A DOM just real enough for the stylesheet helper and the interval handle, plus the module
// loader facade the page provides. The bundle is evaluated as a script in its own context,
// which is what a browser does: the file is neither CJS nor ESM by itself, and node's module
// detection (this package is type: module) would refuse to guess.
const styles = []
const sandboxWindow = {
  confirm: () => true,
  setInterval: () => 1,
  clearInterval: () => {},
  __ModuleLoader__: { load: (bundle) => { registration = bundle } },
}
const sandboxDocument = {
  querySelector: () => null,
  createElement: () => ({ dataset: {}, style: {}, textContent: '' }),
  head: { appendChild: (tag) => styles.push(tag) },
}

// The real module system hands a plugin its peers through require(); only react is needed.
const fakeReact = {
  createElement: (type, props, ...children) => ({ type, props: props ?? {}, children }),
}

let registration = null
const bundlePath = process.env.DSHGW_SSH_CLIENT || join(process.cwd(), 'cmd/dshgw/plugin/ssh-workspace/client.js')
{
  const sandbox = { window: sandboxWindow, document: sandboxDocument, console, setTimeout, clearTimeout, Math, JSON, Date }
  sandbox.globalThis = sandbox
  vm.createContext(sandbox)
  vm.runInContext(await readFile(bundlePath, 'utf8'), sandbox, { filename: bundlePath })
}

check(registration !== null, 'the bundle registers itself with the module loader')
equal(registration.id, 'dshgw-ssh-workspace', 'the module id is the package name the host serves')
const exportsObject = registration.factory((specifier) => {
  if (specifier === 'react') return fakeReact
  throw new Error(`unexpected require(${specifier})`)
})
equal(typeof exportsObject.apply, 'function', 'the bundle exports apply')
deepEqual(exportsObject.inject, ['slots', 'connection', 'remote.workspace', 'uiWorkspace'], 'the bundle declares the services it needs')

// ── apply() against a fake client context ────────────────────────────────────────────────
const registered = []
const calls = []
const fakeCtx = {
  slots: {
    inject: (slot, callback) => {
      registered.push({ slot, component: null })
      callback()
      return () => {}
    },
    register: (options, component) => {
      const entry = registered.find((item) => item.slot === options.name && item.component === null)
      if (entry === undefined) registered.push({ slot: options.name, component, options })
      else {
        entry.component = component
        entry.options = options
      }
      return () => {}
    },
  },
  connection: {
    rpc: {
      call: async (channel, endpoint, payload) => {
        calls.push({ channel, endpoint, payload })
        if (endpoint === 'hosts') {
          return {
            ok: true,
            value: { aliases: [{ name: 'gpt001' }], allowList: [], mountSubdir: 'ssh', home: '/w/dsh-a', sshConfig: true, identity: true },
          }
        }
        if (endpoint === 'identityStatus') return { ok: true, value: { default: { configured: false }, host: { configured: false }, effective: 'none' } }
        if (endpoint === 'identityUpload' || endpoint === 'identityDelete') return { ok: true, value: { default: { configured: false }, host: { configured: false }, effective: 'none' } }
        if (endpoint === 'mounts') return { ok: true, value: { mounts: [], replies: [], mountSubdir: 'ssh', mirror: false } }
        if (endpoint === 'probe') return { ok: true, value: { host: payload.host, home: '/home/remote' } }
        if (endpoint === 'list') return { ok: true, value: { path: payload.path, entries: [], truncated: false } }
        if (endpoint === 'open') return { ok: true, value: { id: 'open-1', host: payload.host, remote: payload.remote, mountpoint: '/w/dsh-a/ssh/gpt001/opt', pending: true } }
        return { ok: true, value: {} }
      },
    },
  },
  remote: {
    workspace: {
      create: async (payload) => {
        calls.push({ endpoint: 'workspace/create', payload })
        return { ok: true, value: { workspace: { workspaceId: 'ws-1', path: payload.path, title: 'opt' }, created: true } }
      },
      rename: async (payload) => {
        calls.push({ endpoint: 'workspace/rename', payload })
        return { ok: true, value: { workspace: { workspaceId: payload.workspaceId, path: '', title: payload.title } } }
      },
    },
  },
  uiWorkspace: { connectWorkspace: async (id) => { calls.push({ endpoint: 'connect', payload: id }) } },
  effect: (fn) => fn(),
  logger: { info: () => {}, warn: () => {} },
}

exportsObject.apply(fakeCtx, {})

const slots = registered.map((entry) => entry.slot)
deepEqual(slots, ['sidebar.footer.action', 'shell.overlay'], 'both surfaces land in list slots and none is shadowed')
for (const entry of registered) {
  equal(typeof entry.component, 'function', `${entry.slot} registered a component`)
  equal(entry.options.id, entry.slot === 'shell.overlay' ? 'ssh-workspace-dialog' : 'ssh-workspace', `${entry.slot} carries a fresh id`)
}
check(styles.length === 1, 'the stylesheet is installed once')
check(String(styles[0].dataset.pluginCss).startsWith('dshgw-ssh-workspace'), 'the stylesheet is namespaced by plugin')
// The shell renders `sidebar.footer.action` as one flex row, which would squeeze this row
// and the browser-workspace one into half the foot each; both plugins ship the rule that
// stacks the container instead. The list slot wraps every registration in a classless div,
// so the shell's container is two levels up from the row. The browser row is matched by its
// CONTAINER class only: an earlier rule also matched the row's inner action button, which
// turned that container into a column and put its folder icon under its label.
check(/div:has\(> div > \.dshgw-bw-row\)[\s\S]*div:has\(> div > \.dshgw-ssh-action\)[\s\S]*flex-direction: column/.test(String(styles[0].textContent)),
  'the stylesheet stacks the shared sidebar foot')
check(!/> \.dshgw-bw-action/.test(String(styles[0].textContent)),
  'the rule must not match the browser row container itself')

// The dialog is closed until the action is used: rendering it must be a no-op, not a crash.
const dialog = registered.find((entry) => entry.slot === 'shell.overlay').component
equal(dialog(), null, 'the dialog renders nothing while closed')

// The action opens it and pulls the account's hosts and mounts.
const action = registered.find((entry) => entry.slot === 'sidebar.footer.action').component
const element = action()
equal(element.type, 'button', 'the sidebar entry is a button')
equal(element.props.className, 'dshgw-ssh-action', 'the expanded sidebar shows the full row')
// The collapsed rail is one icon column, where the label cannot fit.
equal(action({ wide: false }).props.className, 'dshgw-ssh-action dshgw-ssh-action-rail', 'the rail drops the label')
await element.props.onClick()
deepEqual(calls.map((call) => call.endpoint), ['hosts', 'identityStatus', 'mounts'], 'opening the dialog loads hosts and mounts')
check(calls.every((call) => call.channel === '/ssh-workspace'), 'every host call uses the plugin channel, not /api')

const dialogElement = dialog()
check(dialogElement !== null, 'the dialog renders once open')
equal(dialogElement.props.role, 'dialog', 'the dialog is a modal dialog')
equal(dialogElement.props.style.pointerEvents, 'auto', 'the overlay entry opts back into pointer events')

// Rendering once more exercises the whole tree with the loaded state: it must stay a tree.
const body = dialogElement.children[0]
const sections = Array.isArray(body.children[0]) ? body.children[0] : body.children
check(sections.length > 3, 'the dialog renders its sections')
check(sections.some((node) => node && node.type === 'h2'), 'the dialog has a heading')
check(sections.some((node) => node && node.type === 'div' && node.props.className === 'dshgw-ssh-footer'), 'the dialog has a footer')
const serialized = JSON.stringify(dialogElement, (key, value) => (typeof value === 'function' ? '[fn]' : value))
check(serialized.includes('SSH 工作区'), 'the dialog carries its title')
check(serialized.includes('已挂载的远端目录'), 'the dialog lists mounts')
check(serialized.includes('gpt001'), 'the host suggestion from the account config is offered')
check(serialized.includes('用户名') && serialized.includes('端口'), 'the dialog exposes independent SSH username and port fields')
check(serialized.includes('SSH 私钥') && serialized.includes('当前生效'), 'the dialog exposes identity scope and effective status')
check(serialized.includes('上传/替换') && serialized.includes('删除'), 'the dialog exposes identity replacement and deletion actions')
check(calls.some((call) => call.endpoint === 'identityStatus'), 'opening the dialog refreshes identity status')


// ── identity interactions and validation ────────────────────────────────────────────────
const findNodes = (node, predicate, out = []) => {
  if (!node) return out
  if (Array.isArray(node)) { for (const child of node) findNodes(child, predicate, out); return out }
  if (predicate(node)) out.push(node)
  for (const child of (node.children || [])) findNodes(child, predicate, out)
  return out
}
const renderDialog = () => dialog()
const byKey = (key) => findNodes(renderDialog(), (node) => node?.props?.key === key)[0]
const hostInput = () => findNodes(renderDialog(), (node) => node?.props?.list === 'dshgw-ssh-hosts')[0]
const identityFile = () => byKey('key')
const identityUploadButton = () => byKey('upload')
const identityDeleteButton = () => byKey('delete')

// A host-specific status is returned and must be sent as the complete binding on default upload.
const statusCalls = []
const originalRpc = fakeCtx.connection.rpc.call
fakeCtx.connection.rpc.call = async (channel, endpoint, payload) => {
  if (endpoint === 'identityStatus') {
    statusCalls.push(payload)
    return { ok: true, value: { default: { configured: true, fingerprint: 'SHA256/default' }, host: { configured: true, fingerprint: 'SHA256/host' }, effective: 'host' } }
  }
  if (endpoint === 'identityUpload' || endpoint === 'identityDelete') {
    calls.push({ endpoint, payload })
    return { ok: true, value: { default: { configured: true }, host: { configured: true }, effective: 'host' } }
  }
  return originalRpc(channel, endpoint, payload)
}
// Re-open to render current controls after swapping the RPC stub.
await element.props.onClick()
let host = hostInput()
check(host && typeof host.props.onChange === 'function', 'host input has a real change handler')
host.props.onChange({ target: { value: 'alice@example.com' } })
await new Promise((resolve) => setTimeout(resolve, 0))
equal(statusCalls.at(-1).host, 'alice@example.com', 'identity status uses composed host after host change')
const port = byKey('port')
port.props.onChange({ target: { value: '2222' } })
await new Promise((resolve) => setTimeout(resolve, 0))
equal(statusCalls.at(-1).host, 'alice@example.com:2222', 'identity status includes independent port')
const keyText = '-----BEGIN OPENSSH PRIVATE KEY-----\nkey\n-----END OPENSSH PRIVATE KEY-----'
await identityFile().props.onChange({ target: { files: [{ size: keyText.length, text: async () => keyText }] } })
await identityUploadButton().props.onClick()
const upload = calls.findLast((call) => call.endpoint === 'identityUpload')
check(upload && upload.payload.host === 'alice@example.com:2222', 'default upload binds the current complete host')

// Oversize replacement clears the previous selection and does not issue RPC.
const beforeOversize = calls.filter((call) => call.endpoint === 'identityUpload').length
await identityFile().props.onChange({ target: { files: [{ size: 64 * 1024 + 1, text: async () => 'stale' }] } })
equal(identityUploadButton().props.disabled, true, 'oversize file cannot be uploaded with stale key')
equal(calls.filter((call) => call.endpoint === 'identityUpload').length, beforeOversize, 'oversize file does not issue upload RPC')

// Cancelled replacement/deletion must not issue RPC.
sandboxWindow.confirm = () => false
await identityFile().props.onChange({ target: { files: [{ size: 3, text: async () => 'key' }] } })
await identityUploadButton().props.onClick()
await identityDeleteButton().props.onClick()
equal(calls.filter((call) => call.endpoint === 'identityUpload').length, beforeOversize, 'cancelled replacement does not issue upload RPC')
check(!calls.some((call) => call.endpoint === 'identityDelete'), 'cancelled deletion does not issue delete RPC')
sandboxWindow.confirm = () => true

// Invalid ports are rejected locally and never reach the gateway.
const invalidBefore = calls.length
port.props.onChange({ target: { value: '65536' } })
await byKey('probe').props.onClick()
equal(calls.length, invalidBefore, 'invalid port does not issue probe RPC')

// No host permits default identity operations and omits host from the payload.
host.props.onChange({ target: { value: '' } })
port.props.onChange({ target: { value: '' } })
await new Promise((resolve) => setTimeout(resolve, 0))
await identityFile().props.onChange({ target: { files: [{ size: 3, text: async () => 'key' }] } })
await identityUploadButton().props.onClick()
const noHostUpload = calls.findLast((call) => call.endpoint === 'identityUpload')
check(noHostUpload && !Object.hasOwn(noHostUpload.payload, 'host'), 'default upload without host omits host')
// Out-of-order status responses cannot overwrite the newest host.
const pendingStatus = []
fakeCtx.connection.rpc.call = async (channel, endpoint, payload) => {
  if (endpoint === 'identityStatus') return new Promise((resolve) => pendingStatus.push({ resolve, payload }))
  return originalRpc(channel, endpoint, payload)
}
host.props.onChange({ target: { value: 'first.example' } })
host.props.onChange({ target: { value: 'second.example' } })
check(pendingStatus.length >= 2, 'host changes issue independent status requests')
pendingStatus.at(-1).resolve({ ok: true, value: { default: { configured: false }, host: { configured: true, fingerprint: 'NEW' }, effective: 'host' } })
await new Promise((resolve) => setTimeout(resolve, 0))
pendingStatus[0].resolve({ ok: true, value: { default: { configured: true, fingerprint: 'OLD' }, host: { configured: false }, effective: 'default' } })
await new Promise((resolve) => setTimeout(resolve, 0))
check(JSON.stringify(renderDialog()).includes('NEW') && !JSON.stringify(renderDialog()).includes('OLD'), 'stale status response cannot overwrite newest host')


console.log(`ssh-workspace client: ${assertions} assertions passed`)
