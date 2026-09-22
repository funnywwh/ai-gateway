# dshgw-git-diff

A read-only git change review surface inside the dsh web GUI: a **变更** View in the session's main
area (beside 对话 and 轨迹), the changed files down the left, and a **two-column side-by-side diff** on
the right for whichever file you click.

This plugin is out-of-tree in the sense that matters: the published distributions ship no git
review, and everything third-party on npm targets the 0.1.5/0.1.6 client generation — the same
reason `dshgw-workspace-files` and `dshgw-web-tty` exist in this deployment. It composes the feature
out of what the installed 0.1.2-rc.1 shell actually offers.

This repository ships it at `cmd/dshgw/plugin/git-diff/`, beside the picker plugin the sandbox
already binds, and the gateway renders a row for it in every account's profile (M75) — so a tenant's
dsh has the 变更 View without anybody editing that account. A deployment that prefers the standalone
layout can still drop the directory into `$DSH_HOME/plugins/git-diff` and name it in that account's
own patch instead.

## What it does

- Adds a **变更** tab to the session's main area. The tab mounts only while it is selected, so nothing
  polls while you are reading the conversation.
- **Changed files on the left**: one row per path, with a status badge (`M`/`A`/`D`/`?`), the
  directory in a lighter tone and the file name in a brighter one, a `已暂存` / `未跟踪` tag where it
  applies, plus filter chips, a free-text path filter, and a running count per side.
- **Two columns on the right**: removed lines on the left, added lines on the right, each with its own
  line numbers, hunk headers, filler cells where one side has no counterpart, and the characters that
  actually changed highlighted inside a paired line. A 统一 toggle switches to the classic one-column
  patch, and a 换行 toggle wraps long lines instead of scrolling them.
- **Every comparison a file participates in**: 未暂存 (索引↔工作区), 已暂存 (HEAD↔索引), and 全部
  (HEAD↔工作区) when both exist, plus 新增 (未跟踪→工作区) for untracked files.
- **Partial scans** on a huge tree: the worktree set comes from a background scan that walks the
  repository one top-level directory at a time, cheapest first, reporting results as they arrive and
  cancellable at any point. A directory that blows its per-chunk budget is marked and skipped.
- **Scoped scans**: type a path (`frameworks/base`) into 扫描范围 and only that subtree is scanned —
  the fast path when you already know where you have been working.

## What it never does

- **It never writes the repository.** Every git invocation passes `--no-optional-locks` and the
  environment sets `GIT_OPTIONAL_LOCKS=0`, so git neither refreshes nor locks `.git/index`. There is no
  stage/unstage/discard/commit endpoint. `host.test.mjs` asserts that the index's bytes, size and mtime
  survive a full scan plus every diff side.
- **It never leaves the workspace.** `config.root` is both the discovery base and the clamp: repository
  paths are resolved through `realpath` and must live under it, and each diff path is rejected if it is
  absolute, contains `..`, a backslash or a NUL, or resolves — symlink included — outside the
  repository. Every path reaches git after `--`, so a name that looks like an option is data.
- **It never guesses.** The panel shows git's own diff text; the browser half parses it and nothing
  else. There is no second diff implementation that could disagree with `git diff` on the command line.

## Why this design: the workspace is a network filesystem

This deployment's workspace is an **sshfs mount of a 26 GiB AOSP checkout**: 805,368 tracked files, a
118 MB index, and every `lstat` a network round trip. Two consequences shape everything above, and both
were measured against the real repository rather than assumed:

| Operation | Measured |
|---|---|
| `git diff-index --cached --name-status HEAD` (index vs HEAD, no worktree) | ~1.2 s |
| repository discovery walk over the workspace root | ~0.2 s |
| `status` (metadata + staged set) | ~1.2 s |
| `planChunks` (one `git ls-files -z` pass over the index) | ~1.2 s |
| per-chunk invocation floor (each re-reads the 118 MB index) | ~1.2 s + ≈0.4 ms per tracked file in the chunk |
| full chunked worktree scan, one chunk per directory (31 chunks) | 130 s |
| full chunked worktree scan, packed at 25k files per chunk (13 chunks) | **101 s** |
| the same work in a single `diff-files` pass | 22 s |
| `git status --porcelain -uno` over the whole tree | **did not finish in 300 s** |
| `git diff -- <one file>` (porcelain) | **did not finish in 60 s** |
| `git diff-files -p -- <one file>` (plumbing) | 1.2 s |
| `diff --no-index` on an untracked 5 KB file | 0.03 s |

**1. The porcelain refreshes the index; the plumbing does not.** `git diff` refreshes the index before
it answers, and that refresh is a full worktree pass — which on this mount is minutes. The three
comparisons therefore go through `diff-files -p`, `diff-index -p --cached HEAD` and `diff-index -p HEAD`,
all of which answer in about a second and never touch the index.

**2. sshfs breaks git's stat cache, and `core.checkStat=minimal` fixes it.** git decides whether a
tracked file changed by comparing the stat data it recorded in the index — inode included — against the
worktree. An sshfs mount does not present the remote's inode numbers (index says `ino 23068684`, sshfs
says `929051` for the same file), so *every* tracked file looks stat-dirty. Without the setting, a plain
`diff-files --name-status` reported all 12,018 files under `a-ztc/` as modified on a tree that
`git show HEAD` proves is clean. `core.checkStat=minimal` compares mtime and size only — exactly what git
does on a local disk — so the panel agrees with the `git status` the author sees on the machine that
owns the checkout. `host.test.mjs` reproduces the condition (a replaced inode with the same mtime second
and size) and fails if either the setting or the plumbing choice is dropped.

**3. Every chunk re-reads the whole index, so chunks are packed.** The 31-chunk plan (one per
directory) spent about 108 s of its 130 s re-reading the index — 118 MB over sshfs — once per
invocation. Packing the cheapest directories together to ~25,000 tracked files each, while never
packing a directory that is bigger than the target on its own, brings the same scan down to 13 chunks
and 101 s. That is still 4-5× the 22 s a single pass costs: the difference is what progressive
results, cancellation and per-chunk timeout isolation cost, and it is worth it on a scan this slow.

The consequence for the UI is the two-tier list: the staged set is one index-level query and paints in
about a second, while the worktree set can only come from a scan that costs what the network costs. On
this tenant the full scan finds **386 genuinely modified files** — every one of them verified with a
content diff (`diff-files -p`) — including the Android layout XML, `common.h`'s
`BACKLIGHT_ON_MS 6000 → 3000 //zycheng modify`, and the release IDH scripts the author had not
committed yet.

## Architecture

```
browser (client.js)                             tenant node process (index.js)
  View  in conversation.view id 'git-diff'        ctx.connection.rpc.handle('/dshgw-git-diff')
    left  changed-file list  <-- incremental ---    rpc-handlers.js: hello/repos/status/scanStart/
    right two-column diff    <-- unified text ---     scanStatus/scanCancel/diff/diag
        |                                           git-service.js: one clamp, one scan job,
        |  POST /dshgw-git-diff/<endpoint>            in-memory results + cache.json
        `--------------------------------------->   git, read-only, over the sshfs mount
```

- **Transport**: `ctx.connection.rpc.call` — the same authenticated, same-origin channel the shipped
  ssh/browser-workspace plugins and the other `dshgw-*` plugins use. No websocket, no extra port, no
  second authentication story, no route of this plugin's own.
- **The scan is a job, not a request.** `scanStart` returns immediately with the plan; the browser polls
  `scanStatus` once a second and renders whatever has arrived. One job runs at a time: a second start
  returns the running job instead of racing it, and `force` cancels first.
- **Chunks are top-level directories, ordered cheapest first.** The plan is one chunk per top-level
  directory (plus one for the paths at the repository root, whose names come from the index so a deleted
  root file still keeps its slot), sorted by ascending tracked-file count: on this repository the first
  results arrive from `libnativehelper` (40 files) while `prebuilts` (240,939) and `external` (207,787)
  sit at the end of the queue where cancelling can skip them.
- **Failures are per chunk and coded.** A chunk that times out is marked `timeout` and the run
  continues; the browser shows the count and the failing directory names. Every endpoint answers
  `{ok:true,value}` or `{ok:false,error:{code,message,details}}` with a stable code
  (`git/not-a-repo`, `git/outside-root`, `git/too-large`, `git/timeout`, `git/unavailable`, …).
- **Results survive a restart.** A completed scan is written to `cache.json` (atomically, best-effort)
  and read back at activation, so a page refresh — or a server restart — still paints a list instantly.

## Files

| File | Role |
|---|---|
| `index.js` | host half: config, root resolution, RPC channel registration, trace log |
| `git-service.js` | the git domain — discovery, the clamp, index queries, the chunked scan, diffs; no dsh imports |
| `rpc-handlers.js` | endpoint table (shared with the tests) |
| `client.src.js` | browser half, hand-written |
| `build-client.mjs` | stamps `client.src.js` into `client.js` (nothing is vendored) |
| `client.js` | **generated** — the file dsh serves; edit `client.src.js` and rebuild |
| `trace.jsonl` | append-only diagnostics (activation, discovery, plan, per-chunk failures, every diff) |
| `cache.json` | the last completed scan of each repository, so a refresh paints at once |
| `test/host.test.mjs` | 24 tests: helpers, config, discovery, the clamp, scans, diffs, the read-only guarantee |
| `test/client.test.mjs` | 8 tests: the diff parser, then the built bundle driven against the real host half |

The bundle declares no top-level bindings, because dsh concatenates several plugin bundles into one
script scope; React comes from the shell's frozen module table, so the client half requires only `react`
and declares no externals.

## Enable / disable

The row lives in the **user-level** patch layer, `$DSH_HOME/cordis.patch.yml`, which the gateway never
rewrites:

```yaml
- insert:
    - id: dshgw-git-diff
      name: file:///home/winger/work/ai_gateway/data/dshgw-verify/state/tenants/dsh-tenant/.dsh/plugins/git-diff/index.js
      config:
        root: /home/winger/work/ai_gateway/data/dshgw-verify/state/workspaces/dsh-tenant
        rootLabel: 工作区
        maxRepoDepth: 6
        chunkTimeoutMs: 90000
        trace: true
```

The web profile declares `patchReload: live`, so the host half activates in the running server without a
restart (check `trace.jsonl` for the `activated` line). **The browser must be refreshed once** after the
row is added or `client.js` is rebuilt: the boot graph (`window.__DSH_BOOT__`) is re-rendered per index
request, and an already-running page ignores graph frames. To disable, delete the row or add
`disabled: true`; the RPC channel disappears with the fiber and any running scan is cancelled on
teardown.

### Reloading the host half

`client.js` is picked up by the client-module composer whenever it is rebuilt (the deployment's
`dsh-client-hmr` row polls bundle files), but a host module edited in place is **not** reloaded: dsh's
loader imports a row's module through Node's ESM cache, and this profile runs with the host-side `hmr`
row disabled, so re-applying the patch row would restart the row on top of the previous module bodies
until the whole gateway restarted.

Hence the `?rev=` suffix on the row's `name`. `index.js` reads the suffix off its own URL and tags its
`git-service.js` / `rpc-handlers.js` imports with it, so bumping the suffix gives the entire host graph
fresh module identities and a patch reload is enough:

```yaml
name: file:///…/.dsh/plugins/git-diff/index.js?rev=20260922b
```

The suffix is inert everywhere else — `fileURLToPath` ignores a query, so package discovery and the
client bundle still resolve — and `trace.jsonl`'s `activated` line records the settings that went live
(`chunkTargetFiles`, `chunkTimeoutMs`, `autoScan`), which is how you can tell which build is running.

## Configuration

All fields are optional; malformed values fall back to the defaults. Every number may also be given
inside a `limits` object (`limits.maxFiles`, …) instead of as a flat key.

| Field | Default | Meaning |
|---|---|---|
| `root` | `process.cwd()` (the tenant workspace) | The discovery base **and** the clamp; resolved through `realpath` at activation, and a root that does not exist fails activation loudly |
| `rootLabel` | the root's basename | How the root is labelled |
| `repo` | unset | Pin one work tree instead of discovering; it must still live under `root` |
| `maxRepoDepth` | `6` | How deep the discovery walk looks for a `.git` |
| `maxRepoCandidates` | `400` | Directories the discovery walk may visit in total |
| `chunkTimeoutMs` | `90000` | Budget for one chunk; a chunk that exceeds it is marked and skipped |
| `chunkTargetFiles` | `25000` | Roughly how many tracked files one chunk may cover, since each chunk re-reads the index; `0` scans one directory per chunk |
| `diffTimeoutMs` | `60000` | Budget for one file's diff |
| `autoScan` | `true` | Whether opening the tab starts the background scan by itself |
| `untracked` | `false` | Default for the 未跟踪 chip, adopted only by a browser with no remembered choice |
| `maxUntrackedEntries` | `2000` | Untracked files admitted per scan |
| `maxFiles` | `20000` | Changed entries kept per repository |
| `maxDiffBytes` | `2097152` | Largest file the diff reader will fetch (the browser is told `truncated`) |
| `trace` | `true` | Write `trace.jsonl` |
| `traceFile` | `<this plugin dir>/trace.jsonl` | Where that trace goes. The gateway names a per-tenant file (`<DshHome>/plugin-state/git-diff.trace.jsonl`) because one installed copy serves every account and a shared file would mix them together (M75). A relative or empty value is ignored, and the parent directory is created on first write. |
| `cacheFile` | `<this plugin dir>/cache.json` | The scan cache (repository paths and per-directory summaries). The gateway names a per-tenant file (`<DshHome>/plugin-state/git-diff.cache.json`) for the same reason — handing one account's repository paths to another is a disclosure, not a cache miss. A relative or empty value is ignored. |

## Endpoints

| Endpoint | Payload | Value |
|---|---|---|
| `hello` | — | `{version, root, rootLabel, git:{available,version}, repo, untracked, autoScan, limits}` |
| `repos` | `{refresh?}` | `{repos:[{path,rel,label,writable}], cachedAt}` — one entry per work tree, writable ones first |
| `status` | `{repo}` | `{repo, meta:{head,short,subject,branch,detached,unborn}, staged[], scan, files[], total}` — always fast (index level + cache) |
| `scanStart` | `{repo, scopes?, includeUntracked?, includeStaged?, force?}` | the job snapshot: `{jobId, state, plan[], chunksDone, chunksTotal, files[], total, truncated}` |
| `scanStatus` | `{jobId?}` | the same snapshot, at any point during the run |
| `scanCancel` | `{jobId?}` | the cancelled snapshot; the child is signalled and its slot freed |
| `diff` | `{repo, path, side, context?}` | `{path, side, oldLabel, newLabel, unified, binary, empty, truncated, stats:{additions,deletions}}` |
| `diag` | — | version, pid, node, git, root, pinned repo, discovered repos, live jobs, cached repositories, per-endpoint call counts |

## Verify

```sh
cd $DSH_HOME/plugins/git-diff
npm run build        # regenerate client.js after editing client.src.js
npm test             # 32 tests: host half against real repositories, then the built bundle end to end
tail -n 20 trace.jsonl   # activation, discovery, plan, per-chunk failures, every diff
```

Two of those tests exist to keep the two hard-won properties from regressing: the index-immutability
test (which would catch a return to the porcelain's index refresh) and the replaced-inode test (which
would catch a return to full `checkStat`).

`npm test` needs no browser and no network: the host tests build temporary git repositories on a real
filesystem (skipped with a clear message if `git` is missing), and the client tests mount the real
`client.js` with a stubbed DOM and a stub React against the real endpoint table, driving
open → staged list → scan → click a file → two-column diff → switch comparison → filter → rescan →
cancel.

In the GUI after a refresh: the main area gains a **变更** tab. On this tenant it opens on
`ssh/aipc/home/winger/ZT20Q` (the writable mount; the read-only `browser/ZT20Q` mirror of the same tree
is listed after it and marked 只读), shows the staged set at once, and starts a chunked scan that fills
the list in from the cheap end. `trace.jsonl` gains `{"event":"discover"}`, `{"event":"plan"}` and
`{"event":"scan-start"}`, then one `scan-end` with the per-chunk timings.

## Known limitations

- **A full scan of this repository takes minutes, not seconds.** 805,368 files must be stat-ed over the
  network; there is no way around that except scoping the scan, which is why 扫描范围 exists and why the
  chunks are ordered cheapest-first and cancellable.
- **Stat-based, like git itself.** With `core.checkStat=minimal` a file whose size *and* whole-second
  mtime are unchanged is not re-read; that is git's normal behaviour on a local disk, and it is why the
  panel can only be as accurate as `git status` on the machine that owns the checkout.
- **No rename detection** (`--no-renames` everywhere, for speed): a rename shows as a deletion plus an
  addition.
- **Read-only by design.** No staging, unstaging, discarding, committing or stash operations — this
  panel reviews, it does not change anything.
- **Untracked scanning is opt-in and bounded.** This workspace has an un-ignored `out/` build tree with
  millions of files; enumerating it would be exactly the crawl the design avoids, so the 未跟踪 chip
  rescans with a hard entry cap.
- **A long diff is cut off, not windowed.** More than 4000 rendered rows or a file over `maxDiffBytes`
  is reported as truncated rather than virtualized.
- **The list renders at most 1500 rows** before it asks you to filter; a repository with tens of
  thousands of changes is a list to narrow, not to scroll.
