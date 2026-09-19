// Browser half of dshgw's sidebar identity row (M67).
//
// Prebuilt by hand on purpose: this installation ships no bundler, and dsh serves a client
// plugin's ./client file byte for byte. The contract is exactly what any shipped surface
// does — call window.__ModuleLoader__.load with this package's name, and export `apply` plus
// the service names the plugin needs.
//
// The row answers one question — who is signed in — and offers the one action that follows
// from it: 退出. Both live in this gateway, not in the tenant: the names come from aigw (only
// it knows a person's Feishu name) and the logout needs the session store, so the gateway
// serves GET /dshgw/session/ and POST /dshgw/logout/ under the tenant's OWN origin and this
// bundle only renders them. Both are gated by account_card.enabled: where the feature is off
// those paths answer 404 and this row never appears, which is also what a plain dsh (no
// gateway at all) looks like.

window.__ModuleLoader__.load({
  id: 'dshgw-account-card',
  factory: (require) => {
    // The wrapper every shipped bundle uses: the page's loader hands the factory a CommonJS
    // style `require`, and it must return `module.exports`.
    var module = { exports: {} }
    var exports = module.exports
    const React = require('react')
    const h = React.createElement

    /** Required service: the slot registry. The row talks HTTP, not RPC, so nothing else. */
    const inject = ['slots']

    const SESSION_PATH = '/dshgw/session/'
    const LOGOUT_PATH = '/dshgw/logout/'
    const PORTAL_PATH = '/'

    const CSS = `
.dshgw-account-card { display: flex; flex-direction: column; width: 100%; }
.dshgw-account-row { display: flex; align-items: center; gap: 6px; width: 100%; padding: 6px 8px; border-radius: 6px; }
.dshgw-account-row:hover { background: rgba(127,127,127,.14); }
.dshgw-account-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; opacity: .85; }
/* justify-content is what centres the glyph and the label INSIDE the button: a button is a
   flex box of its own, so without it its content sits at the start edge and the padding on the
   right makes the pair look pushed left. text-align does not apply to a flex container's
   items, which is why the button, not the label, carries the centring. */
.dshgw-account-logout { display: inline-flex; align-items: center; justify-content: center; gap: 4px; flex: none; background: none; border: 1px solid rgba(127,127,127,.35); color: inherit; font: inherit; cursor: pointer; padding: 2px 8px; border-radius: 6px; }
.dshgw-account-logout:hover { background: rgba(127,127,127,.2); }
.dshgw-account-logout[disabled] { opacity: .5; cursor: default; }
.dshgw-account-rail .dshgw-account-name, .dshgw-account-rail .dshgw-account-logout-label { display: none; }
/* The collapsed rail is one icon column, and every level of it is a flex container, so what has
   to end up centred is the BUTTON itself. Measured on the running tenant (CDP, real DOM): the
   shell's .footerActions is 27px wide at x=14 and each row inside it is another flex container;
   without a justify-content of its own a row lays its only child out at flex-start, which left
   this button at x=14 (centre 20.50) while the ssh and browser buttons sat at centre 27.50.
   The horizontal padding has to be gone at the same time (it is, above): with padding still
   there, centring lines up the padding box rather than the button and overshoots by 8px.
   With both rules the button centre is 27.50 — the same column as the other two icons. */
.dshgw-account-rail .dshgw-account-row { padding: 6px 0; justify-content: center; }
/* The power symbol's INK sits 1px left of the centre of its own 13px advance at 14px
   (measured in a real browser: ink [0..11] inside a 13px box, i.e. −1.00px), so even a
   perfectly centred box reads as "slightly left". At 15px the ink fills the box and the offset
   is −0.50px — the same small optical bias the ssh workspace's ⇄ already has, which is what
   the eye compares this against. line-height:1 keeps the box height tied to the glyph so the
   rail column's vertical rhythm does not change. */
.dshgw-account-logout-icon { font-size: 15px; line-height: 1; }
.dshgw-account-rail .dshgw-account-logout { border: 0; padding: 0; }
.dshgw-account-error { margin: 2px 8px 0; font-size: 12px; color: #ff9b9b; }
/* The shell renders sidebar.footer.action as one flex ROW, so every registration in it shares
   a foot that only fits one. A plugin owns no wrapper element: the list slot wraps each
   registration in a classless div, which puts the shell's container two levels up — :has() is
   the only selector that can reach it from here, and both shapes are covered. The browser and
   ssh action rows ship the same rule; this one makes the identity row a second LINE under
   them instead of a third column beside them. */
div:has(> .dshgw-account-card), div:has(> div > .dshgw-account-card) { flex-direction: column; }
`

    /** What the gateway said about the signed-in person, or why it said nothing. */
    let state = { phase: 'loading', loading: false, name: '', tenant: '', error: '', busy: false }
    const listeners = new Set()
    const emit = () => {
      for (const listener of [...listeners]) listener()
    }
    const patch = (next) => {
      state = { ...state, ...next }
      emit()
    }

    /** Install the stylesheet once. */
    function installCSS() {
      if (document.querySelector('style[data-plugin-css="dshgw-account-card/client.css"]') !== null) return
      const tag = document.createElement('style')
      tag.dataset.pluginCss = 'dshgw-account-card/client.css'
      tag.textContent = CSS
      document.head.appendChild(tag)
    }

    /**
     * Plugin body.
     * @param ctx - client root context.
     */
    function apply(ctx) {
      installCSS()

      // One read per page. A 404 means "this deployment has no identity row" (account_card
      // off, or a dsh that is not behind dshgw at all), which is not a failure to report: the
      // row simply stays absent. Anything else is a real failure — the person may well be
      // signed in and unable to sign out — so the row appears with the reason and a retry.
      const load = async () => {
        // Two callers are possible in principle (a re-mounted row, and the retry button), so
        // an in-flight read is never started twice: the answer is per page, not per click.
        if (state.loading) return
        patch({ loading: true })
        try {
          const response = await window.fetch(SESSION_PATH, {
            credentials: 'same-origin',
            headers: { Accept: 'application/json' },
          })
          if (response.status === 404) { patch({ phase: 'absent', loading: false, error: '' }); return }
          if (!response.ok) throw new Error(`HTTP ${response.status}`)
          const body = await response.json()
          if (!body || body.ok !== true || !body.value) throw new Error('unexpected answer')
          patch({
            phase: 'ready',
            loading: false,
            error: '',
            name: body.value.name || body.value.tenant || '',
            tenant: body.value.tenant || '',
          })
        } catch (error) {
          ctx.logger?.warn?.(`account-card: reading the signed-in identity failed: ${error?.message || error}`)
          patch({ phase: 'failed', loading: false, error: `账号信息读取失败：${error?.message || error}` })
        }
      }

      // The logout is a POST on this tenant's own origin: it revokes this tenant's session and
      // leaves the browser's other tenants signed in. The gateway answers 303 to the portal's
      // login page, and the navigation is done here — so the request must NOT follow that
      // redirect. Measured: when the portal's origin is not reachable from the browser, a
      // redirect-following fetch fails at the network layer ("TypeError: Failed to fetch")
      // even though the gateway already revoked the session, and the person is told the exit
      // failed while they are, in fact, signed out. With `redirect: 'manual'` the same answer
      // arrives as an opaque redirect, which is exactly "revoked, and the browser was told
      // where to go".
      const logout = async () => {
        if (state.busy) return
        patch({ busy: true, error: '' })
        try {
          const response = await window.fetch(LOGOUT_PATH, {
            method: 'POST',
            credentials: 'same-origin',
            headers: { Accept: 'application/json' },
            redirect: 'manual',
          })
          // 3xx (opaque or not) and 2xx both mean the request was served; 0 is the opaque
          // answer's status. A 4xx/5xx means the session is still alive and the person must
          // be told, because nothing they did had any effect.
          const served = response.type === 'opaqueredirect' || response.status === 0 || response.status < 400
          if (!served) throw new Error(`HTTP ${response.status}`)
          patch({ busy: false })
          window.location.assign(PORTAL_PATH)
        } catch (error) {
          // The request did not come back, which is not the same as "the session survived":
          // ask the gateway who we are. A refused read is the proof that the exit DID land
          // (the session is gone), so the row follows through to the portal instead of
          // reporting a failure the person cannot act on.
          try {
            const check = await window.fetch(SESSION_PATH, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
            if (check.status === 401 || check.status === 403 || check.status === 302 || check.status === 0) {
              patch({ busy: false, error: '' })
              window.location.assign(PORTAL_PATH)
              return
            }
          } catch (probeError) {
            // Both requests failed: the gateway itself is unreachable. The message below is
            // then the honest one, and the row keeps a retry.
          }
          patch({ busy: false, error: `退出失败：${error?.message || error}` })
        }
      }

      const subscribe = (listener) => {
        listeners.add(listener)
        return () => listeners.delete(listener)
      }

      /** The sidebar-foot row: the identity and the way out of it. */
      function AccountCard({ wide } = {}) {
        const current = React.useSyncExternalStore
          ? React.useSyncExternalStore(subscribe, () => state)
          : state
        React.useEffect(() => {
          load().catch(() => {})
        }, [])
        // Nothing to render where there is no identity surface at all: a plain dsh (or one
        // whose gateway keeps the feature off) must not grow a row that says "unavailable".
        if (current.phase === 'loading' || (current.phase === 'absent' && current.error === '')) return null
        const ready = current.phase === 'ready'
        return h('div', {
          className: wide === false ? 'dshgw-account-card dshgw-account-rail' : 'dshgw-account-card',
          'data-dshgw-account': current.tenant,
          'data-dshgw-account-state': current.phase,
        }, [
          h('div', { className: 'dshgw-account-row', key: 'row' }, [
            h('span', {
              className: 'dshgw-account-name',
              key: 'name',
              title: current.name,
            }, current.name),
            ready ? h('button', {
              key: 'logout',
              type: 'button',
              className: 'dshgw-account-logout',
              title: '退出登录',
              'aria-label': '退出登录',
              disabled: current.busy,
              onClick: () => { logout().catch(() => {}) },
            }, [
              h('span', { key: 'icon', className: 'dshgw-account-logout-icon', 'aria-hidden': 'true' }, '⏻'),
              h('span', { className: 'dshgw-account-logout-label', key: 'label' }, current.busy ? '退出中…' : '退出'),
            ]) : h('button', {
              // The read failed, so the row cannot even name the person yet: offer the read
              // again rather than an exit whose session state is unknown.
              key: 'retry',
              type: 'button',
              className: 'dshgw-account-logout',
              disabled: current.loading,
              onClick: () => { load().catch(() => {}) },
            }, current.loading ? '重试中…' : '重试'),
          ]),
          current.error === '' ? null : h('p', { className: 'dshgw-account-error', key: 'error' }, current.error),
        ])
      }

      // order 120 keeps this row after the workspace actions (browser 90, ssh 100): the foot
      // reads as "things you open", then "who you are".
      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({
        name: 'sidebar.footer.action',
        id: 'dshgw-account-card',
        order: 120,
        label: '账号',
      }, AccountCard)), 'account-card: sidebar entry')

      ctx.logger?.info?.('account-card: client surface registered')
    }

    exports.apply = apply
    exports.inject = inject
    return module.exports
  },
})
