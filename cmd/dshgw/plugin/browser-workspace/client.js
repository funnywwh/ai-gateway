// Hand-built ModuleLoader bundle, served byte-for-byte like ssh-workspace.
window.__ModuleLoader__.load({
  id: 'dshgw-browser-workspace',
  factory: require => {
    const React = require('react')
    const MAX_BYTES = 1024 * 1024
    // How long the success dialog stays up before it closes itself. Exported for the unit
    // test, which fires that one timer by hand instead of sleeping.
    const AUTO_CLOSE_MS = 1500
    const failure = (code, message) => Object.assign(new Error(message), { code })
    function checkRelativePath(path, allowRoot = true) {
      if (typeof path !== 'string' || new TextEncoder().encode(path).length > 4096 || /[\\:\u0000-\u001f\u007f]/.test(path)) throw failure('EINVAL', 'invalid relative path')
      if (path === '' && allowRoot) return path
      if (!path || path.split('/').some(s => !s || s === '.' || s === '..' || new TextEncoder().encode(s).length > 255)) throw failure('EINVAL', 'path must contain only normal relative segments')
      return path
    }
    function number(value, label, maximum = Number.MAX_SAFE_INTEGER) {
      if (!Number.isSafeInteger(value) || value < 0 || value > maximum) throw failure('EINVAL', `invalid ${label}`)
      return value
    }
    function decode(data) {
      if (typeof data !== 'string' || data.length > Math.ceil(MAX_BYTES / 3) * 4 || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(data)) throw failure('EINVAL', 'invalid base64 block')
      const raw = atob(data)
      if (raw.length > MAX_BYTES || btoa(raw) !== data) throw failure('EINVAL', 'noncanonical or oversized base64 block')
      return Uint8Array.from(raw, c => c.charCodeAt(0))
    }
    function encode(bytes) {
      let raw = ''
      for (let i = 0; i < bytes.length; i += 8192) raw += String.fromCharCode(...bytes.subarray(i, i + 8192))
      return btoa(raw)
    }
    function errorOf(error) {
      const codes = { NotFoundError: 'ENOENT', NotAllowedError: 'EACCES', SecurityError: 'EACCES', TypeMismatchError: 'EINVAL', InvalidModificationError: 'ENOTEMPTY', NoModificationAllowedError: 'EROFS', QuotaExceededError: 'ENOSPC', NotSupportedError: 'ENOTSUP', AbortError: 'EIO' }
      return { code: typeof error.code === 'string' ? error.code : codes[error.name] || 'EIO', message: error.message || String(error) }
    }
    function createExecutor(root, { writable = false } = {}) {
      const metadata = async handle => {
        if (handle.kind === 'directory') return { kind: 'directory', size: 0, lastModified: 0 }
        const file = await handle.getFile()
        return { kind: 'file', size: file.size, lastModified: file.lastModified }
      }
      const locate = async path => {
        const parts = path ? path.split('/') : []
        const leaf = parts.pop()
        let parent = root
        for (const segment of parts) parent = await parent.getDirectoryHandle(segment)
        return { parent, leaf }
      }
      const lookup = async ({ parent, leaf }) => {
        if (leaf === undefined) return root
        try { return await parent.getDirectoryHandle(leaf) }
        catch (error) { if (error.name !== 'TypeMismatchError') throw error }
        return parent.getFileHandle(leaf)
      }
      const absent = async location => {
        try { await lookup(location) } catch (error) { if (error.name === 'NotFoundError') return; throw error }
        throw failure('EEXIST', 'entry already exists')
      }
      const truncate = async (handle, size) => {
        const stream = await handle.createWritable({ keepExistingData: true })
        try { await stream.truncate(size); await stream.close() }
        catch (error) { await stream.abort().catch(() => {}); throw error }
      }
      const run = async request => {
        const { op, path, target, data } = request || {}
        if (!['stat', 'list', 'read', 'write', 'create', 'mkdir', 'unlink', 'rmdir', 'truncate', 'rename', 'flush'].includes(op)) throw failure('ENOTSUP', 'unsupported operation')
        checkRelativePath(path, ['stat', 'list'].includes(op))
        const mutation = ['write', 'create', 'mkdir', 'unlink', 'rmdir', 'truncate', 'rename'].includes(op)
        if (mutation && !writable) throw failure('EROFS', 'directory is read-only')
        let bytes, offset, size
        if (op === 'create') for (const flag of ['exclusive', 'truncate']) {
          if (request[flag] !== undefined && typeof request[flag] !== 'boolean') throw failure('EINVAL', `invalid ${flag} flag`)
        }
        if (op === 'read' || op === 'write') offset = number(request.offset ?? 0, 'offset')
        if (op === 'read') size = number(request.size ?? 0, 'read size', MAX_BYTES)
        if (op === 'truncate') size = number(request.size ?? 0, 'truncate size')
        if (op === 'write') bytes = decode(data ?? '')
        if ((op === 'read' || op === 'write') && !Number.isSafeInteger(offset + (size ?? bytes.length))) throw failure('EINVAL', 'range exceeds safe integer')
        if (op === 'rename') checkRelativePath(target, false)
        if (root.queryPermission && await root.queryPermission({ mode: writable ? 'readwrite' : 'read' }) !== 'granted') throw failure('EACCES', 'directory permission revoked')
        const location = await locate(path)
        const { parent, leaf } = location
        if (op === 'mkdir') {
          await absent(location)
          return metadata(await parent.getDirectoryHandle(leaf, { create: true }))
        }
        if (op === 'create') {
          let handle
          try { handle = await lookup(location) } catch (error) { if (error.name !== 'NotFoundError') throw error }
          if (handle && request.exclusive === true) throw failure('EEXIST', 'entry already exists')
          if (handle?.kind === 'directory') throw failure('EISDIR', 'cannot create over directory')
          handle ||= await parent.getFileHandle(leaf, { create: true })
          if (request.truncate === true) await truncate(handle, 0)
          return metadata(handle)
        }
        const handle = await lookup(location)
        if (op === 'stat') return metadata(handle)
        if (op === 'list') {
          if (handle.kind !== 'directory') throw failure('ENOTDIR', 'not a directory')
          const entries = []
          let resultBytes = new TextEncoder().encode('{"entries":[]}').length
          for await (const [name, child] of handle.entries()) {
            if (entries.length >= 10000) throw failure('EOVERFLOW', 'directory exceeds 10000 entries')
            checkRelativePath(name, false)
            const entry = { name, ...await metadata(child) }
            resultBytes += new TextEncoder().encode(JSON.stringify(entry)).length + (entries.length ? 1 : 0)
            if (resultBytes > MAX_BYTES) throw failure('EOVERFLOW', 'directory result exceeds 1 MiB')
            entries.push(entry)
          }
          return { entries }
        }
        if (op === 'rename') {
          const destination = await locate(target)
          if (typeof handle.move !== 'function') throw failure('ENOTSUP', 'browser does not support atomic move')
          await handle.move(destination.parent, destination.leaf)
          return {}
        }
        if (op === 'unlink' || op === 'rmdir') {
          if (op === 'unlink' && handle.kind === 'directory') throw failure('EISDIR', 'cannot unlink a directory')
          if (op === 'rmdir' && handle.kind !== 'directory') throw failure('ENOTDIR', 'not a directory')
          await parent.removeEntry(leaf, { recursive: false })
          return {}
        }
        if (handle.kind !== 'file') throw failure('EISDIR', 'not a file')
        if (op === 'flush') return {} // Writes close/commit before acknowledgement.
        if (op === 'read') {
          const file = await handle.getFile()
          const block = new Uint8Array(await file.slice(offset, offset + size).arrayBuffer())
          return { data: encode(block), bytes: block.length }
        }
        if (op === 'truncate') { await truncate(handle, size); return { size } }
        if (bytes.length === 0) return { bytes: 0 }
        const stream = await handle.createWritable({ keepExistingData: true })
        try { await stream.write({ type: 'write', position: offset, data: bytes }); await stream.close() }
        catch (error) { await stream.abort().catch(() => {}); throw error }
        return { bytes: bytes.length }
      }
      // Serial FSA transactions prevent concurrent writable snapshots losing data.
      let queue = Promise.resolve()
      return request => {
        const result = queue.then(() => run(request))
        queue = result.catch(() => {})
        return result
      }
    }
    // One compact sidebar row, stacked directly above the ssh-workspace entry: an icon,
    // a label, and a muted state note that truncates instead of widening the foot.
    const CSS = `
.dshgw-bw-action { display: flex; align-items: center; gap: 6px; width: 100%; background: none; border: 0; color: inherit; font: inherit; cursor: pointer; padding: 6px 8px; border-radius: 6px; text-align: left; }
.dshgw-bw-action:hover { background: rgba(127,127,127,.14); }
.dshgw-bw-action[aria-pressed="true"] .dshgw-bw-label { font-weight: 600; }
.dshgw-bw-label { flex: none; white-space: nowrap; }
.dshgw-bw-state { flex: 1 1 auto; min-width: 0; margin-left: 4px; opacity: .65; font-size: 12px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.dshgw-bw-action-rail .dshgw-bw-label, .dshgw-bw-action-rail .dshgw-bw-state { display: none; }
.dshgw-bw-backdrop { position: fixed; inset: 0; display: flex; align-items: center; justify-content: center; background: rgba(0,0,0,.45); z-index: 40; }
.dshgw-bw-dialog { width: min(520px, 92vw); max-height: 86vh; overflow: auto; background: var(--dsh-bg, #1b1c1f); color: var(--dsh-fg, #e6e6e6); border: 1px solid rgba(127,127,127,.35); border-radius: 10px; padding: 16px 18px; font-size: 13px; line-height: 1.5; }
.dshgw-bw-dialog h2 { margin: 0 0 4px; font-size: 15px; }
.dshgw-bw-dialog p.hint { margin: 0 0 12px; opacity: .7; }
.dshgw-bw-dialog button { background: rgba(127,127,127,.18); color: inherit; border: 1px solid rgba(127,127,127,.35); border-radius: 6px; padding: 6px 10px; font: inherit; cursor: pointer; }
.dshgw-bw-status { margin: 8px 0; }
.dshgw-bw-error { border: 1px solid #b3453c; background: rgba(179,69,60,.18); border-radius: 6px; padding: 8px 10px; margin: 8px 0; white-space: pre-wrap; }
.dshgw-bw-notice { border: 1px solid #3a7d44; background: rgba(58,125,68,.18); border-radius: 6px; padding: 8px 10px; margin: 8px 0; }
.dshgw-bw-muted { opacity: .65; }
.dshgw-bw-footer { display: flex; justify-content: flex-end; gap: 8px; margin-top: 12px; }
/* The shell renders this whole slot as one flex ROW, which leaves two entries sharing a foot
   that only fits one — so each of them is squeezed to half width. A plugin owns no wrapper
   element around its own row: the list slot wraps every registration in a classless div, so
   the shell's container is TWO levels up, and :has() is the only selector that can reach it
   from here. Both shapes are covered (the direct one too, in case the wrapper ever goes
   away), and both rows ship this rule, so it holds whichever of the two plugins a deployment
   enables. */
div:has(> .dshgw-bw-action), div:has(> div > .dshgw-bw-action),
div:has(> .dshgw-ssh-action), div:has(> div > .dshgw-ssh-action) { flex-direction: column; }
`
    // What the entry promises before anyone clicks it. The consent step that used to
    // stand in front of the picker is gone (it cost a second click and could burn the
    // click's transient user activation), so this warning travels with the row instead.
    const WARNING = '把本机目录挂载成这个账号的真实工作区：AI 可以读取、修改、重命名和删除所选目录中的文件，读取的文件内容可能发送给配置的 AI 模型服务商；挂载和卸载都会重启该账号的 worker，可能中断正在运行的任务或连接。请选择专用目录并先备份，不要选择密钥或敏感目录；挂载期间必须保持此页面打开。'

    /** Install the stylesheet once, namespaced by plugin like every shipped surface. */
    function installCSS() {
      if (document.querySelector('style[data-plugin-css="dshgw-browser-workspace/client.css"]') !== null) return
      const tag = document.createElement('style')
      tag.dataset.pluginCss = 'dshgw-browser-workspace/client.css'
      tag.textContent = CSS
      document.head.appendChild(tag)
    }

    function createTransport(fetcher = window.fetch.bind(window)) {
      return async (endpoint, payload, timeoutMs = 35000, signal) => {
        if (!['open', 'poll', 'respond', 'close', 'activate', 'resume'].includes(endpoint)) throw failure('EINVAL', 'invalid endpoint')
        const controller = new AbortController()
        const abort = () => controller.abort()
        if (signal?.aborted) abort()
        signal?.addEventListener('abort', abort, { once: true })
        const timer = setTimeout(abort, timeoutMs)
        try {
          const response = await fetcher(`./browser-workspace/${endpoint}`, {
            method: 'POST', credentials: 'same-origin', cache: 'no-store', redirect: 'error',
            headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload), signal: controller.signal,
          })
          if (!response.ok) throw failure('EIO', `gateway HTTP ${response.status}`)
          const result = await response.json()
          if (!result?.ok) throw failure(result?.error?.code || 'EIO', result?.error?.message || 'gateway did not answer')
          return result.value
        } finally { clearTimeout(timer); signal?.removeEventListener('abort', abort) }
      }
    }
    // ── the persisted mount record ────────────────────────────────────────────────
    //
    // A reload is not an unmount. The gateway keeps the kernel mount and its mount point for
    // a grace window when the page goes away (the poll connection dying is what starts it),
    // so the only thing a returning page needs is what the old document knew: the capability
    // token, and the directory handle it was serving.
    //
    // Both survive a reload in the browser — a FileSystemDirectoryHandle stored in
    // IndexedDB is a REAL handle again in the next document, and a granted 'readwrite'
    // permission stays granted across the reload and across a new tab (measured in Chromium:
    // queryPermission returns 'granted' with no user gesture, and read/write both work).
    //
    // The record is NOT resumed on load. Recovery happens when the operator clicks the row:
    // that click is what "打开工作区" means, and it must land on the mount that is already
    // there instead of building a second one. So the record is only read during boot (to put
    // the row in its "可恢复" state) and consumed by a click.
    //
    // Everything here degrades rather than lies: no IndexedDB, no stored handle, or a
    // permission the browser wants re-confirmed all leave the mount alone to expire, and the
    // operator's click is an ordinary mount.
    const RECORD_DB = 'dshgw-browser-workspace'
    const RECORD_STORE = 'mounts'
    // How long the gateway keeps a mount waiting for a page that went away. The page cannot
    // know this number (it is the gateway's), so it is mirrored here only to decide when a
    // stored record has gone stale enough to stop advertising a resume that cannot work.
    const RECONNECT_GRACE_MS = 45000

    function openRecordDB(indexedDB = window.indexedDB) {
      if (!indexedDB) return Promise.reject(failure('ENOTSUP', 'IndexedDB unavailable'))
      return new Promise((resolve, reject) => {
        const request = indexedDB.open(RECORD_DB, 1)
        request.onupgradeneeded = () => { request.result.createObjectStore(RECORD_STORE) }
        request.onsuccess = () => resolve(request.result)
        request.onerror = () => reject(request.error || failure('EIO', 'IndexedDB open failed'))
        request.onblocked = () => reject(failure('EIO', 'IndexedDB blocked'))
      })
    }

    // The record is keyed by the origin the page is on: one tenant origin owns exactly one
    // mount at a time, and a record left by a different tenant must never be resumed here
    // (the gateway would refuse it — the capability is bound to tenant and session — but not
    // offering it at all is the honest behaviour).
    function createRecordStore() {
      const key = () => 'mount:' + window.location.origin
      const withStore = async (mode, work) => {
        const db = await openRecordDB()
        try {
          return await new Promise((resolve, reject) => {
            const tx = db.transaction(RECORD_STORE, mode)
            const request = work(tx.objectStore(RECORD_STORE))
            tx.oncomplete = () => resolve(request ? request.result : undefined)
            tx.onerror = () => reject(tx.error || failure('EIO', 'record transaction failed'))
            tx.onabort = () => reject(tx.error || failure('EIO', 'record transaction aborted'))
          })
        } finally { db.close() }
      }
      return {
        load: () => withStore('readonly', store => store.get(key())).catch(() => null),
        save: record => withStore('readwrite', store => store.put(record, key())).catch(() => undefined),
        clear: () => withStore('readwrite', store => store.delete(key())).catch(() => undefined),
      }
    }

    // What the executor needs to reach the picked directory again in a NEW document. The
    // permission is asked for explicitly: queryPermission is what a browser answers without a
    // gesture, and a 'prompt' answer means this page cannot reattach on its own — the row
    // then offers a fresh mount instead, which does raise the picker inside a real click.
    async function reopenHandle(handle) {
      if (!handle || typeof handle.queryPermission !== 'function') return null
      if (await handle.queryPermission({ mode: 'readwrite' }) === 'granted') return handle
      return null
    }

    // A stored record is worth offering only while the gateway could still be holding that
    // mount. Nothing here decides anything on its own: the click asks the gateway, and its
    // answer ("unknown directory capability") is what rules a record out.
    function recordIsFresh(record) {
      return !!record && typeof record.at === 'number' && Date.now() - record.at < RECONNECT_GRACE_MS
    }

    function apply(ctx) {
      installCSS()
      const call = createTransport()
      let active = null, disposed = false, choosing = false
      const listeners = new Set()
      // One status drives both surfaces: the sidebar row (a short note, with the tooltip
      // carrying the sentence that does not fit) and the mount dialog, which is the same
      // status seen large. Nothing here is a consent gate, and nothing here blocks the
      // picker — see `choose` below for why that matters.
      //
      // Phases: idle | resumable | resuming | stopping | picking | mounting | activating |
      // mounted | failed. The phase rides on the row as `data-dshgw-state` so a real browser
      // run can assert it without reading colours or parsing the note.
      let status = { phase: 'idle', text: '' }
      let dialogOpen = false
      let autoCloseTimer = null
      const records = createRecordStore()
      // The mount this page found waiting for it: what the previous document was serving,
      // still mounted on the gateway, with its directory handle still granted here. It is
      // only ever consumed by a click — see the record comments above.
      let resumable = null
      const notify = () => { for (const listener of listeners) listener() }
      const setStatus = (phase, text) => { status = { phase, text }; notify() }
      const entryTitle = () => `浏览器工作区：${WARNING}${status.text === '' ? '' : `（当前：${status.text}）`}`
      const clearAutoClose = () => { if (autoCloseTimer !== null) { clearTimeout(autoCloseTimer); autoCloseTimer = null } }
      const closeDialog = () => { clearAutoClose(); dialogOpen = false; notify() }
      // A person who just watched the mount finish has nothing left to answer, so that one
      // ending closes the dialog by itself. Every other ending — failure, disconnect,
      // cancelled picker — is left to the flow or to the person.
      const scheduleAutoClose = () => {
        clearAutoClose()
        autoCloseTimer = setTimeout(() => { autoCloseTimer = null; if (!disposed) { dialogOpen = false; notify() } }, AUTO_CLOSE_MS)
      }
      const live = share => !disposed && active === share && !share.stopped
      const delay = (ms, signal) => new Promise((resolve, reject) => {
        const aborted = () => { clearTimeout(timer); reject(failure('ECANCELED', 'operation cancelled')) }
        const timer = setTimeout(() => { signal?.removeEventListener('abort', aborted); resolve() }, ms)
        if (signal?.aborted) aborted()
        else signal?.addEventListener('abort', aborted, { once: true })
      })
      const stop = async () => {
        const share = active
        if (!share) return true
        if (share.closing) return share.closing
        share.stopped = true
        share.controller.abort()
        setStatus('stopping', '断开中…（保持页面打开）')
        share.closing = (async () => {
          try {
            await call('close', { token: share.token }, 55000)
            if (active === share) active = null
            setStatus('idle', '已断开')
            // The mount is really gone now, so the record must go too: a later page must not
            // try to resume a capability this page just destroyed.
            await records.clear()
            return true
          } catch (error) {
            // Retain capability for an explicit cleanup retry; never fake success.
            setStatus('failed', `清理未确认：${error.message}；点击重试断开，请勿关闭页面`)
            ctx.logger?.warn?.('browser-workspace: cleanup unconfirmed; retry close or await gateway lease cleanup')
            return false
          } finally { share.closing = null }
        })()
        return share.closing
      }
      // reconnect keeps this page serving its mount across a failure it can recover from —
      // the transport, not the mount, is what broke (a dropped connection, a worker restart,
      // a gateway restart behind a proxy). The gateway holds the mount for its own grace
      // window, so asking to resume is what turns "已断线，请重新选择目录" into a page that
      // heals itself, and it needs no reload and no second directory choice.
      const reconnect = async (share, error) => {
        if (!live(share)) return false
        setStatus('resuming', `连接中断（${error.message}），正在重连…`)
        const deadline = Date.now() + RECONNECT_GRACE_MS - 5000
        while (live(share) && Date.now() < deadline) {
          await delay(1000).catch(() => {})
          if (!live(share)) return false
          if (share.controller.signal.aborted) return false
          try {
            await call('resume', { token: share.token }, 10000)
            return true
          } catch (resumeError) {
            // "unknown directory capability" is the gateway saying this mount is finished:
            // it expired, or something closed it. Retrying that would loop on a dead
            // capability for the whole window, so the record goes and the operator is told
            // the truth.
            if (resumeError.message === 'unknown directory capability' ||
                resumeError.message === 'directory revoked') {
              await records.clear()
              return false
            }
          }
        }
        return false
      }
      // stash is what makes a reload resumable at all: the capability token and the real
      // directory handle, kept where only this origin can read them.
      const stash = async (share, root) => {
        await records.save({
          version: 1, token: share.token, id: share.id || null, name: share.name,
          workspaceId: share.workspaceId || null, handle: root, at: Date.now(),
        })
      }
      const poll = async share => {
        try {
          while (live(share)) {
            const result = await call('poll', { token: share.token }, 35000, share.controller.signal)
            if (!Array.isArray(result?.requests)) throw failure('EIO', 'invalid poll response')
            for (const request of result.requests) {
              if (!live(share)) return
              let reply
              try { reply = { ok: true, value: await share.execute(request) } }
              catch (error) { reply = { ok: false, error: errorOf(error) } }
              if (!live(share)) return
              await call('respond', { token: share.token, id: request.id, result: reply }, 35000, share.controller.signal)
            }
            if (!result.requests.length) await delay(100, share.controller.signal)
          }
        } catch (error) {
          if (!live(share)) return
          if (error.message === 'directory revoked') {
            // Another page resumed this mount while this one was parked: this page is no
            // longer its browser side, and it must not fight the page that is.
            const replaced = share
            if (active === share) active = null
            replaced.stopped = true
            await records.clear()
            setStatus('failed', '该目录已在另一个页面恢复；请在这个页面重新打开工作区')
            return
          }
          if (await reconnect(share, error)) {
            // The mount is ours again with the same path and the same worker binding: the
            // only thing to restore is this page's own serving loop. What the row says about
            // WHICH mount this is (restored, or mounted fresh) stays as it was — a successful
            // reconnect is not a new mount.
            if (live(share) && status.phase === 'resuming') {
              setStatus('mounted', `已挂载 ${share.name}（读写，已重连）；点击断开`)
            }
            await poll(share)
            return
          }
          if (live(share) && await stop()) setStatus('failed', `已断线：${error.message}；请重新选择目录`)
        }
      }
      // Register the Workspace while the mount point is GUARANTEED to exist.
      //
      // `open` creates the mount point and any teardown (page reload, transport
      // failure, disconnect) removes it again, while the activation restart takes
      // seconds. Registering after that restart raced the teardown and failed with
      // workspace/invalid-path: ENOENT realpath on a directory that had just been
      // removed. create(path) is idempotent, unlike opening a new workspace session.
      const createWorkspace = async (share, path) => {
        const deadline = Date.now() + 10000
        let lastError = failure('ETIMEDOUT', 'workspace registration timed out')
        while (live(share) && Date.now() < deadline) {
          let timer
          try {
            const created = await Promise.race([
              ctx.remote.workspace.create({ path }),
              new Promise((_, reject) => { timer = setTimeout(() => reject(failure('ETIMEDOUT', 'workspace registration timed out')), 5000) }),
            ])
            if (created?.ok) return created.value.workspace
            lastError = created?.error || failure('EIO', 'workspace registration failed')
          } catch (error) { lastError = error }
          finally { clearTimeout(timer) }
          await delay(500, share.controller.signal)
        }
        throw lastError
      }
      // A failed mount releases its capability, and the gateway then removes the
      // mount point: the Workspace registered above would point at a directory that
      // no longer exists. Nothing was connected to it yet, so it is safe to forget.
      const forgetWorkspace = async workspace => {
        if (!workspace?.workspaceId) return
        try { await ctx.remote.workspace.delete(workspace.workspaceId) }
        catch { ctx.logger?.warn?.('browser-workspace: the failed mount left its workspace behind') }
      }
      // ConnectionHandle state/generation/reconnect are public DSH APIs. The
      // gateway's worker restart completes before the browser wire reconnects.
      const awaitReconnect = async (share, priorGeneration) => {
        const connection = ctx.connection
        if (connection.generation.getSnapshot()?.id === priorGeneration || connection.state.getSnapshot() !== 'connected') connection.reconnect()
        const deadline = Date.now() + 30000
        while (live(share) && Date.now() < deadline) {
          if (connection.state.getSnapshot() === 'connected' && connection.generation.getSnapshot()?.id !== priorGeneration) return
          await delay(500, share.controller.signal)
        }
        throw failure('ETIMEDOUT', '等待工作区连接超时')
      }
      // finishActivation is everything a live mount needs after its capability exists:
      // registration while the mount point is guaranteed to exist, the activation restart,
      // the wait for the new worker generation, then the workspace is reported to the GUI.
      const finishActivation = async (share, root, workspaceId) => {
        const workspace = workspaceId
          ? { workspaceId }
          : await createWorkspace(share, share.mountpoint)
        if (!live(share)) return null
        try {
          const priorGeneration = ctx.connection.generation.getSnapshot()?.id
          await call('activate', { token: share.token }, 55000, share.controller.signal)
          if (!live(share)) return null
          setStatus('activating', '挂载完成；等待 worker 重连…')
          await awaitReconnect(share, priorGeneration)
          if (!live(share)) return null
          try {
            const renamed = await ctx.remote.workspace.rename({ workspaceId: workspace.workspaceId, title: `本地: ${root.name}` })
            if (!renamed?.ok) ctx.logger?.warn?.('browser-workspace: workspace rename failed')
          } catch { ctx.logger?.warn?.('browser-workspace: workspace rename failed') }
          if (!live(share)) return null
          await ctx.uiWorkspace.connectWorkspace(workspace.workspaceId)
          share.workspaceId = workspace.workspaceId
          share.root = root
          // The mount is live and registered, which is exactly when a record becomes useful:
          // from here a reload is recoverable by one click instead of a new mount.
          await stash(share, root)
          if (live(share)) {
            setStatus('mounted', `已挂载 ${root.name}（读写）；点击断开`)
            if (dialogOpen) scheduleAutoClose()
          }
          return workspace
        } catch (error) {
          await forgetWorkspace(workspace)
          throw error
        }
      }
      // resumableMount takes back the mount the previous document left behind. It is called
      // by a click, from a stored record whose handle this page was already granted at boot —
      // so it touches no picker and no permission prompt, and the mount it returns to is the
      // SAME one (same mount point, same worker binding), not a replacement.
      const resumableMount = async () => {
        const record = resumable
        const handle = record.handle
        const share = {
          token: record.token, id: record.id, name: record.name, workspaceId: record.workspaceId,
          root: handle, execute: createExecutor(handle, { writable: true }),
          controller: new AbortController(), stopped: false, closing: null, seenAt: Date.now(),
        }
        setStatus('resuming', `恢复中…（${record.name}）`)
        try {
          // Ownership first: until the gateway has handed the mount back, this page is just
          // another client asking, and a poll sent before that would fail — which the poll
          // loop would faithfully try to recover from, doubling the handshake.
          const answer = await call('resume', { token: record.token }, 20000, share.controller.signal)
          if (disposed) return null
          if (answer?.mountpoint) share.mountpoint = answer.mountpoint
          active = share
          resumable = null
          // The mount is already mounted and may already be carrying operations the worker
          // queued while nobody was serving it: the loop answers them from here on.
          void poll(share)
          await stash(share, handle)
          const workspaceId = record.workspaceId || await findWorkspace(share.mountpoint)
          setStatus('mounted', `已挂载 ${record.name}（读写，已恢复）；点击断开`)
          if (dialogOpen) scheduleAutoClose()
          // The workspace is already registered under this path, so there is nothing to
          // activate and no worker to restart: the operator gets the old workspace back with
          // its path unchanged. A workspace that cannot be found is reported, not silently
          // followed by a second registration that would duplicate the entry.
          if (workspaceId) {
            share.workspaceId = workspaceId
            await stash(share, handle)
            try { await ctx.uiWorkspace.connectWorkspace(workspaceId) }
            catch (error) { ctx.logger?.warn?.('browser-workspace: reconnecting the restored workspace failed: ' + (error?.message || error)) }
          } else {
            ctx.logger?.warn?.('browser-workspace: the restored mount has no workspace entry to reopen')
          }
          return share
        } catch (error) {
          // The resume request itself failed: nothing on this page owns the token, so there
          // is no loop to stop — the failure is simply reported to the caller.
          share.stopped = true
          throw error
        }
      }
      // findWorkspace locates the GUI's own entry for a path. It exists because the record
      // may have been written by a document that never learned the id, and registering a
      // second workspace for the same directory would give the operator two identical rows.
      const findWorkspace = async path => {
        try {
          const listed = await ctx.remote.workspace.list()
          const items = listed?.value?.workspaces || listed?.workspaces || []
          const match = items.find(item => item?.path === path)
          return match?.workspaceId || match?.id || null
        } catch { return null }
      }
      // One click, one decision: stop a live mount, take back the one this page found waiting,
      // or open the picker. Only the last of those needs transient user activation (about
      // five seconds in Chromium), which is why it is reached without any await in front of
      // it — everything that can be known beforehand (the stored record) was read at boot,
      // and a permission that needs re-confirming takes the picker path rather than a
      // gesture-less requestPermission. The warning is not a gate: it lives on the row
      // (title) and in the dialog that this click opens.
      const choose = async () => {
        if (disposed || choosing) return
        if (active) { choosing = true; try { await stop() } finally { choosing = false }; return }
        // The resumable branch must not await anything before it can fall through to the
        // picker: showDirectoryPicker needs the click's transient user activation, and one
        // await of a store or permission query spends it ("Must be handling a user gesture").
        // That is why the stored handle's permission is resolved once at boot (see the record
        // lookup effect) instead of here.
        if (resumable && resumable.ready === true) {
          choosing = true
          clearAutoClose()
          dialogOpen = true
          try {
            await resumableMount()
            return
          } catch (error) {
            // Any refusal is the gateway's final word on that capability: forget the record
            // and fall through to a fresh mount, so the click still does something.
            resumable = null
            await records.clear()
            if (error.name !== 'AbortError') {
              ctx.logger?.warn?.('browser-workspace: resume refused: ' + (error.message || error))
            }
          } finally { choosing = false }
        }
        if (!window.isSecureContext || !window.showDirectoryPicker) { setStatus('failed', '需要 HTTPS 和支持目录访问的浏览器'); return }
        choosing = true
        clearAutoClose()
        dialogOpen = true
        setStatus('picking', '等待选择目录…')
        try {
          const root = await window.showDirectoryPicker({ mode: 'readwrite' })
          if (disposed) return
          const opened = await call('open', { name: root.name, writable: true })
          if (!opened?.token) throw failure('EIO', 'invalid open handshake')
          const share = { token: opened.token, id: opened.id, name: root.name, mountpoint: opened.mountpoint, root, execute: createExecutor(root, { writable: true }), controller: new AbortController(), stopped: false, closing: null }
          active = share
          if (disposed) { await stop(); return }
          if (!opened.mountpoint) throw failure('EIO', 'invalid open mountpoint')
          setStatus('mounting', `挂载中…（${root.name}，worker 将重启）`)
          // The poll loop must already be answering before ANYTHING asks the worker about
          // this path. The mount is visible inside the tenant's sandbox (mount propagation
          // carries it into the running worker namespace), so the worker's own
          // workspace.create() realpath stats the mount point: with nobody polling, that
          // stat blocks for the whole FUSE timeout and registration dies as
          // "workspace registration timed out".
          void poll(share)
          await finishActivation(share, root, null)
        } catch (error) {
          // AbortError is harmless only when the picker was cancelled before open;
          // an activation timeout still requires cleanup of the retained token.
          if (active) { if (await stop()) setStatus('failed', `挂载失败：${error.message || String(error)}`) }
          // A cancelled picker changed nothing: no capability, no mount, no failure to
          // report — the row goes back to exactly what it said before the click.
          else if (error.name === 'AbortError') { closeDialog(); setStatus('idle', '') }
          else setStatus('failed', `挂载失败：${error.message || String(error)}`)
        } finally { choosing = false }
      }
      // The sidebar entry: one row, stacked directly above the ssh-workspace one. The click
      // is the whole gesture — no consent step, no second confirmation.
      function Action({ wide } = {}) {
        const [, update] = React.useState(0)
        React.useEffect(() => { const listener = () => update(n => n + 1); listeners.add(listener); return () => listeners.delete(listener) }, [])
        // The collapsed rail is one icon column: the text cannot fit there, so it is dropped
        // and the row's tooltip carries the state instead.
        const rail = wide === false
        return React.createElement('button', {
          type: 'button',
          className: rail ? 'dshgw-bw-action dshgw-bw-action-rail' : 'dshgw-bw-action',
          title: entryTitle(),
          'aria-pressed': active !== null,
          'aria-label': status.text === '' ? '浏览器工作区' : `浏览器工作区：${status.text}`,
          'data-dshgw-state': status.phase,
          onClick: choose,
        },
        React.createElement('span', { 'aria-hidden': 'true' }, '🖥'),
        React.createElement('span', { className: 'dshgw-bw-label' }, '浏览器工作区'),
        status.text === '' ? null : React.createElement('span', { className: 'dshgw-bw-state' }, status.text))
      }
      // The mount dialog: the same status the row carries, large enough to read, in the
      // frame-wide overlay list slot. It closes itself on success (AUTO_CLOSE_MS) and stays
      // open on failure until a person dismisses it; dismissing it never cancels the mount,
      // because the row keeps reporting the real state either way.
      function Dialog() {
        const [, update] = React.useState(0)
        React.useEffect(() => { const listener = () => update(n => n + 1); listeners.add(listener); return () => listeners.delete(listener) }, [])
        if (!dialogOpen) return null
        const failed = status.phase === 'failed'
        const mounted = status.phase === 'mounted'
        return React.createElement('div', {
          className: 'dshgw-bw-backdrop',
          style: { pointerEvents: 'auto' },
          role: 'dialog',
          'aria-modal': 'true',
          'aria-label': '浏览器工作区',
          'data-dshgw-dialog': 'browser-workspace',
          onClick: (event) => { if (event.target === event.currentTarget) closeDialog() },
        }, React.createElement('div', { className: 'dshgw-bw-dialog' }, [
          React.createElement('h2', { key: 'title' }, '浏览器工作区'),
          React.createElement('p', { className: 'hint', key: 'hint' }, WARNING),
          React.createElement('div', {
            key: 'status',
            className: failed ? 'dshgw-bw-error' : mounted ? 'dshgw-bw-notice' : 'dshgw-bw-status',
            role: 'status',
            'aria-live': 'polite',
          }, status.text === '' ? '准备中…' : status.text),
          mounted ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'auto' }, '成功：窗口会自行关闭，不需要手动关闭。') : null,
          status.phase === 'resumable' ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'resume' }, '点击「恢复」把刷新前挂载的同一个目录接回来（不会重新挂载，也不会重启 worker）。') : null,
          failed ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'retry' }, '修复后可以再点一次侧栏那一行重试。') : null,
          React.createElement('div', { className: 'dshgw-bw-footer', key: 'footer' }, [
            status.phase === 'resumable'
              ? React.createElement('button', { key: 'resume', type: 'button', onClick: choose }, '恢复')
              : null,
            React.createElement('button', { key: 'close', type: 'button', onClick: closeDialog }, '关闭'),
          ]),
        ]))
      }
      // What the previous document left behind, read once per page: a mount the gateway is
      // still holding, with a directory handle this browser has already granted. It only
      // moves the row into its "可恢复" state — the resume itself is the operator's click.
      ctx.effect(() => {
        let cancelled = false
        records.load().then(record => {
          if (cancelled || disposed || !recordIsFresh(record)) {
            if (!cancelled && record) void records.clear()
            return
          }
          return reopenHandle(record.handle).then(handle => {
            if (cancelled || disposed) return
            if (!handle) { return records.clear() }
            resumable = Object.assign({}, record, { handle, ready: true })
            setStatus('resumable', `刷新前挂载的是 ${record.name}；点击恢复`)
          })
        }).catch(error => { ctx.logger?.warn?.('browser-workspace: reading the stored mount failed: ' + (error?.message || error)) })
        return () => { cancelled = true }
      }, 'browser-workspace: stored mount lookup')
      // order 90 keeps this row above the ssh-workspace entry (order 100); order 210 keeps
      // this dialog above the ssh-workspace one (200) when both happen to be open.
      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({ name: 'sidebar.footer.action', id: 'browser-workspace', order: 90, label: '浏览器工作区' }, Action)), 'browser-workspace: sidebar entry')
      ctx.effect(() => ctx.slots.inject('shell.overlay', () => ctx.slots.register({ name: 'shell.overlay', id: 'browser-workspace-dialog', order: 210, label: '浏览器工作区' }, Dialog)), 'browser-workspace: mount dialog')
      // Disposal is a page going away, NOT a decision to unmount: a reload lands here too,
      // and the whole point of the stored record is that the next document can take this
      // mount back. So the record is left alone on purpose — only an explicit stop clears it.
      ctx.effect(() => () => { disposed = true; clearAutoClose(); void stop(); listeners.clear() }, 'browser-workspace: cleanup')
    }
    // 'remote' AND 'remote.workspace' are both required: Cordis resolves the dotted name
    // as its own service, but every `ctx.remote.workspace.*` call below first reads the
    // parent `ctx.remote`, and without 'remote' in this list that read throws
    // `cannot get property "remote" without inject` inside a real DSH GUI (the same pair
    // @deepseek-ai/dsh-api-workspace-controller declares). A mocked ctx that hands the
    // plugin a ready-made `remote` object cannot catch this.
    return { inject: ['slots', 'connection', 'remote', 'remote.workspace', 'uiWorkspace'], apply, createExecutor, checkRelativePath, createTransport, errorOf, createRecordStore, reopenHandle, recordIsFresh, AUTO_CLOSE_MS, RECONNECT_GRACE_MS }
  },
})
