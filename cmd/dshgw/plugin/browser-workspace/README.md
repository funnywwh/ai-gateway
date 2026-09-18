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
| `close` | `{token}` | acknowledgement |

The client starts poll **before** activate and continues during worker restart.
Activate and close use 55-second client timeouts to accommodate a 45-second worker
restart; other requests time out at 35 seconds. Gateway poll must return within
that deadline. Empty polls are paced at 100ms. No filesystem mutation is replayed.

After activation the client waits for a fresh connected DSH generation using the
public `connection.state`, `connection.generation` and `connection.reconnect`
services. Workspace `create(path)` is idempotent and retried for up to 30 seconds
while the worker reconnects (each call bounded to 5 seconds). It then best-effort
renames the workspace and invokes `uiWorkspace.connectWorkspace` once. Session
opening is deliberately not retried because it can create a second session.

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

The sidebar requires a secure context and File System Access browser support.
Before the readwrite picker, confirmation explicitly explains that the AI can
read/write/delete files, file contents may be sent to the configured AI model
provider, and mounting/unmounting restarts the account worker and can interrupt
running tasks/connections. Users should select a dedicated backed-up directory,
never secrets or credentials. Status includes pending mount/reconnection, active
sharing, disconnect and unconfirmed cleanup with an explicit retry action.

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
retry with retained capability, disposal, and picker cancellation.
