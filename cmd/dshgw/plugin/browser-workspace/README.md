# Browser workspace filesystem client

The plugin follows `ssh-workspace`'s Cordis/ModuleLoader packaging. `index.js`
only activates the client package: the **gateway**, not the tenant Node worker,
owns authenticated reverse HTTP, request queues, FUSE mounts and worker restarts.

One account can have **several local directories** mounted at once. Each one is a *folder*
here: a saved entry (a stable key, the directory handle it was granted, and the workspace it
maps to) that is either connected or not. The sidebar row is the entry point — one click does
the obvious thing — and the icon at its right opens the folder list, where a person adds,
connects, disconnects and deletes folders.

## Gateway HTTP contract

All requests are same-origin JSON POSTs to `./browser-workspace/<endpoint>` with
same-origin credentials; redirects are rejected. Responses are
`{ok:true,value}` or `{ok:false,error:{code,message}}`.

| Endpoint | Payload | Value |
| --- | --- | --- |
| `open` | `{name,writable:true,key?}` | `{token,mountpoint,id}` (mount pending) |
| `poll` | `{token}` | `{requests:[{id,op,path,offset?,size?,data?,target?,exclusive?,truncate?}]}` |
| `respond` | `{token,id,result:{ok,value?,error?}}` | acknowledgement |
| `activate` | `{token}` | `{mountpoint,id}` after mount/worker restart |
| `resume` | `{token}` | `{id,mountpoint,resumed}` — takes an existing mount back |
| `close` | `{token,purge?}` | acknowledgement |

`key` is the folder's **stable directory key**: 32 lowercase hex characters, generated once by
the client and kept with the folder. The gateway mounts that folder at
`<workspace>/browser/<key>`, so the same local directory always mounts at the same virtual
path. A key another share still holds — serving, or waiting out its reconnect grace — is
refused (`directory key already mounted for this account`) instead of mounted twice; a key
without one gets a fresh random id (an older client), whose mount point is per-mount scratch.

`purge` on `close` is the operator removing the folder for good: the mount point and its record
are released instead of kept for the next mount of that key. A plain `close` keeps the empty
mount point, because that directory *is* the virtual path the account's own workspace entry
points at.

**A gateway that predates this client** rejects both extra fields outright (its JSON decoder
runs with `DisallowUnknownFields`), so a page loaded just before a gateway restart would lose
the ability to mount at all. The client recognises that one answer and degrades instead of
failing: `open` is retried without the key (the mount works, at the old per-mount path, and a
warning says so) and a `purge` that is refused falls back to a plain close (an older gateway
removes every mount point itself). Pages already open keep working across a rolling restart;
a reload picks up the new bundle and the stable keys.

The client starts poll **before** activate and continues during worker restart. Activate and close use 55-second client timeouts to accommodate a 45-second worker
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
gateway run the old teardown: restart worker, unmount, and remove the record (the
mount point itself stays for a stable key).

`resume` is refused while another browser side holds the mount's poll connection,
and the replaced side is then told `directory revoked` so it stops instead of
fighting the live page. Because a request the browser never sent (a blocked or
dropped connection) leaves the gateway still believing the old poll is alive, the
client's own reconnect only succeeds once that request has actually ended — one
retry later, not forever. A refusal that means "another page serves this directory"
is final for the click: the folder is marked as served elsewhere rather than mounted
again, because two FUSE mounts of one local directory let two browsers write the same
files at once.

Workspace `create(path)` runs BEFORE activation, while the mount point is
guaranteed to exist: `open` creates or reuses that directory, and registering after
the activation restart raced the teardown and failed with `workspace/invalid-path:
ENOENT realpath`. `create` is idempotent **by path**, which is what makes the stable
key worth having: the same local directory returns the same workspace id, the same
title and the same sessions, across disconnects, reloads and gateway restarts.
Registration is retried for up to 10 seconds (each call bounded to 5 seconds), and a
mount that fails after registering deletes the workspace **only if it created it** —
an existing workspace is the mapping, and deleting it would throw its sessions away.

After activation the client waits for a fresh connected DSH generation using the
public `connection.state`, `connection.generation` and `connection.reconnect`
services. It then best-effort renames the workspace and invokes
`uiWorkspace.connectWorkspace` once. Session opening is deliberately not retried
because it can create a second session.

A transport failure is recovered **in place**: the page asks the gateway to resume,
keeps serving, and never needs a reload, a click or a second directory choice. Only
a `resume` the gateway refuses (the capability is gone: `unknown directory
capability`, or `directory revoked` because another page took it over) ends that
mount — `unknown directory capability` clears the token and the next click mounts
the folder again at the SAME path, while `directory revoked` leaves the folder to the
page that took it over.

## The saved folder list (IndexedDB)

The list is keyed by the tenant origin and holds one record per folder:

```js
{ version: 2, key, name, workspaceId, token, mountpoint, at, handle }
```

* `key` — the stable directory key (32 hex). It names the mount point, so it is the
  mapping; it never changes for a folder, not even when the directory is re-picked.
* `handle` — a `FileSystemDirectoryHandle` stored in IndexedDB is a REAL handle again in the
  next document, and a granted `readwrite` permission survives the reload (measured in
  Chromium: `queryPermission` returns `granted` with no user gesture, and reads and writes both
  work). That is what lets a reload reconnect without opening the directory dialog.
* `token` + `at` — the capability and when it was taken. The token is the perishable part: on
  boot it is kept only while the gateway could still hold that mount, and dropped otherwise.
  The FOLDER is never dropped by a stale token — only an explicit deletion removes it.
* `workspaceId` — kept across a disconnect, because the workspace entry is kept too.

The v2 list lives under a NEW key (`folders:<origin>`); v1 stored exactly one mount under
`mount:<origin>`. A v1 record is adopted once (its random id becomes the folder's key, so the
path its workspace entry already points at stays valid) and that key is then removed. Keeping
the two apart means an older bundle running in another tab cannot clear the v2 list it cannot
parse.

Each folder is serialised through one chain, so a connect that finishes while a second folder
is being added cannot overwrite the list the other one just wrote.

## The window, and what a click does

The row's right-hand icon opens the folder list: one row per saved folder (name, state, and
its own 连接/断开, 打开 and 删除 buttons) plus 添加文件夹 and 关闭. 删除 needs a second,
deliberate click (确认删除) and never uses `window.confirm`: the picker and the dialog both
need the click's own task, and a blocking confirm is what this plugin must not put in front of
them.

The row body is the adaptive one-click gesture:

| Saved folders | A click on the row body |
| --- | --- |
| none | opens the readwrite picker and mounts what the person chooses (today's gesture) |
| exactly one | that folder's connect (resume, or mount) or disconnect |
| several | opens the folder list — "disconnect which one?" has no obvious answer |

`showDirectoryPicker()` is called **synchronously inside the click** (no await in front of it),
because it needs transient user activation — about five seconds in Chromium — and any earlier
step, a consent click or a blocking dialog alike, spends that clock before the picker is
reached ("Must be handling a user gesture").

The window is a management surface, so it does not close itself; only the row's own one-click
mount does, 1.5 seconds after a mount succeeds. A failure keeps the window open with the
reason, and the row keeps reporting the real state either way.

## Folder states

| State | Meaning |
| --- | --- |
| `connected` | this page serves that mount right now |
| `disconnected` | saved and offline; `ready === false` means the grant is gone and connecting will ask for the directory again |
| `resumable` | the gateway may still hold that mount (`token` fresh) — one click takes it back with `resume`, no worker restart |
| `connecting` / `disconnecting` | the operation is running (that folder's buttons are disabled) |
| `error` | the last operation failed; `retry` says whether the retry is a connect or a close |
| `elsewhere` | another page of this account serves that directory |

The row carries the aggregate phase as `data-dshgw-state` (`idle`, `disconnected`,
`resumable`, `mounted`, `failed`, or the running operation) and the folder rows carry
`data-dshgw-folder-state`; both are what the real-browser runs assert, because the row
deliberately has no status dot — the state note and the window say it in words.

## User consent and limitations

The sidebar entry is a compact row above the ssh-workspace row (slot
`sidebar.footer.action`, order 90, label `浏览器工作区`). The shell renders this list slot
as a flex row and wraps each registration in a classless `div`; both workspace plugins ship
a scoped `:has()` rule that changes the shared footer to a column, so the two rows occupy
separate full-width lines. The browser row is a container (`.dshgw-bw-row`) holding the row
body and the folder icon; the collapsed rail keeps one icon and hides the rest.

The warning travels with the row and the window instead of gating the picker: the row's
tooltip states that the AI can read, modify, rename and delete files in the selected
directory, that file contents may be sent to the configured AI model provider, and that
connecting and disconnecting restart the account worker and can interrupt running tasks or
connections. Users should select a dedicated backed-up directory, never secrets or
credentials. A secure context and File System Access browser support are still required.

Up to 8 folders may be SAVED per browser profile. How many may be MOUNTED at once is the
gateway's bound (currently 4 per account, and one long-poll connection per mount from the
page); exceeding it is reported in the window in words the operator can act on. Deleting a
folder releases its virtual path and its workspace entry; disconnecting keeps both, which is
what makes "reconnect" return to the same workspace with the same sessions.

FSA is not a complete POSIX filesystem: hard links, symlinks, chmod, ownership,
real directory timestamps and reliable external-writer exclusion are unavailable.
Exclusive creation checks cannot rule out races with external programs. Native
move availability and atomicity remain browser/platform capabilities. The picker
and real mount flow require secure-context Chromium integration verification;
Node tests below use fake handles and do not prove a production kernel mount.

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

## Tests

```sh
node --test cmd/dshgw/plugin/browser-workspace/*.test.mjs
```

`harness.mjs` is the one fake browser the behaviour tests share: IndexedDB with real request
ordering, a picker that answers with whichever directories the test queues, a stateful fake
gateway that hands out one capability per mounted key, and the DSH services the plugin
injects. On top of it:

- `client.test.mjs`, `limits.test.mjs`, `large-block.test.mjs` — the executor: binary
  roundtrips/offsets/sparse writes, create flags, truncation, serialization,
  base64/range/JSON-size limits, path escape rejection, read-only/revoked permission, typed
  deletion, rename ENOTSUP, same-origin HTTP and timeouts.
- `ui.test.mjs` — one directory: the row contract (two controls, phases, the stacking rule),
  the synchronous picker, open → poll → create → activate → reconnect ordering, a failed
  activation (including "do not delete a workspace this mount did not create"), an
  unconfirmed cleanup and its retry, disposal, and a cancelled picker changing nothing.
- `manage.test.mjs` — several directories: adding two, mounting each at its own key, the
  adaptive row click, disconnecting one while the other keeps serving, reconnecting at the
  same path and workspace, the two-step deletion, the per-account limit in words, a
  duplicate directory in the list, and a re-picked directory keeping its key.
- `reconnect.test.mjs` — what survives a page: offering a stored mount without opening the
  picker, a withdrawn grant, a refused resume falling back to the same directory, an expired
  capability that keeps the folder, an in-page reconnect, and a browser tab that serves the
  directory being reported instead of mounted twice.
