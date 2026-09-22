# dshgw-web-tty

An interactive terminal inside the dsh web GUI: a real PTY behind dsh's own authenticated RPC
channel, rendered by xterm.js in the shell's overlay slot.

This plugin is out-of-tree in the sense that matters: it is not part of the published `dsh`
distribution, which ships no terminal UI (`ctx.terminals` is a line-oriented, agent-scoped seam
with sanitized output and no resize verb, so nothing there can back an xterm.js screen). This
repository ships it at `cmd/dshgw/plugin/web-tty/`, beside the picker plugin the sandbox already
binds, and the gateway renders a row for it in every account's profile (M75) — so a tenant's dsh
has the 终端 panel without anybody editing that account. A deployment that prefers the standalone
layout can still drop the directory into `$DSH_HOME/plugins/web-tty` and name it in that account's
own patch instead.

## What it does

- Adds a **终端** row to the sidebar footer. Clicking it docks a floating terminal panel
  (bottom-right by default, draggable, resizable, maximizable, `Ctrl+\`` toggles).
- Each tab is its own shell (`/bin/bash -i` by default) with a real PTY: full-screen programs,
  Ctrl+C, colours, arrow keys and history all behave normally.
- Closing the panel hides it **without killing the shells**; reopening replays the host's
  retained output. Closing a tab kills that shell.
- Geometry follows the panel: xterm's fit addon measures the pane and the plugin sends the new
  rows/cols, so the shell receives `SIGWINCH` and programs like `top` or `vim` repaint correctly.

## Architecture

```
browser (client.js)                       tenant node process (index.js)
  xterm.js + fit addon                      ctx.connection.rpc  ->  RPC_CHANNEL '/dshgw-web-tty'
  sidebar.footer.action: 终端 row            rpc-handlers.js: hello/open/read/write/resize/close/list/diag
  shell.overlay:        panel + tabs        tty-session.js:   session registry + scrollback ring
      |                                        |
      |  POST /dshgw-web-tty/<endpoint>        `-- node-pty 1.2.0-beta.15 (resolved from the dsh release)
      `---------------------------------------->  real PTY inside this process's bwrap sandbox
```

- **Transport**: `ctx.connection.rpc.call` — the same authenticated, same-origin channel the
  shipped ssh/browser-workspace plugins use. No websocket, no extra port, no second auth story.
- **`read` is a long poll.** The handler holds the POST until the PTY produces output past the
  caller's cursor, the child exits, the caller aborts, or `waitMs` elapses. Typing latency is
  therefore one round trip, not one poll interval, and an idle terminal costs one held request.
- **Offsets, not buffers.** The browser tracks an absolute cursor; the host keeps a bounded ring
  (400 000 code units by default) and answers `reset: true` when a cursor fell off the front, so
  a reconnect after a long absence replays the tail instead of rendering a torn screen.
  Surrogate pairs are never split at either edge of the ring.
- **The PTY is this plugin's own**, not `ctx.terminals`: only node-pty gives raw bytes and
  `resize()`. The shell therefore runs inside the tenant's bubblewrap sandbox with exactly the
  powers the agent's own bash tool has.

## Files

| File | Role |
|---|---|
| `index.js` | host half: config, node-pty resolution, RPC channel registration, idle sweep, reaper |
| `rpc-handlers.js` | endpoint table (shared with the tests) |
| `tty-session.js` | `OutputBuffer` ring + `TtySession` + `TtyRegistry` (no dsh imports, unit-tested) |
| `client.src.js` | browser half, hand-written |
| `build-client.mjs` | concatenates `vendor/*` + `client.src.js` into `client.js` |
| `client.js` | **generated** — the file dsh serves; edit `client.src.js` and rebuild |
| `vendor/` | xterm.js 6.0.0, @xterm/addon-fit 0.11.0, xterm.css (pinned, hashes in the built header) |
| `trace.jsonl` | append-only diagnostics (activation, sessions, browser calls); truncates at 256 KiB |

No bundler is involved: dsh serves a client plugin's `./client` file byte for byte as a classic
same-origin script, and the vendored UMD builds are wrapped in an IIFE that shadows
`module`/`exports`/`define` so they fall through to their `globalThis` branch. The bundle declares
no top-level bindings, because dsh concatenates several plugin bundles into one script scope.

## Enable / disable

The row lives in the **user-level** patch layer, `$DSH_HOME/cordis.patch.yml`, which the gateway
never rewrites (it renders only `profiles/web/cordis.patch.yml`, `settings.yaml`,
`.credentials.yaml`, `storages/workspace.json`):

```yaml
- insert:
    - id: dshgw-web-tty
      name: file:///home/winger/work/ai_gateway/data/dshgw-verify/state/tenants/dsh-tenant/.dsh/plugins/web-tty/index.js
```

The web profile declares `patchReload: live`, so the host half activates in the running server
without a restart. **The browser must still be refreshed once** after the row is first added:
the boot graph (`window.__DSH_BOOT__`) is re-rendered per index request, and an already-running
page ignores graph frames. To disable, delete the row (the plugin tree disposes and every PTY
is reaped) or add `disabled: true` to it.

## Configuration

All fields are optional; malformed values fall back to the defaults.

| Field | Default | Meaning |
|---|---|---|
| `shell` | `/bin/bash` | Program spawned per session |
| `args` | `['-i']` | Arguments for that program |
| `cwd` | `process.cwd()` (the tenant workspace) | Working directory of a new shell |
| `cwdRoot` | `''` (no clamp) | When set, a requested `cwd` outside it falls back to `cwd` |
| `cols` / `rows` | `120` / `32` | Initial geometry before the browser measures its pane |
| `termName` | `xterm-256color` | `TERM` handed to the child |
| `bufferChars` | `400000` | Scrollback ring per session |
| `maxSessions` | `8` | Concurrent PTYs; exited ones are reclaimed first |
| `maxReadChars` | `262144` | Largest page one `read` returns |
| `maxWaitMs` | `25000` | Longest hold for one `read` |
| `idleTimeoutMs` | `0` (off) | Reap a session with no traffic for this long |
| `trace` | `true` | Write `trace.jsonl` |
| `traceFile` | `<this plugin dir>/trace.jsonl` | Where that trace goes. The gateway names a per-tenant file (`<DshHome>/plugin-state/web-tty.trace.jsonl`) because one installed copy serves every account and a shared file would mix them together (M75). A relative or empty value is ignored, and the parent directory is created on first write. |

The browser can pass only `cwd`, `cols` and `rows`; `shell` and `args` are never taken from the
request, so a page cannot ask for an arbitrary binary.

## Verify

```sh
cd $DSH_HOME/plugins/web-tty
npm run build        # regenerate client.js after editing client.src.js
npm test             # 13 tests: ring/offset math, real-PTY sessions, host endpoints, client flow
tail -n 20 trace.jsonl
```

`npm test` needs `DSHGW_DSH_ANCHOR` (or a `node` argv[1]) pointing at the dsh release so
`node-pty` resolves. The client test mounts the real bundle with a stubbed renderer and xterm and
drives it against the real endpoint table and a real PTY, so it covers open → read → keystroke →
read → resize → close without a browser.

In the GUI after a refresh: the sidebar footer shows **终端**; `trace.jsonl` gains
`{"event":"rpc","endpoint":"hello"}` when the browser half reaches the host, then `open`, `write`
and `read` lines as you type.

## Known limitations

- Sessions are process-local: restarting the harness kills every shell.
- One shell per browser tab; nothing is shared between people or tabs.
- `read` is a POST long poll, not a websocket. Output latency is one round trip; a websocket over
  `ctx.webServer.registerUpgrade` would save the per-call envelope but needs its own auth gate and
  proxy `Upgrade` forwarding.
- xterm renders in the DOM renderer; the WebGL addon is not bundled.
- The panel is an overlay, not a docked grid row: the shell exposes no bottom-panel slot.
