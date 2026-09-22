// Browser half of dshgw-web-tty: a dockable terminal panel inside the dsh web shell.
//
// Prebuilt by hand, like every out-of-tree surface in this deployment: the installation ships
// no bundler and dsh serves a client plugin's ./client file byte for byte. build-client.mjs
// concatenates the vendored xterm.js/@xterm/addon-fit UMD bundles (which assign `Terminal` and
// `FitAddon` onto globalThis) ahead of this file, so `window.Terminal` exists by the time the
// factory below materializes.
//
// Transport: one authenticated POST per intent on dsh's own RPC channel (ctx.connection.rpc).
// `read` is a long poll, so the panel does not poll on a timer: the pending read resolves the
// instant the PTY produces output, which is what keeps typing latency at one round trip while
// costing no websocket, no extra port and no second authentication story.

window.__ModuleLoader__.load({
  id: 'dshgw-web-tty',
  factory: (require) => {
    // The wrapper every shipped bundle uses: the page's loader hands the factory a CommonJS
    // style `require`, and it must return `module.exports`.
    var module = { exports: {} }
    var exports = module.exports
    const React = require('react')
    const h = React.createElement
    const { useCallback, useEffect, useRef, useState } = React

    /** Required services: the slot registry and the authenticated browser RPC carrier. */
    const inject = ['slots', 'connection']

    const RPC_CHANNEL = '/dshgw-web-tty'
    /** How long one `read` POST may be held open before it answers empty. */
    const LONG_POLL_MS = 20000
    /** Retry delays after a transport failure, capped. */
    const RETRY_BASE_MS = 400
    const RETRY_CAP_MS = 5000
    /** Total tabs this panel will hold open at once. */
    const MAX_TABS = 6
    const STORAGE_LAYOUT = 'dshgw-web-tty:layout'
    const STORAGE_LAYOUT_VERSION = 1
    const MIN_W = 360
    const MIN_H = 160
    const EDGE = 12

    /** One flat stylesheet: vendored xterm CSS first so panel rules can win later. */
    const XTERM_CSS = String(window.__DSHGW_WEB_TTY_XTERM_CSS__ ?? '')
    const CSS = `
.dshgw-tty-panel { position: absolute; display: flex; flex-direction: column; min-width: ${MIN_W}px; min-height: ${MIN_H}px;
  background: var(--dsw-alias-bg-layer-2, #1b1c1f); color: var(--dsw-alias-label-primary, #e6e6e6);
  border: 1px solid var(--dsw-alias-border-l3, rgba(127,127,127,.35)); border-radius: 10px;
  box-shadow: 0 14px 44px rgba(0,0,0,.5); overflow: hidden; z-index: 30; font-size: 12px; }
.dshgw-tty-head { display: flex; align-items: center; gap: 6px; padding: 3px 6px; flex: none;
  background: var(--dsw-alias-bg-overlay, rgba(127,127,127,.12)); border-bottom: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.24)); }
.dshgw-tty-grip { display: flex; align-items: center; gap: 6px; cursor: move; user-select: none; flex: none; }
.dshgw-tty-name { font-weight: 600; opacity: .85; }
.dshgw-tty-tabs { display: flex; align-items: center; gap: 2px; flex: 1; min-width: 0; overflow: hidden; }
.dshgw-tty-tab { display: inline-flex; align-items: center; gap: 5px; max-width: 170px; padding: 2px 6px; border-radius: 5px;
  cursor: pointer; opacity: .7; white-space: nowrap; overflow: hidden; }
.dshgw-tty-tab[data-active="true"] { background: var(--dsw-alias-interactive-bg-hover, rgba(127,127,127,.24)); opacity: 1; }
.dshgw-tty-tab-label { overflow: hidden; text-overflow: ellipsis; }
.dshgw-tty-tab-x { border: 0; background: none; color: inherit; font: inherit; line-height: 1; padding: 0 2px; cursor: pointer; opacity: .6; }
.dshgw-tty-tab-x:hover { opacity: 1; }
.dshgw-tty-dot { width: 6px; height: 6px; border-radius: 50%; flex: none; background: #8a8f98; }
.dshgw-tty-dot[data-status="live"] { background: var(--dsw-alias-state-success-primary, #3a7d44); }
.dshgw-tty-dot[data-status="exited"] { background: var(--dsw-alias-label-secondary, #b8b8b8); }
.dshgw-tty-dot[data-status="failed"] { background: var(--dsw-alias-state-error-primary, #b3453c); }
.dshgw-tty-tools { display: flex; align-items: center; gap: 2px; flex: none; }
.dshgw-tty-btn { display: inline-flex; align-items: center; justify-content: center; min-width: 24px; height: 22px; padding: 0 5px;
  background: none; border: 0; border-radius: 5px; color: inherit; font: inherit; cursor: pointer; opacity: .8; }
.dshgw-tty-btn:hover { background: var(--dsw-alias-interactive-bg-hover, rgba(127,127,127,.28)); opacity: 1; }
.dshgw-tty-btn[disabled] { opacity: .35; cursor: default; }
.dshgw-tty-body { position: relative; flex: 1; min-height: 0; }
.dshgw-tty-pane { position: absolute; inset: 0; padding: 4px 0 0 6px; }
.dshgw-tty-pane[hidden] { display: none; }
.dshgw-tty-foot { display: flex; align-items: center; gap: 8px; flex: none; padding: 2px 8px; font-size: 11px;
  border-top: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.24)); opacity: .8; }
.dshgw-tty-foot-hint { margin-left: auto; opacity: .7; }
.dshgw-tty-error { color: var(--dsw-alias-state-error-primary, #b3453c); }
.dshgw-tty-size { flex: none; width: 14px; height: 14px; cursor: nwse-resize; }
.dshgw-tty-size-x { position: absolute; top: 26px; right: 0; bottom: 14px; width: 5px; cursor: ew-resize; }
.dshgw-tty-size-y { position: absolute; left: 0; right: 14px; bottom: 0; height: 5px; cursor: ns-resize; }
.dshgw-tty-thumb { position: absolute; right: 3px; bottom: 3px; opacity: .45; pointer-events: none; }
.dshgw-tty-entry { display: flex; align-items: center; gap: 6px; width: 100%; padding: 6px 8px; border-radius: 6px; background: none; border: 0; color: inherit; font: inherit; text-align: left; cursor: pointer; }
.dshgw-tty-entry:hover { background: rgba(127,127,127,.14); }
.dshgw-tty-entry-label { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; opacity: .85; }
.dshgw-tty-entry-icon { flex: none; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; opacity: .8; }
/* The collapsed sidebar is one narrow icon column: the label cannot fit, so it is dropped and the
   row centres its only child — the same shape the ssh and account rows use in the rail. */
.dshgw-tty-entry-rail .dshgw-tty-entry-label { display: none; }
.dshgw-tty-entry-rail { padding: 6px 0; justify-content: center; }
`

    /** Read one theme token, falling back when the shell does not define it. */
    function readVar(name, fallback) {
      try {
        const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
        return value === '' ? fallback : value
      } catch {
        return fallback
      }
    }

    /** #rrggbb -> perceived luminance, or null when the token is not a hex colour. */
    function luminance(hex) {
      const match = /^#([0-9a-f]{6})$/i.exec(hex.trim())
      if (match === null) return null
      const value = parseInt(match[1], 16)
      const channel = [(value >> 16) & 0xff, (value >> 8) & 0xff, value & 0xff].map((part) => part / 255)
      const linear = channel.map((part) => (part <= 0.03928 ? part / 12.92 : ((part + 0.055) / 1.055) ** 2.4))
      return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2]
    }

    /**
     * xterm theme derived from the shell's own tokens, so the panel belongs to the active theme
     * instead of being a dark island inside a light UI (or the reverse). ANSI slots follow the
     * luminance of the resolved background.
     */
    function readTheme() {
      const background = readVar('--dsw-alias-bg-base', '#16171a')
      const foreground = readVar('--dsw-alias-label-primary', '#e6e6e6')
      const light = (luminance(background) ?? 0) > 0.5
      const ansi = light
        ? ['#3b3f46', '#c0392b', '#2f7d32', '#a1791d', '#2f6feb', '#8e44ad', '#1d7f8c', '#c8ccd4',
           '#7b818c', '#e05545', '#3f9a45', '#bf942c', '#4a86f7', '#a35cbf', '#2b98a6', '#2b2f36']
        : ['#2b2f36', '#e06c75', '#98c379', '#e5c07b', '#61afef', '#c678dd', '#56b6c2', '#c8ccd4',
           '#5c6370', '#e58c92', '#a9d18e', '#f0d197', '#82c4f5', '#d79be8', '#7fd0da', '#f2f4f8']
      return {
        background,
        foreground,
        cursor: readVar('--dsw-alias-label-primary', foreground),
        cursorAccent: background,
        selectionBackground: light ? 'rgba(47,111,235,.28)' : 'rgba(97,175,239,.32)',
        black: ansi[0], red: ansi[1], green: ansi[2], yellow: ansi[3], blue: ansi[4], magenta: ansi[5], cyan: ansi[6], white: ansi[7],
        brightBlack: ansi[8], brightRed: ansi[9], brightGreen: ansi[10], brightYellow: ansi[11],
        brightBlue: ansi[12], brightMagenta: ansi[13], brightCyan: ansi[14], brightWhite: ansi[15],
      }
    }

    /** Install the one stylesheet this plugin owns. */
    function installCSS() {
      if (document.querySelector('style[data-plugin-css="dshgw-web-tty/client.css"]') !== null) return
      const tag = document.createElement('style')
      tag.dataset.pluginCss = 'dshgw-web-tty/client.css'
      tag.textContent = `${XTERM_CSS}\n${CSS}`
      document.head.appendChild(tag)
    }

    const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

    /** Layout persistence: panel geometry survives a page reload. */
    function loadLayout() {
      const fallback = { open: false, x: null, y: null, w: 780, h: 360, maximized: false }
      try {
        const raw = window.localStorage.getItem(STORAGE_LAYOUT)
        if (raw === null) return fallback
        const parsed = JSON.parse(raw)
        if (parsed === null || typeof parsed !== 'object' || parsed.version !== STORAGE_LAYOUT_VERSION) return fallback
        const number = (value, keep) => (typeof value === 'number' && Number.isFinite(value) ? value : keep)
        return {
          open: parsed.open === true,
          maximized: parsed.maximized === true,
          x: parsed.x === null ? null : number(parsed.x, null),
          y: parsed.y === null ? null : number(parsed.y, null),
          w: Math.max(MIN_W, number(parsed.w, fallback.w)),
          h: Math.max(MIN_H, number(parsed.h, fallback.h)),
        }
      } catch {
        return fallback
      }
    }

    /** The plugin body. */
    function apply(ctx) {
      installCSS()

      /**
       * One RPC call, unwrapped. `signal` is optional and only used by the long-polling read,
       * so that a disposed panel stops holding a request open on the host.
       *
       * `payload ?? {}` keeps the envelope valid for an endpoint that takes no arguments — the
       * argument-less `hello` below is one. The shell serializes the body with JSON.stringify
       * (which drops `payload: undefined`) and the host's schema keeps `payload` non-optional on
       * this installation's zod 4, so a missing key is rejected as "invalid client-request message"
       * before the endpoint runs. The field failure and the regression test that pins it are in
       * `cmd/dshgw/plugin/git-diff/` (client.src.js and test/client.test.mjs).
       */
      const call = async (endpoint, payload, signal) => {
        const result = await ctx.connection.rpc.call(RPC_CHANNEL, endpoint, payload ?? {}, signal)
        if (result === null || typeof result !== 'object' || result.ok !== true) {
          const error = (result !== null && typeof result === 'object' && result.error) || null
          const failure = new Error(error === null ? 'the terminal service did not answer' : String(error.message))
          failure.code = error === null ? 'transport' : String(error.code)
          throw failure
        }
        return result.value
      }

      // ---- shared panel state -------------------------------------------------------------
      let state = { layout: loadLayout(), tabs: [], active: null, notice: '' }
      const listeners = new Set()
      /** tab key -> the mounted pane's live handles; the toolbar's only way to reach a terminal. */
      const panes = new Map()
      let tabSeq = 0

      const notify = () => {
        for (const listener of [...listeners]) {
          try {
            listener(state)
          } catch (error) {
            ctx.logger?.warn?.(`web-tty: listener failed: ${error?.message ?? error}`)
          }
        }
      }
      const setState = (patch) => { state = { ...state, ...patch }; notify() }
      const patchTab = (key, patch) => {
        setState({ tabs: state.tabs.map((tab) => (tab.key === key ? { ...tab, ...patch } : tab)) })
      }
      const saveLayout = (patch) => {
        const layout = { ...state.layout, ...patch }
        setState({ layout })
        try {
          window.localStorage.setItem(STORAGE_LAYOUT, JSON.stringify({ version: STORAGE_LAYOUT_VERSION, ...layout }))
        } catch {
          // A blocked localStorage only costs the remembered geometry.
        }
      }

      /** Open one more tab; its session is created by that tab's own view once it is measured. */
      const addTab = () => {
        if (state.tabs.length >= MAX_TABS) return
        const key = `tab-${++tabSeq}`
        setState({ tabs: [...state.tabs, { key, id: null, status: 'opening', title: '新终端', exitCode: null, cwd: null, pid: null, notice: '' }], active: key })
      }

      /** Close a tab and its PTY. */
      const closeTab = (key) => {
        const tab = state.tabs.find((candidate) => candidate.key === key)
        const rest = state.tabs.filter((candidate) => candidate.key !== key)
        const active = state.active === key ? (rest.length > 0 ? rest[rest.length - 1].key : null) : state.active
        setState({ tabs: rest, active })
        if (tab?.id != null) void call('close', { id: tab.id }).catch(() => {})
      }

      /** Show the panel, opening a first terminal when there is none. */
      const show = () => {
        saveLayout({ open: true })
        if (state.tabs.length === 0) addTab()
      }
      const toggle = () => {
        if (state.layout.open) saveLayout({ open: false })
        else show()
      }

      /** Keep a panel that was restored inside the viewport of a window that has since shrunk. */
      const clampLayout = () => {
        const layout = state.layout
        const maxW = Math.max(MIN_W, document.documentElement.clientWidth - EDGE * 2)
        const maxH = Math.max(MIN_H, document.documentElement.clientHeight - EDGE * 2)
        const w = Math.min(layout.w, maxW)
        const h = Math.min(layout.h, maxH)
        if (w === layout.w && h === layout.h) return
        saveLayout({ w, h })
      }

      /** The terminal surface for one tab: its own xterm instance plus its own read loop. */
      function TerminalPane({ tab, visible }) {
        const hostRef = useRef(null)
        const termRef = useRef(null)
        const fitRef = useRef(null)
        const idRef = useRef(tab.id)
        const cursorRef = useRef(0)
        const aliveRef = useRef(true)

        // Re-fit whenever this tab becomes the visible one: a hidden pane measures 0x0 and must
        // not be allowed to resize the PTY down to a two-column screen.
        useEffect(() => {
          if (!visible) return undefined
          const handle = window.requestAnimationFrame(() => {
            try {
              fitRef.current?.fit()
            } catch {
              // A pane that is still being laid out simply fits on the next observation.
            }
          })
          return () => window.cancelAnimationFrame(handle)
        }, [visible])

        useEffect(() => {
          aliveRef.current = true
          const host = hostRef.current
          if (host === null || typeof window.Terminal !== 'function') {
            patchTab(tab.key, { status: 'failed', notice: '终端组件 (xterm.js) 未能加载' })
            return undefined
          }
          const term = new window.Terminal({
            allowProposedApi: true,
            convertEol: false,
            cursorBlink: true,
            fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
            fontSize: 12.5,
            letterSpacing: 0,
            scrollback: 5000,
            theme: readTheme(),
          })
          const fit = new window.FitAddon.FitAddon()
          term.loadAddon(fit)
          term.open(host)
          termRef.current = term
          fitRef.current = fit
          const fitQuietly = () => {
            if (!aliveRef.current || host.clientWidth === 0 || host.clientHeight === 0) return
            try {
              fit.fit()
            } catch {
              // fit() throws while the pane is mid-layout; the next observation retries.
            }
          }
          fitQuietly()

          const controller = new AbortController()
          const dataSub = term.onData((data) => {
            const id = idRef.current
            if (id === null) return
            call('write', { id, data }).catch((error) => {
              if (error?.code === 'NO_SESSION') patchTab(tab.key, { status: 'failed', notice: '会话已结束' })
            })
          })
          const resizeSub = term.onResize(({ cols, rows }) => {
            const id = idRef.current
            if (id === null) return
            call('resize', { id, cols, rows }).catch(() => {})
          })
          const observer = new ResizeObserver(fitQuietly)
          observer.observe(host)
          panes.set(tab.key, { term, fit, focus: () => term.focus() })

          /** Hold one read POST open at a time; each answer is written straight to the screen. */
          const pump = async (id) => {
            let failures = 0
            while (aliveRef.current && !controller.signal.aborted) {
              try {
                const page = await call('read', { id, offset: cursorRef.current, waitMs: LONG_POLL_MS, maxChars: 262144 }, controller.signal)
                if (!aliveRef.current) return
                failures = 0
                if (page.reset) term.reset()
                if (typeof page.data === 'string' && page.data.length > 0) term.write(page.data)
                cursorRef.current = page.offset
                if (page.status !== 'running') {
                  const code = page.exitCode === null || page.exitCode === undefined ? '?' : page.exitCode
                  term.write(`\r\n\x1b[2m[进程已退出 code=${code}] 按 ＋ 新建终端\x1b[0m\r\n`)
                  patchTab(tab.key, { status: 'exited', exitCode: page.exitCode ?? null, notice: '' })
                  return
                }
              } catch (error) {
                if (!aliveRef.current || controller.signal.aborted) return
                if (error?.code === 'NO_SESSION') {
                  patchTab(tab.key, { status: 'exited', notice: '会话已结束，按 ＋ 新建终端' })
                  return
                }
                failures += 1
                if (failures >= 4) patchTab(tab.key, { status: 'failed', notice: `${error?.message ?? error}` })
                await sleep(Math.min(RETRY_CAP_MS, RETRY_BASE_MS * 2 ** Math.min(6, failures)))
              }
            }
          }

          /** Create this tab's PTY, then start its read loop. */
          const start = async () => {
            try {
              if (tab.id !== null) return await attach(tab.id)
              const cols = term.cols >= 2 ? term.cols : 80
              const rows = term.rows >= 1 ? term.rows : 24
              const opened = await call('open', { cols, rows })
              if (!aliveRef.current) return
              idRef.current = opened.session.id
              cursorRef.current = opened.page.offset
              if (typeof opened.page.data === 'string' && opened.page.data.length > 0) term.write(opened.page.data)
              patchTab(tab.key, {
                id: opened.session.id,
                status: opened.session.status === 'running' ? 'live' : 'exited',
                title: `#${opened.session.pid ?? '?'}`,
                cwd: opened.session.cwd ?? null,
                pid: opened.session.pid ?? null,
                notice: '',
              })
              term.focus()
              void pump(opened.session.id)
            } catch (error) {
              if (!aliveRef.current) return
              if (error?.code === 'NO_SESSION') {
                idRef.current = null
                patchTab(tab.key, { id: null, status: 'exited', notice: '会话已结束，按 ＋ 新建终端' })
                term.write('\r\n\x1b[2m[会话已结束] 按 ＋ 新建终端\x1b[0m\r\n')
                return
              }
              term.write(`\r\n\x1b[31m终端启动失败：${error?.message ?? error}\x1b[0m\r\n`)
              patchTab(tab.key, { status: 'failed', notice: `${error?.message ?? error}` })
            }
          }

          /**
           * Re-attach to a PTY this tab already owns: hiding the panel unmounts the pane but must
           * not kill a running shell, so a remount replays the host's retained output from its
           * window start and resumes reading where that ends.
           * @param id - the session id recorded on the tab.
           */
          const attach = async (id) => {
            idRef.current = id
            cursorRef.current = 0
            const page = await call('read', { id, offset: 0, waitMs: 0, maxChars: 262144 })
            if (!aliveRef.current) return
            if (typeof page.data === 'string' && page.data.length > 0) term.write(page.data)
            cursorRef.current = page.offset
            fitQuietly()
            term.focus()
            patchTab(tab.key, {
              status: page.status === 'running' ? 'live' : 'exited',
              notice: page.status === 'running' ? '' : '会话已结束，按 ＋ 新建终端',
            })
            if (page.status !== 'running') return
            void pump(id)
          }
          void start()

          return () => {
            aliveRef.current = false
            controller.abort()
            observer.disconnect()
            dataSub.dispose()
            resizeSub.dispose()
            panes.delete(tab.key)
            termRef.current = null
            fitRef.current = null
            term.dispose()
          }
          // One terminal per tab key: the pane is mounted once and only hidden afterwards.
          // eslint-disable-next-line react-hooks/exhaustive-deps
        }, [tab.key])

        return h('div', { className: 'dshgw-tty-pane', hidden: !visible, ref: hostRef })
      }

      /** Panel chrome: drag, resize, tabs. */
      function Panel() {
        const [snapshot, setSnapshot] = useState(state)
        useEffect(() => {
          const listener = (next) => setSnapshot(next)
          listeners.add(listener)
          return () => listeners.delete(listener)
        }, [])
        const { layout, tabs, active, notice } = snapshot

        useEffect(() => {
          if (!layout.open) return undefined
          clampLayout()
          const onWindowResize = () => clampLayout()
          window.addEventListener('resize', onWindowResize)
          return () => window.removeEventListener('resize', onWindowResize)
          // eslint-disable-next-line react-hooks/exhaustive-deps
        }, [layout.open])

        /** Pointer drag: `mode` is move, east, south or both. */
        const beginDrag = useCallback((event, mode) => {
          if (event.button !== 0) return
          event.preventDefault()
          const startX = event.clientX
          const startY = event.clientY
          const origin = { ...state.layout }
          const frameW = document.documentElement.clientWidth
          const frameH = document.documentElement.clientHeight
          const baseX = origin.x ?? frameW - origin.w - EDGE
          const baseY = origin.y ?? frameH - origin.h - EDGE
          const move = (moveEvent) => {
            const dx = moveEvent.clientX - startX
            const dy = moveEvent.clientY - startY
            const next = { ...state.layout }
            if (mode === 'move' || mode === 'both') {
              next.x = Math.max(0, Math.min(frameW - 80, baseX + dx))
              next.y = Math.max(0, Math.min(frameH - 40, baseY + dy))
            }
            if (mode === 'east' || mode === 'both') next.w = Math.max(MIN_W, Math.min(frameW - EDGE, origin.w + dx))
            if (mode === 'south' || mode === 'both') next.h = Math.max(MIN_H, Math.min(frameH - EDGE, origin.h + dy))
            saveLayout(next)
          }
          const stop = () => {
            window.removeEventListener('pointermove', move)
            window.removeEventListener('pointerup', stop)
          }
          window.addEventListener('pointermove', move)
          window.addEventListener('pointerup', stop)
          // eslint-disable-next-line react-hooks/exhaustive-deps
        }, [])

        useEffect(() => {
          const onKey = (event) => {
            if ((event.ctrlKey || event.metaKey) && event.key === '`') {
              event.preventDefault()
              toggle()
            }
            if (event.key === 'Escape' && state.layout.maximized) saveLayout({ maximized: false })
          }
          window.addEventListener('keydown', onKey)
          return () => window.removeEventListener('keydown', onKey)
          // eslint-disable-next-line react-hooks/exhaustive-deps
        }, [])

        if (!layout.open) return null

        const frameW = document.documentElement.clientWidth
        const frameH = document.documentElement.clientHeight
        const style = layout.maximized
          ? { left: `${EDGE}px`, top: `${EDGE}px`, width: `${frameW - EDGE * 2}px`, height: `${frameH - EDGE * 2}px` }
          : {
              left: `${layout.x ?? frameW - layout.w - EDGE}px`,
              top: `${layout.y ?? frameH - layout.h - EDGE}px`,
              width: `${layout.w}px`,
              height: `${layout.h}px`,
            }
        const activeTab = tabs.find((tab) => tab.key === active) ?? tabs[tabs.length - 1] ?? null

        const tabRow = tabs.map((tab) => h('div', {
          key: tab.key,
          className: 'dshgw-tty-tab',
          'data-active': String(tab.key === activeTab?.key),
          title: `${tab.title}${tab.cwd === null ? '' : ` — ${tab.cwd}`}`,
          onClick: () => setState({ active: tab.key }),
        }, [
          h('span', { key: 'dot', className: 'dshgw-tty-dot', 'data-status': tab.status }),
          h('span', { key: 'label', className: 'dshgw-tty-tab-label' }, tab.title),
          h('button', {
            key: 'x',
            type: 'button',
            className: 'dshgw-tty-tab-x',
            title: '关闭这个终端',
            'aria-label': '关闭这个终端',
            onClick: (event) => { event.stopPropagation(); closeTab(tab.key) },
          }, '×'),
        ]))

        const tools = [
          h('button', { key: 'add', type: 'button', className: 'dshgw-tty-btn', title: '新建终端', 'aria-label': '新建终端',
            disabled: tabs.length >= MAX_TABS, onClick: () => addTab() }, '＋'),
          h('button', { key: 'clear', type: 'button', className: 'dshgw-tty-btn', title: '清屏', 'aria-label': '清屏',
            disabled: activeTab === null, onClick: () => { panes.get(activeTab.key)?.term.clear() } }, '清'),
          h('button', { key: 'max', type: 'button', className: 'dshgw-tty-btn', title: layout.maximized ? '还原' : '最大化',
            'aria-label': layout.maximized ? '还原' : '最大化', onClick: () => saveLayout({ maximized: !layout.maximized }) },
            layout.maximized ? '❐' : '⛶'),
          h('button', { key: 'close', type: 'button', className: 'dshgw-tty-btn', title: '收起面板 (Ctrl+`)', 'aria-label': '收起面板',
            onClick: () => saveLayout({ open: false }) }, '×'),
        ]

        return h('div', { className: 'dshgw-tty-panel', style, role: 'region', 'aria-label': '终端' }, [
          h('div', { key: 'head', className: 'dshgw-tty-head' }, [
            h('div', { key: 'grip', className: 'dshgw-tty-grip', onPointerDown: (event) => beginDrag(event, 'move'), title: '拖动' }, [
              h('span', { key: 'name', className: 'dshgw-tty-name' }, '终端'),
            ]),
            h('div', { key: 'tabs', className: 'dshgw-tty-tabs' }, tabRow),
            h('div', { key: 'tools', className: 'dshgw-tty-tools' }, tools),
          ]),
          h('div', { key: 'body', className: 'dshgw-tty-body' },
            tabs.map((tab) => h(TerminalPane, { key: tab.key, tab, visible: tab.key === activeTab?.key }))),
          h('div', { key: 'foot', className: 'dshgw-tty-foot' }, [
            h('span', { key: 'cwd' }, activeTab?.cwd ?? '〜'),
            h('span', { key: 'status' }, activeTab?.status === 'live' ? '运行中' : activeTab?.status === 'exited' ? '已退出' : activeTab?.status === 'failed' ? '失败' : '连接中…'),
            notice === '' ? null : h('span', { key: 'notice', className: 'dshgw-tty-error' }, notice),
            h('span', { key: 'hint', className: 'dshgw-tty-foot-hint' }, 'Ctrl+` 收起'),
          ]),
          h('div', { key: 'sx', className: 'dshgw-tty-size-x', onPointerDown: (event) => beginDrag(event, 'east') }),
          h('div', { key: 'sy', className: 'dshgw-tty-size-y', onPointerDown: (event) => beginDrag(event, 'south') }),
          h('div', { key: 's', className: 'dshgw-tty-size', onPointerDown: (event) => beginDrag(event, 'both') }, [
            h('span', { key: 'thumb', className: 'dshgw-tty-thumb' }, '◢'),
          ]),
        ])
      }

      /** Sidebar entry: the panel's only always-visible affordance. */
      function Entry(props) {
        const [snapshot, setSnapshot] = useState(state)
        useEffect(() => {
          const listener = (next) => setSnapshot(next)
          listeners.add(listener)
          return () => listeners.delete(listener)
        }, [])
        // The sidebar hands every footer action a `wide` flag; `false` is the collapsed icon rail.
        const wide = props?.wide !== false
        const live = snapshot.tabs.filter((tab) => tab.status === 'live').length
        return h('button', {
          type: 'button',
          className: wide ? 'dshgw-tty-entry' : 'dshgw-tty-entry dshgw-tty-entry-rail',
          title: snapshot.layout.open ? '收起终端 (Ctrl+`)' : '打开终端 (Ctrl+`)',
          'aria-label': '终端',
          onClick: () => toggle(),
        }, [
          h('span', { key: 'icon', className: 'dshgw-tty-entry-icon', 'aria-hidden': 'true' }, '>_'),
          wide ? h('span', { key: 'label', className: 'dshgw-tty-entry-label' }, live > 0 ? `终端 (${live})` : '终端') : null,
        ])
      }

      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({
        name: 'sidebar.footer.action',
        id: 'dshgw-web-tty-entry',
        order: 110,
        label: '终端',
      }, Entry)), 'web-tty: sidebar entry')

      ctx.effect(() => ctx.slots.inject('shell.overlay', () => ctx.slots.register({
        name: 'shell.overlay',
        id: 'dshgw-web-tty-panel',
        order: 210,
        label: '终端',
      }, Panel)), 'web-tty: terminal panel')

      // The host reports what it activated, which is the only way to tell "bundle loaded" from
      // "bundle loaded and reached the terminal service" without a console.
      void call('hello').then((info) => {
        ctx.logger?.info?.(`web-tty: client ready (host ${info.version}, node-pty ${info.nodePty ?? '?'})`)
      }).catch((error) => {
        ctx.logger?.warn?.(`web-tty: host half unreachable: ${error?.message ?? error}`)
      })

      ctx.logger?.info?.('web-tty: client surface registered')
    }

    exports.apply = apply
    exports.inject = inject
    return module.exports
  },
})
