// Structural and behavioural tests for the account-card browser half (M67).
//
// The bundle is hand-written (this installation ships no bundler), so what needs pinning is
// the contract with the shell and with the gateway: the module id must be the package name,
// the row must land in the sidebar's footer list, it must stay invisible where the gateway
// has no identity surface (a plain dsh, or account_card off), it must show the name the
// gateway answered with, and 退出 must be a POST to this tenant's own origin followed by a
// navigation to the portal — not a link, and not a request somewhere else.
//
// A minimal fake React, a fake slot registry and a fake fetch do that without a browser; the
// real rendering is exercised by the operator's acceptance run.
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

const bundlePath = process.env.DSHGW_ACCOUNT_CARD_CLIENT || join(process.cwd(), 'cmd/dshgw/plugin/account-card/client.js')
const source = await readFile(bundlePath, 'utf8')
// Some assertions are about the markup the bundle ships rather than about a rendered tree.
const bundleSource = source

/**
 * One page under test.
 *
 * @param options.session   what GET /dshgw/session/ answers: {status, body}
 * @param options.logout    what POST /dshgw/logout/ answers: {status}
 * @param options.fetchFails the first N session reads throw (a transport failure)
 */
function setup (options = {}) {
  const {
    session = { status: 200, body: { ok: true, value: { authenticated: true, tenant: 'dsh-colin', account: '李智超(colin)', feishu_name: '李智超', name: '李智超' } } },
    logout = { status: 303 },
    fetchFails = 0,
    logoutThrows = false,
    sessionAfterLogout = null,
  } = options

  const calls = []
  const events = []
  const warnings = []
  let remainingFailures = fetchFails
  let assigned = null
  let registration = null

  const window = {
    async fetch (url, init = {}) {
      calls.push({ url, method: init.method || 'GET', credentials: init.credentials, redirect: init.redirect })
      if (url === '/dshgw/session/' && remainingFailures > 0) {
        remainingFailures -= 1
        throw new Error('network down')
      }
      // A logout request that the browser cannot complete: the gateway revoked the session and
      // answered 303 to the portal, and following that redirect failed at the network layer.
      if (url === '/dshgw/logout/' && logoutThrows) throw new TypeError('Failed to fetch')
      if (url === '/dshgw/session/' && sessionAfterLogout !== null && calls.some((call) => call.url === '/dshgw/logout/')) {
        return { status: sessionAfterLogout, ok: false, type: 'basic', async json () { return {} } }
      }
      const answer = url === '/dshgw/session/' ? session : logout
      return {
        status: answer.status,
        ok: answer.status >= 200 && answer.status < 300,
        type: answer.type ?? 'basic',
        async json () { return answer.body ?? {} },
      }
    },
    location: { assign (target) { assigned = target; events.push('assign:' + target) } },
    __ModuleLoader__: { load (bundle) { registration = bundle } },
  }
  const styles = []
  const document = {
    querySelector: () => null,
    createElement: () => ({ dataset: {}, textContent: '' }),
    head: { appendChild: (tag) => styles.push(tag) },
  }

  // A React just real enough, with the two behaviours the plugin depends on:
  //
  //   · useSyncExternalStore returns a REFERENTIALLY STABLE snapshot per store version, and
  //     notifies subscribers when it changes. Returning a fresh object every render would
  //     make effects re-run, which is not what React does — an earlier version of this
  //     harness did exactly that and reported failures the plugin does not have.
  //   · useEffect is captured, not run: the shell runs it after the first render, which this
  //     harness does explicitly through `mount()`.
  let storeVersion = 0
  const subscribers = new Set()
  // A store notification invalidates every memoized snapshot, exactly as React noticing a
  // new snapshot value does: the next render reads the state as it is now.
  const notify = () => { storeVersion += 1; snapshots.clear(); for (const listener of [...subscribers]) listener() }
  const fakeReact = {
    createElement: (type, props, ...children) => ({ type, props: props ?? {}, children: children.flat(Infinity) }),
    useEffect: (fn) => { pendingEffects.push(fn) },
    useSyncExternalStore: (subscribe, getSnapshot) => {
      const seen = storeVersion
      if (!snapshots.has(seen)) snapshots.set(seen, getSnapshot())
      subscribe(notify)
      return snapshots.get(seen)
    },
  }

  const pendingEffects = []
  const snapshots = new Map()
  const registered = new Map()
  const ctx = {
    logger: { warn: (message) => warnings.push(message), info: (message) => events.push('info:' + message) },
    slots: {
      inject: (_name, fn) => { fn(); return () => {} },
      register (entry, component) {
        events.push('register:' + entry.id)
        registered.set(entry.name, { options: entry, component })
        return () => {}
      },
    },
    effect (fn) { pendingEffects.push(fn()) },
  }

  const sandbox = { window, document, console, setTimeout, clearTimeout, Promise, Error, JSON, Array, Object, String, Number }
  sandbox.globalThis = sandbox
  vm.createContext(sandbox)
  vm.runInContext(source, sandbox, { filename: bundlePath })

  const exportsObject = registration.factory((specifier) => {
    if (specifier === 'react') return fakeReact
    throw new Error(`unexpected require(${specifier})`)
  })
  exportsObject.apply(ctx)

  const row = () => registered.get('sidebar.footer.action')
  const component = () => row().component

  return {
    /** Render once, then run the effects React would run after that render. */
    mount: (props) => {
      const element = component()(props)
      const effects = pendingEffects.splice(0, pendingEffects.length)
      for (const effect of effects) effect()
      return element
    },
    calls,
    events,
    warnings,
    exportsObject,
    styles,
    registered,
    assigned: () => assigned,
    options: () => row().options,
    // Re-render only, for inspecting a row whose state already settled: this is what the
    // shell does when the store notifies, and it must not run effects again.
    render: (props) => component()(props),
    text: (props) => textOf(component()(props)),
    // Tell the subscribers the store changed, the way the real store does after a patch.
    notify,
  }
}

function walk (node, visit) {
  if (node === null || node === undefined || typeof node !== 'object') return
  if (Array.isArray(node)) { for (const child of node) walk(child, visit); return }
  visit(node)
  walk(node.children, visit)
}

function textOf (node) {
  if (node === null || node === undefined || node === false) return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textOf).join('')
  return textOf(node.children)
}

const findByClass = (node, className) => {
  let found = null
  walk(node, (child) => {
    if (found === null && String(child?.props?.className || '').includes(className)) found = child
  })
  return found
}

const byKey = (node, key) => {
  let found = null
  walk(node, (child) => { if (found === null && child?.props?.key === key) found = child })
  return found
}

// ── the module contract ─────────────────────────────────────────────────────────────────
{
  const page = setup()
  equal(page.exportsObject.inject.length, 1, 'the bundle declares the services it needs')
  equal(page.exportsObject.inject[0], 'slots', 'the row needs only the slot registry')
  equal(page.options().id, 'dshgw-account-card', 'the sidebar registration carries its own id')
  equal(page.options().name, 'sidebar.footer.action', 'the row lands in the sidebar footer list slot')
  equal(page.options().order, 120, 'the row sorts after the workspace actions (90 and 100)')
  check(page.styles.length === 1, 'the stylesheet is installed exactly once')
  equal(page.styles[0].dataset.pluginCss, 'dshgw-account-card/client.css', 'the stylesheet is namespaced by plugin')
  check(/div:has\(> \.dshgw-account-card\), div:has\(> div > \.dshgw-account-card\) \{ flex-direction: column; \}/.test(String(page.styles[0].textContent)),
    'the stylesheet stacks the shared sidebar foot so the row is a line of its own')
  // The button is a flex box, so the glyph and the label sit at its start edge unless it
  // centres them itself: text-align does not reach a flex container's items, and the padding
  // on the right then makes the pair look pushed left.
  const logoutRule = String(page.styles[0].textContent).split('\n').find((line) => line.startsWith('.dshgw-account-logout {'))
  check(/display: inline-flex/.test(logoutRule), 'the logout button lays its glyph and label out as a flex row')
  check(/justify-content: center/.test(logoutRule), 'the logout button centres its icon and text horizontally')
  check(/align-items: center/.test(logoutRule), 'the logout button centres its icon and text vertically')
  // The collapsed rail is one icon column: the shell centres each entry, so the ICON must be
  // the thing being centred. Keeping the row's 8px horizontal padding in that mode centres the
  // padding box instead and pushed this icon 8px right of the ssh and browser icons
  // (measured in a real browser: x=36 against their x=28).
  const css = String(page.styles[0].textContent)
  const railRowRule = css.split('\n').find((line) => line.startsWith('.dshgw-account-rail .dshgw-account-row {'))
  const railButtonRule = css.split('\n').find((line) => line.startsWith('.dshgw-account-rail .dshgw-account-logout {'))
  check(/padding: 6px 0/.test(railRowRule), 'the rail row drops its horizontal padding so the button is what gets centred')
  // Every level of the rail is a flex container, so a row without its own justify-content lays
  // its only child out at flex-start: measured on the running tenant, the button sat at centre
  // 20.50 while the ssh and browser buttons sat at 27.50. Both rules are needed together — the
  // padding above, or centring would line up the padding box and overshoot by 8px.
  check(/justify-content: center/.test(railRowRule), 'and centres the button inside that row')
  check(/padding: 0/.test(railButtonRule), 'the rail button keeps no padding of its own either')
  check(/display: none/.test(css.split('\n').find((line) => line.startsWith('.dshgw-account-rail .dshgw-account-name'))),
    'the rail hides the name so the icon is the row\'s only content')
  // The glyph's INK is not centred in its own advance at the inherited size: measured in a real
  // browser, ⏻ paints [0..11] inside a 13px advance at 14px (−1.00px), which reads as "slightly
  // left" even when the box is dead centre. At 15px the ink fills the advance and the bias is
  // −0.50px — the same as the ssh workspace's ⇄ that the eye compares it against.
  const iconRule = css.split('\n').find((line) => line.startsWith('.dshgw-account-logout-icon {'))
  check(/font-size: 15px/.test(iconRule), 'the power glyph is sized so its ink is centred in its advance')
  check(/line-height: 1/.test(iconRule), 'and its box stays as tall as the glyph, keeping the rail rhythm')
  check(/className: 'dshgw-account-logout-icon'/.test(bundleSource), 'the icon span carries that class')
}

// ── the identity the gateway answered with ──────────────────────────────────────────────
{
  const page = setup()
  // The first render starts the read (the shell mounts the row as soon as the page loads), so
  // the element is inspected before and after the answer arrives.
  check(page.mount({ wide: true }) === null, 'nothing is drawn while the read is in flight')
  await new Promise((resolve) => setImmediate(resolve))
  equal(page.calls.length, 1, 'the row reads the session once per page')
  equal(page.calls[0].url, '/dshgw/session/', 'the read is the gateway route under the tenant origin')
  equal(page.calls[0].method, 'GET', 'the read is a GET')
  equal(page.calls[0].credentials, 'same-origin', 'the session cookie must travel with the read')

  const element = page.render({ wide: true })
  check(element !== null, 'the row renders once the identity is known')
  equal(element.props['data-dshgw-account'], 'dsh-colin', 'the row carries the tenant it describes')
  equal(element.props['data-dshgw-account-state'], 'ready', 'the row reports its own state')
  check(page.text({ wide: true }).includes('李智超'), 'the Feishu name is what the row shows')

  const logoutButton = findByClass(element, 'dshgw-account-logout')
  check(logoutButton !== null && logoutButton.type === 'button', '退出 is a button, not a link')
  equal(textOf(logoutButton).includes('退出'), true, 'the button is labelled 退出')
  await logoutButton.props.onClick()
  await new Promise((resolve) => setImmediate(resolve))
  const post = page.calls.find((call) => call.url === '/dshgw/logout/')
  check(post !== undefined, 'clicking 退出 posts to the gateway route')
  equal(post.method, 'POST', 'the logout is a POST — a GET would be a cross-site image away from signing someone out')
  equal(post.credentials, 'same-origin', 'the logout carries the session cookie')
  equal(post.redirect, 'manual', 'the logout must not follow the gateway\'s redirect to the portal inside fetch')
  equal(page.assigned(), '/', 'the page then goes to the portal login')
}

// ── the collapsed rail ─────────────────────────────────────────────────────────────────
{
  const page = setup()
  page.mount({ wide: false })
  await new Promise((resolve) => setImmediate(resolve))
  check(page.render({ wide: false }).props.className.includes('dshgw-account-rail'), 'the rail variant is marked')
  const logoutButton = findByClass(page.render({ wide: false }), 'dshgw-account-logout')
  equal(logoutButton.props['aria-label'], '退出登录', 'the icon-only button still says what it does')
}

// ── a deployment without the identity surface ──────────────────────────────────────────
{
  const page = setup({ session: { status: 404 } })
  page.mount({ wide: true })
  await new Promise((resolve) => setImmediate(resolve))
  equal(page.render({ wide: true }), null, 'a 404 means no row at all, not a row saying "unavailable"')
  equal(page.warnings.length, 0, 'an expected absence is not a warning')
  // The stylesheet and the registration still exist: the row is simply not drawn.
  check(page.registered.has('sidebar.footer.action'), 'the registration stays in place')
}

// ── a failed read ──────────────────────────────────────────────────────────────────────
{
  const page = setup({ fetchFails: 1 })
  page.mount({ wide: true })
  await new Promise((resolve) => setImmediate(resolve))
  const element = page.render({ wide: true })
  check(element !== null, 'a failed read still renders a row')
  equal(element.props['data-dshgw-account-state'], 'failed', 'the row reports the failure')
  const retry = byKey(element, 'retry')
  check(retry !== null, 'the failed row offers a retry instead of an exit')
  check(page.text({ wide: true }).includes('账号信息读取失败'), 'the reason is shown in the row')
  equal(page.warnings.length, 1, 'the failure is logged once, for the operator')

  await retry.props.onClick()
  await new Promise((resolve) => setImmediate(resolve))
  const healed = page.render({ wide: true })
  equal(healed.props['data-dshgw-account-state'], 'ready', 'a retry that succeeds shows the identity')
  check(page.text({ wide: true }).includes('李智超'), 'and the name comes back')
}

// ── a logout whose redirect the browser cannot follow ────────────────────────────────
// Measured against a real tenant: the gateway revokes the session and answers 303 to the
// portal; when that origin is unreachable from the browser, a redirect-FOLLOWING fetch fails
// with "TypeError: Failed to fetch". The person was signed out and must not be told otherwise.
{
  const page = setup({ logoutThrows: true, sessionAfterLogout: 302 })
  page.mount({ wide: true })
  await new Promise((resolve) => setImmediate(resolve))
  const logoutButton = findByClass(page.render({ wide: true }), 'dshgw-account-logout')
  await logoutButton.props.onClick()
  await new Promise((resolve) => setImmediate(resolve))
  equal(page.assigned(), '/', 'a logout that landed anyway still sends the person to the portal')
  check(!page.text({ wide: true }).includes('退出失败'), 'and it is not reported as a failure')
  const probe = page.calls.filter((call) => call.url === '/dshgw/session/')
  check(probe.length > 1, 'the row asks the gateway who it is before believing the failure')
}

// ── a logout the gateway refused ───────────────────────────────────────────────────────
{
  const page = setup({ logout: { status: 403 } })
  page.mount({ wide: true })
  await new Promise((resolve) => setImmediate(resolve))
  const logoutButton = findByClass(page.render({ wide: true }), 'dshgw-account-logout')
  await logoutButton.props.onClick()
  await new Promise((resolve) => setImmediate(resolve))
  equal(page.assigned(), null, 'a refused logout must not navigate as if it had worked')
  check(page.text({ wide: true }).includes('退出失败'), 'the refusal is reported in the row')
}

console.log(`account-card client: ${assertions} assertions passed`)
