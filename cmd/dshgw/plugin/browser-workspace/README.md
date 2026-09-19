# Browser workspace filesystem client

The plugin follows `ssh-workspace`'s Cordis/ModuleLoader packaging. `index.js`
only activates the client package: the **gateway**, not the tenant Node worker,
owns authenticated reverse HTTP, request queues, FUSE mounts and worker restarts.

## Gateway HTTP contract

All requests are same-origin JSON POSTs to `./browser-workspace/<endpoint>` with
same-origin credentials; redirects are rejected. Responses are
`{ok:true,value}` or `{ok:false,error:{code,message}}`.

| Endpoint | Payload | Value |
| --- | --- | --- |
| `open` | `{name,writable:true}` | `{token,mountpoint,id}` (mount pending) |
| `poll` | `{token}` | `{requests:[{id,op,path,offset?,size?,data?,target?,exclusive?,truncate?}]}` |
| `respond` | `{token,id,result:{ok,value?,error?}}` | acknowledgement |
| `activate` | `{token}` | `{mountpoint,id}` after mount/worker restart |
| `resume` | `{token}` | `{id,mountpoint,resumed}` — takes an existing mount back |
| `close` | `{token}` | acknowledgement |

The client starts poll **before** activate and continues during worker restart.
Activate and close use 55-second client timeouts to accommodate a 45-second worker
restart; other requests time out at 35 seconds. Gateway poll must return within
that deadline. Empty polls are paced at 100ms. No filesystem mutation is replayed.

The poll request *is* the browser side of the mount. When that connection dies
(page reload/close/crash, or this client's own abort before close) the mount stops
being served at once and is **excluded from every worker profile** — resolving one
clientless mount path costs the whole FUSE timeout and fails the worker start,
which would otherwise surface as an unconfirmed cleanup on another mount.

It is not torn down at once. The gateway keeps the kernel mount and its mount point
for a 45-second grace window, during which the same page (or the page that replaced
it) can take the mount back with `resume` — same path, same worker binding, no
second mount and no worker restart. Only when that window passes unused does the
gateway run the old teardown: restart worker, unmount, remove the mount point and
its record.

`resume` is refused while another browser side holds the mount's poll connection,
and the replaced side is then told `directory revoked` so it stops instead of
fighting the live page. Because a request the browser never sent (a blocked or
dropped connection) leaves the gateway still believing the old poll is alive, the
client's own reconnect only succeeds once that request has actually ended — one
retry later, not forever.

Workspace `create(path)` runs BEFORE activation, while the mount point is
guaranteed to exist: `open` creates that directory and any teardown (page reload,
transport failure, disconnect) removes it again, so registering after the
activation restart raced that removal and failed with `workspace/invalid-path:
ENOENT realpath`. Registration is idempotent and retried for up to 10 seconds
(each call bounded to 5 seconds).

After activation the client waits for a fresh connected DSH generation using the
public `connection.state`, `connection.generation` and `connection.reconnect`
services. It then best-effort renames the workspace and invokes
`uiWorkspace.connectWorkspace` once. Session opening is deliberately not retried
because it can create a second session. A mount that fails after registration
forgets that workspace (nothing was connected to it yet), because the gateway
removes the mount point with the capability.

A transport failure is recovered **in place**: the page asks the gateway to resume,
keeps serving, and never needs a reload, a click or a second directory choice. Only
a `resume` the gateway refuses (the capability is gone: `unknown directory
capability`, or `directory revoked` because another page took it over) ends the
mount and clears the stored record.

The mount's capability token and its directory handle are kept in IndexedDB, keyed
by the tenant origin. They are what makes recovery possible after a reload: a
FileSystemDirectoryHandle stored there is a real handle again in the next document,
and a granted `readwrite` permission survives the reload (measured in Chromium:
`queryPermission` returns `granted` with no user gesture, and read/write both work).
The next document does **not** resume on its own — it reads the record at boot and
shows "刷新前挂载的是 <dir>；点击恢复", and one click takes the same mount back. A
record the browser will not re-grant, one older than the grace window, or one the
gateway refuses is dropped, and that same click falls through to an ordinary mount.

On transport failure or explicit stop, local polling is aborted before requesting
close. Only a successful close response clears the directory token and displays
"disconnected". Cleanup failure preserves the token, displays an unconfirmed
cleanup warning, and offers retry without selecting/opening a second directory.
Disposal performs best-effort close and logs unconfirmed cleanup; the gateway
lease reaper remains necessary when a page disappears before acknowledgement.
Directory capabilities and handles stay in memory and are never logged. Gateway
must authenticate each endpoint and validate operation-specific response schemas.

## Filesystem protocol

All paths are already relative POSIX paths; root is `""` for `stat` and `list`.
No URL decoding or normalization occurs. Absolute paths, `.`/`..`, empty segments,
backslashes, controls, colons, >255-byte segments and >4096-byte paths are rejected.
Every operation traverses only the selected FSA directory handle. Permissions are
queried without prompting; revoked permission fails closed.

- `stat`: `{kind,size,lastModified}`; directory size/time are 0 because FSA exposes
  no portable directory metadata.
- `list`: `{entries:[{name,kind,size,lastModified}]}`; >10000 entries or >1 MiB of
  UTF-8 encoded JSON fails EOVERFLOW rather than silently losing entries.
- `read`: `offset,size` -> `{data:base64,bytes}`; maximum block 1 MiB, EOF yields an
  empty block. No UTF-8 conversion: arbitrary binary files are supported.
- `write`: `offset,data:base64` -> `{bytes}`; strict canonical base64 <=1 MiB,
  keepExistingData and positioned write preserve untouched file content. Omitted
  data is an empty write (Go omitempty); zero-byte writes never extend the file.
- `create`: `exclusive` and `truncate` are optional booleans, both default false.
  Existing files are returned without modifying content unless truncate=true.
  Exclusive creation fails EEXIST before truncation; directories fail EISDIR.
  Missing files are created. No parent directory is implicitly created.
- `mkdir`: create a new directory; existing entries fail EEXIST.
- `truncate`: `size` -> `{size}`; shrink/extend through the native writable stream.
- `unlink`, `rmdir`: enforce entry kind, never recursively delete.
- `rename`: `target`; native handle.move only. Missing native support returns
  ENOTSUP. No copy/delete fallback falsely promises atomic rename.
- `flush`: acknowledgement after verifying a file; every write/truncate already
  closes its writable stream before returning. This does not claim POSIX fsync.

Executor transactions are serialized to prevent overlapping FSA snapshots losing
updates. Offsets and sizes must fit non-negative JavaScript safe integers.
DOM failures map to errno-like errors (ENOENT, EACCES, ENOSPC, ENOTEMPTY, etc.).
Mutation timeout has unknown outcome and must not be blindly retried.

## User consent and limitations

The sidebar entry is a compact row above the ssh-workspace row (slot
`sidebar.footer.action`, order 90, label `浏览器工作区`). The shell renders this list slot
as a flex row and wraps each registration in a classless `div`; both workspace plugins ship
a scoped `:has()` rule that changes the shared footer to a column, so the two rows occupy
separate full-width lines. In the expanded sidebar the row contains an icon, the label and
a muted state note; in the collapsed rail only the icon remains. The row carries its phase
as `data-dshgw-state` (`mounted` while a directory is mounted, `failed` after the last
operation failed, everything else otherwise), which is what the browser tests assert — the
row itself deliberately has no status dot: the state note and the dialog say it in words.

**One click is the whole gesture** — it opens the readwrite picker synchronously, because
`showDirectoryPicker()` needs transient user activation (about five seconds in Chromium)
and any earlier step, a consent click or a blocking `window.confirm()` alike, spends that
clock before the picker is reached ("Must be handling a user gesture"). After the picker
returns, the browser row opens a frame-wide status dialog. It reports picking, mounting,
worker reconnect and the final result. A successful mount shows the green success state and
automatically closes the dialog after 1.5 seconds; a failure keeps the dialog open with a
red error until the user closes it. Closing the dialog never cancels an in-flight mount—the
row continues to report the real state. Cancelling the native picker is not a failure: the
dialog closes and the row returns to its previous state.

The warning therefore travels with the row and dialog instead of gating the picker: the
row's tooltip states that the AI can read, modify, rename and delete files in the selected
directory, that file contents may be sent to the configured AI model provider, and that
mounting and unmounting restart the account worker and can interrupt running tasks or
connections. Users should select a dedicated backed-up directory, never secrets or
credentials. The state note covers mounting, waiting for the worker to reconnect, active
sharing, disconnect and unconfirmed cleanup with an explicit retry action. A secure
context and File System Access browser support are still required.

FSA is not a complete POSIX filesystem: hard links, symlinks, chmod, ownership,
real directory timestamps and reliable external-writer exclusion are unavailable.
Exclusive creation checks cannot rule out races with external programs. Native
move availability and atomicity remain browser/platform capabilities. The picker
and real mount flow require secure-context Chromium integration verification;
Node tests below use fake handles and do not prove a production kernel mount.

## Tests

```sh
node --test cmd/dshgw/plugin/browser-workspace/*.test.mjs
```

Coverage includes binary roundtrips/offsets/sparse writes, create flags, truncation
and metadata, serialization, base64/range/JSON-size limits, path escape rejection,
read-only/revoked permission, typed deletion, rename ENOTSUP, same-origin HTTP and
timeouts. UI tests execute actual `apply()` against fake React/DSH services and
cover the complete picker/open/poll/respond/activate/reconnect/register/connect/
close contract, idempotent create retry, activation failure, cleanup failure and
retry with retained capability, disposal, and picker cancellation. One test pins the
sidebar contract (slot, order above the ssh entry, label, single-click row, warning
on the row) and asserts the picker is invoked synchronously by that click with no
consent step and no blocking dialog.
