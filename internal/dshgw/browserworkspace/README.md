# Browser workspace FUSE adapter

This package adapts a context-aware request/response `Backend` to go-fuse. It
contains **no mount persistence or tenant-path policy**: `browsermount` owns the
lifecycle, authorization and mount location. The unused earlier `Store`, `Mount`,
`Mountpoint` and `ValidateRelative` prototypes were removed because their mount
layout and `"."` root disagreed with the live protocol. The protocol root is `""`.

## Supported behavior and explicit limitations

- Operations: stat/list/read/write/create/mkdir/unlink/rmdir/truncate/rename/flush.
  File/directory modes are synthetic 0644/0755; requested creation modes are not
  persisted. chmod/chown and explicit timestamp changes return `EOPNOTSUPP`, even
  when combined with a size change. No metadata mutation is silently accepted.
- `exclusive` and `truncate` booleans carry create intent. Existing-file
  `O_TRUNC` goes through kernel Setattr. FSA has no atomic exclusive-create
  primitive: a browser can serialize its own operations and check existence,
  but cannot exclude other tabs or native programs racing the operation.
- Successful FUSE rename uses go-fuse's inode-tree update. Repeated lookup reuses
  an existing child inode, so sequential renames of a file or its ancestor
  directory are reflected by nested already-open handles, including truncate.
  **Whether rename works at all depends on the selected browser handle's native
  move capability**. The browser must return `EOPNOTSUPP` when unavailable, not
  emulate it with copy/delete. Atomic replace and all POSIX rename guarantees
  are not promised by File System Access. Rename flags are unsupported.
- Handles are path-based, not durable browser object IDs. Open-after-unlink,
  replaced-file identity, external rename tracking and concurrent external
  namespace mutation do not have full POSIX semantics. An external replacement
  at the same path can be seen by an existing handle. Do not use this adapter
  for databases or workloads requiring locking, atomic transactions or strict
  POSIX namespace semantics. Symlinks, hard links, fsync durability, mmap and
  permission/ownership preservation are not implemented.
- Writes are acknowledged only by the backend; browser writable-stream close
  is not an OS-level fsync guarantee. Flush contacts the backend rather than
  returning local success after a disconnect.
- Attribute, positive-entry and negative-entry TTLs are zero; files use direct
  I/O, and directory contents are not kernel-cached. An already-created directory
  stream is still a snapshot. This is not a cross-client consistency guarantee.
- Every backend request receives a deadline (default 30 seconds), and the
  backend **must honor context cancellation**. Deadline errors become
  `ETIMEDOUT`, cancellation becomes `EINTR`, transport/disconnect and malformed
  responses become `EIO`. Named remote POSIX errors are preserved; unknown codes
  fail closed as `EIO`. A timeout does not prove a mutation was rolled back.
- Stat/lookup require a nonnegative size and exactly `file` or `directory` kind.
  Invalid entry names, duplicate directory entries and oversized read/write
  results are rejected. Response matching/authentication belongs to the backend.
- Mount identity is filesystem `fuse.browser-workspace`, source
  `dshgw-browser-workspace`, for lifecycle code to distinguish it from unrelated
  mounts. MountFS itself does not enforce tenant policy or symlink-safe paths.

## Verification

```sh
source scripts/goenv.sh
go test ./internal/dshgw/browserworkspace
BROWSERWORKSPACE_FUSE_TEST=1 go test -v ./internal/dshgw/browserworkspace -count=1
# Optional race run requires a C compiler in PATH:
CGO_ENABLED=1 BROWSERWORKSPACE_FUSE_TEST=1 go test -race ./internal/dshgw/browserworkspace
```

The opt-in test mounts the real kernel FUSE implementation with an isolated
**temporary-disk mock backend**, then unmounts via cleanup before removing temp
directories. It covers mkdir, read/write, truncate, file/directory rename with
nested already-open handles, create flags, chmod/timestamp rejection, external
changes/zero caches, disconnect errors, deadline errors, unlink and rmdir.
The mock intentionally supports POSIX rename: its success verifies the adapter,
**not** browser support for directory move or atomic replacement. Browser client
capability tests and manual browser authorization remain separate requirements.
No test deploys services or uses a real user directory.
