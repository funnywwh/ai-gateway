# dshgw-workspace-files

A workspace file manager inside the dsh web GUI: browse the tenant's workspace, preview or edit text,
preview images and media, upload, download, create folders, rename and delete — all over dsh's own
authenticated RPC channel, with every path clamped to one workspace root.

This plugin is out-of-tree in the sense that matters: the published distributions ship no file
manager, and the third-party ones on npm (`@deepseek-ai/dsh-client-ui-sidebar-files`,
`dsh-sidebar-files`, `@khorsheed/dsh-client-ui-file-preview`, `dsh-better-sidebar`, …) all target
the **0.1.5/0.1.6 client generation**: they inject `@deepseek-ai/dsh-client-resources`,
`@deepseek-ai/dsh-client-ui-sidebar-right`, `@deepseek-ai/dsh-api-workspace-files`,
`@deepseek-ai/dsh-client-ui-primitives` and `@deepseek-ai/dsh-client-ui-slots`, none of which exists
in this 0.1.2-rc.1 install. This plugin composes the same feature out of what the installed shell
actually offers.

This repository ships it at `cmd/dshgw/plugin/workspace-files/`, beside the picker plugin the
sandbox already binds, and the gateway renders a row for it in every account's profile (M75) — so a
tenant's dsh has the 文件 panel without anybody editing that account. A deployment that prefers the
standalone layout can still drop the directory into `$DSH_HOME/plugins/workspace-files` and name it
in that account's own patch instead.

## What it does

- Adds a **文件** row to the sidebar footer. Clicking it docks a floating panel (bottom-right by
  default, draggable, resizable, maximizable) over the conversation.
- **Browse**: breadcrumb navigation, parent/root/reload buttons, a hidden-entry toggle, a
  name/size/mtime sort that toggles direction, and a row per entry with its kind badge, size and
  mtime. Directories open on click or double click; a symlink whose target leaves the workspace is
  flagged.
- **Search** by file name across the whole workspace (bounded, breadth-first, hidden directories
  skipped unless the hidden toggle is on). A hit opens its folder, or the file itself.
- **View and edit**: text files open in a monospace editor with a **Save** button and `Ctrl/Cmd+S`;
  images, video, audio and PDF preview from a blob assembled over the RPC channel; anything else
  reports that it cannot be previewed and still offers a download. Save refuses to clobber a version
  the editor has not seen (it sends the mtime it read).
- **Upload**: the toolbar button or a drag-and-drop onto the panel, chunked, with progress, several
  files at once, and a notice that says how many same-named files were overwritten.
- **Download** any file, chunked, with progress.
- **Create folder**, **rename**, **delete** through in-panel dialogs (delete always confirms and
  says the operation is irreversible). Every mutation reloads the directory afterwards.

## Architecture

```
browser (client.js)                          tenant node process (index.js)
  Panel  in shell.overlay                      ctx.connection.rpc  ->  RPC_CHANNEL '/dshgw-workspace-files'
  Entry  in sidebar.footer.action              rpc-handlers.js: hello/list/stat/readText/writeText/
      |                                          readChunk/writeChunk/mkdir/rename/remove/find/diag
      |  POST /dshgw-workspace-files/<endpoint>  fs-service.js: one root, one clamp, bounded I/O
      `--------------------------------------->  the tenant's own workspace on a real filesystem
```

- **Transport**: `ctx.connection.rpc.call` — the same authenticated, same-origin channel the shipped
  ssh/browser-workspace plugins and `dshgw-web-tty` use. No websocket, no extra port, no second auth
  story, no route of this plugin's own.
- **Chunked bytes**: file content crosses as base64 in `chunkBytes`-sized pieces, in both directions,
  so a 33 MiB video downloads as a loop of small authenticated POSTs and neither side holds the whole
  file twice. The browser asks the host how large a chunk it accepts (`hello().limits.chunkBytes`)
  and never asks for more.
- **One root**: `config.root`. Every request path is relative, is rejected if it contains `..`, `.`,
  a backslash, a NUL or a leading slash, and is then resolved through `realpath` and compared against
  the resolved root — so a symlink that leaves the workspace is refused rather than followed. The
  root itself is never writable, renamable or removable.
- **Failures are coded**: every endpoint answers `{ok:true,value}` or
  `{ok:false,error:{code,message,details}}` with a stable code (`files/outside-root`,
  `files/not-found`, `files/too-large`, `files/read-only`, …), and the browser branches on the code
  rather than the message.

## Files

| File | Role |
|---|---|
| `index.js` | host half: config, root resolution, RPC channel registration, trace log |
| `fs-service.js` | the clamp, the bounded read/write/transfer primitives, no dsh imports |
| `rpc-handlers.js` | endpoint table (shared with the tests) |
| `client.src.js` | browser half, hand-written |
| `build-client.mjs` | stamps `client.src.js` into `client.js` (nothing is vendored) |
| `client.js` | **generated** — the file dsh serves; edit `client.src.js` and rebuild |
| `trace.jsonl` | append-only diagnostics (activation, RPC calls); truncates at 256 KiB |
| `test/host.test.mjs` | 11 tests: path vocabulary, clamp, limits, every endpoint |
| `test/client.test.mjs` | 6 tests: the built bundle driven against the real host half |

The bundle declares no top-level bindings, because dsh concatenates several plugin bundles into one
script scope; React comes from the shell's frozen module table, so the client half requires only
`react` and declares no externals.

## Enable / disable

The row lives in the **user-level** patch layer, `$DSH_HOME/cordis.patch.yml`, which the gateway
never rewrites:

```yaml
- insert:
    - id: dshgw-workspace-files
      name: file:///home/winger/work/ai_gateway/data/dshgw-verify/state/tenants/dsh-tenant/.dsh/plugins/workspace-files/index.js
      config:
        root: /home/winger/work/ai_gateway/data/dshgw-verify/state/workspaces/dsh-tenant
        rootLabel: 工作区
```

The web profile declares `patchReload: live`, so the host half activates in the running server
without a restart (check `trace.jsonl` for the `activated` line). **The browser must be refreshed
once** after the row is first added: the boot graph (`window.__DSH_BOOT__`) is re-rendered per index
request, and an already-running page ignores graph frames. To disable, delete the row or add
`disabled: true`; the RPC channel disappears with the fiber.

## Configuration

All fields are optional; malformed values fall back to the defaults.

| Field | Default | Meaning |
|---|---|---|
| `root` | `process.cwd()` (the tenant workspace) | The only subtree the panel may touch; resolved through `realpath` at activation, and a root that does not exist fails activation loudly |
| `rootLabel` | the root's basename | How the root is labelled in the panel |
| `readOnly` | `false` | Refuse every mutation (the toolbar hides/disabled them and `hello` reports it) |
| `limits.maxTextBytes` | 2 MiB | Largest file the editor will open or write |
| `limits.chunkBytes` | 1 MiB | Largest base64 chunk per transfer call; the browser adopts it |
| `limits.maxListEntries` | 2000 | Rows per listing before it truncates |
| `limits.maxUploadBytes` | 512 MiB | Largest accepted write offset |
| `limits.maxSearchResults` | 200 | Search hits before it truncates |
| `limits.maxSearchDepth` | 6 | Directory levels the search descends |
| `limits.maxSearchVisits` | 20000 | Directory entries the search may look at |
| `trace` | `true` | Write `trace.jsonl` |
| `traceFile` | `<this plugin dir>/trace.jsonl` | Where that trace goes. The gateway names a per-tenant file (`<DshHome>/plugin-state/workspace-files.trace.jsonl`) because one installed copy serves every account and a shared file would mix them together (M75). A relative or empty value is ignored, and the parent directory is created on first write. |

The same numbers may be given as flat keys (`maxTextBytes`, `chunkBytes`, …) instead of a `limits`
object.

## Endpoints

| Endpoint | Payload | Value |
|---|---|---|
| `hello` | — | `{version,root,rootLabel,readOnly,limits}` |
| `list` | `{path,showHidden?}` | `{path,absolute,parent,entries[],total,hidden,truncated}` |
| `stat` | `{path}` | one entry's metadata |
| `readText` | `{path,maxBytes?}` | `{path,size,mtimeMs,text,limit}`; refuses binary and oversized files |
| `writeText` | `{path,text,expectedMtimeMs?}` | `{path,size,mtimeMs,created}`; refuses a stale editor |
| `readChunk` | `{path,offset,length?}` | `{offset,size,bytes(base64),eof,mtimeMs}` |
| `writeChunk` | `{path,offset,data(base64)}` | `{offset,written,size}`; offset 0 truncates |
| `mkdir` | `{path,name?}` | the new directory's metadata |
| `rename` | `{path,name}` | the renamed entry's metadata |
| `remove` | `{path,recursive?}` | `{path,kind}`; a non-empty directory needs `recursive` |
| `find` | `{query,limit?,maxDepth?,showHidden?}` | `{query,matches[],visited,truncated}` |
| `diag` | — | version, pid, node, root, limits, per-endpoint call counts |

## Verify

```sh
cd $DSH_HOME/plugins/workspace-files
npm run build        # regenerate client.js after editing client.src.js
npm test             # 17 tests: clamp/limits/endpoints, then the built bundle end to end
tail -n 20 trace.jsonl
```

`npm test` needs no browser and no network: the host tests run against a temporary workspace on a
real filesystem, and the client test mounts the real `client.js` with a stubbed DOM and a stub React
against the real endpoint table, driving open → browse → edit → save → upload → download → create →
rename → delete → search.

In the GUI after a refresh: the sidebar footer shows **文件**; clicking it lists the workspace root
(in this tenant: `browser/`, `scratch/`, `ssh/`, `test/`, …). `trace.jsonl` gains
`{"event":"rpc","endpoint":"hello"}` as the browser half reaches the host, then `list`, `readText`,
`readChunk`… lines as you use it.

## Known limitations

- **No move or copy** — rename stays inside one directory; there is no drag-to-move and no copy.
- **Preview loads the whole file into a blob** — chunked and bounded, but a 2 GiB video is refused
  (`MAX_TRANSFER_BYTES`) rather than streamed; a Range-served HTTP route would be the fix, and it
  would need its own authentication gate.
- **No recursive delete of a directory tree without the dialog's explicit confirmation** — deleting a
  folder always passes `recursive: true` after the confirmation, which is why the dialog states the
  retention boundary.
- **No file watching** — the listing only refreshes when you reload, mutate, or upload; a file the
  agent created meanwhile appears after the next refresh.
- **No multi-select, no per-file permissions or owner display, no trash** — deletes are permanent.
- **Text editing is a plain textarea** — no syntax highlighting, no diff, no undo across saves.
- **Search matches names only** — no content search, and it does not descend into hidden directories
  unless the hidden toggle is on.
- **The panel is an overlay, not a docked grid row** — the 0.1.2-rc.1 shell exposes no bottom or
  right-panel seat to an out-of-tree plugin (`details` is single-owner and session-scoped).
