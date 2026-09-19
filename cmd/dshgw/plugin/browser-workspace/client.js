// Hand-built ModuleLoader bundle, served byte-for-byte like ssh-workspace.
//
// One account can have SEVERAL local directories mounted at once. Each one is a "folder"
// here: a saved entry (identity + directory handle + the workspace it maps to) that is either
// connected or not. The sidebar row stays the entry point (one click does the obvious thing),
// and the icon at its right opens the folder list, where a person adds, connects, disconnects
// and deletes folders.
//
// The virtual mapping is what makes this usable rather than merely possible: the mount point
// is <workspace>/browser/<folder key>, where the key is generated once per saved folder and
// kept in IndexedDB. The key — not a per-mount random id — is what DSH's own workspace entry
// is keyed by (the registry reuses a workspace by its canonical path), so reconnecting the
// same local directory returns the same path, the same workspace id, the same title and the
// same sessions. See the README for the state machine and the gateway contract.
window.__ModuleLoader__.load({
  id: 'dshgw-browser-workspace',
  factory: require => {
    const React = require('react')
    const MAX_BYTES = 1024 * 1024
    // How long the success dialog stays up before it closes itself. Exported for the unit
    // test, which fires that one timer by hand instead of sleeping.
    const AUTO_CLOSE_MS = 1500
    // How many local directories one browser profile may keep SAVED. Only
    // MAX_MOUNTS_PER_ACCOUNT (the gateway's own bound, currently 4) may be mounted at once;
    // saved-but-disconnected entries beyond that are what makes swapping directories cheap.
    const MAX_FOLDERS = 8
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
    // The sidebar row is one container with two controls: the row body (the obvious single
    // action) and the folder icon at its right (the list window). The shell renders this slot
    // as one flex ROW, so the stacking rule at the bottom of this stylesheet is what keeps the
    // browser row and the ssh-workspace row on separate lines.
    const CSS = `
.dshgw-bw-row { display: flex; align-items: center; gap: 2px; width: 100%; min-width: 0; }
.dshgw-bw-action { flex: 1 1 auto; min-width: 0; display: flex; align-items: center; gap: 6px; background: none; border: 0; color: inherit; font: inherit; cursor: pointer; padding: 6px 8px; border-radius: 6px; text-align: left; }
.dshgw-bw-action:hover { background: rgba(127,127,127,.14); }
.dshgw-bw-action[aria-pressed="true"] .dshgw-bw-label { font-weight: 600; }
.dshgw-bw-label { flex: none; white-space: nowrap; }
.dshgw-bw-state { flex: 1 1 auto; min-width: 0; margin-left: 4px; opacity: .65; font-size: 12px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.dshgw-bw-manage { flex: none; display: inline-flex; align-items: center; justify-content: center; width: 24px; height: 24px; border: 0; border-radius: 6px; background: none; color: inherit; font: inherit; font-size: 14px; line-height: 1; cursor: pointer; opacity: .7; }
.dshgw-bw-manage:hover { background: rgba(127,127,127,.18); opacity: 1; }
.dshgw-bw-row-rail .dshgw-bw-label, .dshgw-bw-row-rail .dshgw-bw-state, .dshgw-bw-row-rail .dshgw-bw-manage { display: none; }
.dshgw-bw-backdrop { position: fixed; inset: 0; display: flex; align-items: center; justify-content: center; background: rgba(0,0,0,.45); z-index: 40; }
.dshgw-bw-dialog { width: min(600px, 92vw); max-height: 86vh; overflow: auto; background: var(--dsh-bg, #1b1c1f); color: var(--dsh-fg, #e6e6e6); border: 1px solid rgba(127,127,127,.35); border-radius: 10px; padding: 16px 18px; font-size: 13px; line-height: 1.5; }
.dshgw-bw-dialog h2 { margin: 0 0 4px; font-size: 15px; }
.dshgw-bw-dialog p.hint { margin: 0 0 12px; opacity: .7; }
.dshgw-bw-dialog button { background: rgba(127,127,127,.18); color: inherit; border: 1px solid rgba(127,127,127,.35); border-radius: 6px; padding: 6px 10px; font: inherit; cursor: pointer; }
.dshgw-bw-dialog button[disabled] { opacity: .5; cursor: default; }
.dshgw-bw-status { margin: 8px 0; }
.dshgw-bw-error { border: 1px solid #b3453c; background: rgba(179,69,60,.18); border-radius: 6px; padding: 8px 10px; margin: 8px 0; white-space: pre-wrap; }
.dshgw-bw-notice { border: 1px solid #3a7d44; background: rgba(58,125,68,.18); border-radius: 6px; padding: 8px 10px; margin: 8px 0; }
.dshgw-bw-muted { opacity: .65; }
.dshgw-bw-folders { border: 1px solid rgba(127,127,127,.3); border-radius: 6px; margin: 8px 0; }
.dshgw-bw-folder { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 6px 8px; border-bottom: 1px solid rgba(127,127,127,.14); }
.dshgw-bw-folder:last-child { border-bottom: 0; }
.dshgw-bw-folder-name { min-width: 0; display: flex; flex-direction: column; }
.dshgw-bw-folder-name > span:first-child { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.dshgw-bw-folder-state { opacity: .65; font-size: 12px; }
.dshgw-bw-folder-actions { flex: none; display: flex; gap: 6px; }
.dshgw-bw-folder-actions button { padding: 3px 8px; }
.dshgw-bw-folder-error { color: #e08078; font-size: 12px; }
.dshgw-bw-empty { padding: 10px; opacity: .7; }
.dshgw-bw-footer { display: flex; justify-content: flex-end; gap: 8px; margin-top: 12px; }
/* The shell renders this whole slot as one flex ROW, which leaves two entries sharing a foot
   that only fits one — so each of them is squeezed to half width. A plugin owns no wrapper
   element around its own row: the list slot wraps every registration in a classless div, so
   the shell's container is TWO levels up, and :has() is the only selector that can reach it
   from here. Both shapes are covered (the direct one too, in case the wrapper ever goes
   away), and both rows ship this rule, so it holds whichever of the two plugins a deployment
   enables.

   The browser row is a CONTAINER, and that matters here: the rule must match the container
   from OUTSIDE, never the container itself. An earlier version also matched the inner
   '.dshgw-bw-action' button, which made the row's own container a column — the folder icon
   ended up under the label instead of beside it (caught by the real-browser geometry check in
   browser_workspace_mount_e2e.py). Only the container class is matched now. */
div:has(> .dshgw-bw-row), div:has(> div > .dshgw-bw-row),
div:has(> .dshgw-ssh-action), div:has(> div > .dshgw-ssh-action) { flex-direction: column; }
`
    // What the entry promises before anyone clicks it. The consent step that used to
    // stand in front of the picker is gone (it cost a second click and could burn the
    // click's transient user activation), so this warning travels with the row instead.
    const WARNING = '把本机目录挂载成这个账号的真实工作区：AI 可以读取、修改、重命名和删除所选目录中的文件，读取的文件内容可能发送给配置的 AI 模型服务商；每个目录是独立挂载，连接和断开都会重启该账号的 worker，可能中断正在运行的任务或连接。请选择专用目录并先备份，不要选择密钥或敏感目录；挂载期间必须保持此页面打开。'
    // Gateway failures a person can act on. Everything else is shown as the gateway said it,
    // because inventing a friendlier sentence for an unknown failure would hide its cause.
    const MESSAGES = [
      ['per-account browser directory limit reached', '已达每账号 4 个目录上限：请先断开一个目录'],
      ['global mount limit reached or service stopping', '网关挂载已满或正在停服，请稍后重试'],
      ['directory key already mounted', '该目录在此账号上已有挂载（可能正由另一个页面服务）'],
      ['directory already served by another page', '该目录正在另一个页面服务，请到那个页面使用'],
      ['directory revoked', '该目录已被另一个页面接管，本页已停止服务'],
      ['unknown directory capability', '该挂载已失效，将重新挂载'],
      ['directory disconnected', '挂载已断开'],
      ['browser root unavailable', '所选目录已不可用：请重新选择目录'],
    ]
    const explain = error => {
      const message = error?.message || String(error)
      for (const [needle, text] of MESSAGES) if (message.includes(needle)) return text
      return message
    }
    const isAbort = error => error?.name === 'AbortError'
    // A gateway that predates this client rejects the extra fields outright (its JSON decoder
    // runs with DisallowUnknownFields): `key` on `open`, `purge` on `close`. Recognising that
    // one answer is what lets a page loaded just before a gateway restart keep working — it
    // degrades to the old behaviour (a per-mount random path, no explicit release) instead of
    // failing to mount at all.
    const isUnknownField = error => typeof error?.message === 'string' && error.message.includes('unknown field')

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
    // ── the saved folder list ─────────────────────────────────────────────────────
    //
    // A reload is not an unmount, and a disconnect is not a removal. What survives in the
    // browser is the list of SAVED folders: for each one its stable key, its display name and
    // the FileSystemDirectoryHandle it was granted. The capability token is stored too, but it
    // is the perishable part: the gateway keeps a mount for its grace window and refuses the
    // token afterwards, while the saved folder stays until the operator deletes it.
    //
    // The stable key is the whole virtual mapping: the gateway mounts the folder at
    // <workspace>/browser/<key>, DSH reuses a workspace by that path, and so reconnecting the
    // same local directory returns the same workspace id with the same sessions. Nothing here
    // is resumed automatically: a mount belongs to a click.
    const RECORD_DB = 'dshgw-browser-workspace'
    const RECORD_STORE = 'mounts'
    const RECORD_VERSION = 2
    const LEGACY_KEY_PREFIX = 'mount:' // v1: exactly one mount per origin
    const FOLDERS_KEY_PREFIX = 'folders:' // v2: the saved folder list
    // How long the gateway keeps a mount waiting for a page that went away. The page cannot
    // know this number (it is the gateway's), so it is mirrored here only to decide when a
    // stored token has gone stale enough to stop advertising a resume that cannot work.
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

    // Saved folders are keyed by the origin the page is on: one tenant origin owns its own
    // list, and a list left by a different tenant must never be mounted here (the gateway
    // would refuse it — a capability is bound to tenant and session — but not offering it at
    // all is the honest behaviour).
    //
    // The v2 list lives under a NEW key on purpose. An older bundle in another tab reads
    // `mount:<origin>` and clears a record it cannot parse; keeping the two apart means a
    // mixed-version window cannot wipe the operator's saved folders.
    function createRecordStore() {
      const foldersKey = () => FOLDERS_KEY_PREFIX + window.location.origin
      const legacyKey = () => LEGACY_KEY_PREFIX + window.location.origin
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
      const read = key => withStore('readonly', store => store.get(key)).catch(() => null)
      // One value may be a v2 list or the single v1 record; both normalise to the same shape.
      // A v1 record named its mount point with a random id, so that id becomes the folder's
      // stable key — the path, and the workspace entry pointing at it, stay where they are.
      const normalise = value => {
        if (!value || typeof value !== 'object') return []
        const rows = Array.isArray(value.folders) ? value.folders : (value.token || value.handle ? [value] : [])
        const adopted = []
        for (const row of rows) {
          if (!row || typeof row !== 'object' || !row.handle) continue
          const key = typeof row.key === 'string' ? row.key : (typeof row.id === 'string' ? row.id : null)
          if (key === null) continue
          adopted.push(Object.assign({}, row, { key }))
        }
        return adopted
      }
      // Every mutation goes through one chain: a connect that finishes while a second folder
      // is being added must not overwrite the list the other one just wrote.
      let queue = Promise.resolve()
      const serial = work => { const result = queue.then(work); queue = result.catch(() => {}); return result }
      return {
        load: () => serial(async () => {
          const current = await read(foldersKey())
          if (current) return normalise(current)
          const legacy = await read(legacyKey())
          const adopted = normalise(legacy)
          // The adoption is written back by the caller's next save; dropping the v1 key here
          // stops a second document from adopting the same single mount.
          if (legacy) await withStore('readwrite', store => store.delete(legacyKey())).catch(() => undefined)
          return adopted
        }),
        save: folders => serial(() => withStore('readwrite', store => store.put({ version: RECORD_VERSION, folders }, foldersKey())).catch(() => undefined)),
        clear: () => serial(() => withStore('readwrite', store => store.delete(foldersKey())).catch(() => undefined)),
      }
    }

    // What the executor needs to reach the picked directory again in a NEW document. The
    // permission is asked for explicitly: queryPermission is what a browser answers without a
    // gesture, and a 'prompt' answer means this page cannot reattach on its own — the folder
    // then offers "重新选择目录", which does raise the picker inside a real click.
    async function reopenHandle(handle) {
      if (!handle || typeof handle.queryPermission !== 'function') return null
      if (await handle.queryPermission({ mode: 'readwrite' }) === 'granted') return handle
      return null
    }

    // A stored token is worth offering only while the gateway could still be holding that
    // mount. Nothing here decides anything on its own: the click asks the gateway, and its
    // answer ("unknown directory capability") is what rules a token out.
    function recordIsFresh(record) {
      return !!record && typeof record.at === 'number' && Date.now() - record.at < RECONNECT_GRACE_MS
    }

    // The stable identity of one saved folder: 32 hex characters, generated once and kept with
    // the folder. It names the mount point, so it must stay the same for the lifetime of the
    // folder — that is what keeps the workspace (and its sessions) mapped to this directory.
    function newKey() {
      const uuid = globalThis.crypto?.randomUUID?.()
      if (typeof uuid === 'string') return uuid.replace(/-/g, '').slice(0, 32).toLowerCase()
      const bytes = new Uint8Array(16)
      if (globalThis.crypto?.getRandomValues) globalThis.crypto.getRandomValues(bytes)
      else for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256)
      return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('')
    }

    function apply(ctx) {
      installCSS()
      const call = createTransport()
      let disposed = false, dialogOpen = false, dialogAutoCloses = false, autoCloseTimer = null, picking = false, armedDelete = null, armedTimer = null
      const listeners = new Set()
      // Saved folders, in display order. Each one is the durable half of the feature.
      const folders = []
      // Live mounts by folder key: a folder is connected exactly when it has a share here.
      const shares = new Map()
      // One status drives both surfaces: the sidebar row (a short note, with the tooltip
      // carrying the sentence that does not fit) and the folder window, which is the same
      // status seen large. Nothing here is a consent gate, and nothing here blocks the
      // picker — see `addFolder`/`chooseFolder` for why that matters.
      //
      // Phases: idle | resumable | disconnected | failed | picking | mounting | activating |
      // resuming | disconnecting | mounted. The phase rides on the row as `data-dshgw-state`
      // so a real browser run can assert it without reading colours or parsing the note.
      let status = { phase: 'idle', text: '' }
      // The gateway restarts the account's worker for every connect and disconnect, so those
      // transitions are serialised here: two in flight at once would interleave two worker
      // restarts and two connection-generation waits.
      let chain = Promise.resolve()
      const notify = () => { for (const listener of listeners) listener() }
      const setStatus = (phase, text) => { status = { phase, text }; notify() }
      const entryTitle = () => `浏览器工作区：${WARNING}${status.text === '' ? '' : `（当前：${status.text}）`}`
      const clearAutoClose = () => { if (autoCloseTimer !== null) { clearTimeout(autoCloseTimer); autoCloseTimer = null } }
      const closeDialog = () => { clearAutoClose(); dialogOpen = false; dialogAutoCloses = false; notify() }
      const openDialog = ({ autoClose = false } = {}) => {
        clearAutoClose()
        dialogOpen = true
        dialogAutoCloses = autoClose
        notify()
      }
      // A person who just watched the mount finish has nothing left to answer, so that one
      // ending closes the window by itself — but only when the row's own click opened it. A
      // window the operator opened to manage folders stays where it is.
      const scheduleAutoClose = () => {
        if (!dialogAutoCloses) return
        clearAutoClose()
        autoCloseTimer = setTimeout(() => {
          autoCloseTimer = null
          if (!disposed) { dialogOpen = false; dialogAutoCloses = false; notify() }
        }, AUTO_CLOSE_MS)
      }
      const delay = (ms, signal) => new Promise((resolve, reject) => {
        const aborted = () => { clearTimeout(timer); reject(failure('ECANCELED', 'operation cancelled')) }
        const timer = setTimeout(() => { signal?.removeEventListener('abort', aborted); resolve() }, ms)
        if (signal?.aborted) aborted()
        else signal?.addEventListener('abort', aborted, { once: true })
      })
      const live = share => !disposed && shares.get(share.key) === share && !share.stopped
      const folderOf = share => folders.find(folder => folder.key === share.key) || null
      const folderBusy = () => folders.some(folder => folder.busy !== null) || picking
      const serial = work => { const result = chain.then(work); chain = result.catch(() => {}); return result }
      const connected = () => folders.filter(folder => folder.state === 'connected')
      // The resting status: what the row says when no operation is narrating itself.
      const resting = () => {
        const liveCount = connected().length
        const failing = folders.filter(folder => folder.state === 'error' || folder.state === 'elsewhere').length
        const resumable = folders.some(folder => folder.state === 'resumable')
        if (folders.length === 0) return { phase: 'idle', text: '' }
        if (folders.length === 1) {
          const folder = folders[0]
          if (folder.state === 'connected') return { phase: 'mounted', text: folder.note }
          if (folder.state === 'resumable') return { phase: 'resumable', text: folder.note }
          if (folder.state === 'error' || folder.state === 'elsewhere') return { phase: 'failed', text: folder.note }
          return { phase: 'disconnected', text: folder.note }
        }
        const suffix = failing === 0 ? '' : `，${failing} 个失败`
        if (liveCount > 0) return { phase: failing === 0 ? 'mounted' : 'failed', text: `已连接 ${liveCount}/${folders.length} 个目录${suffix}；点击管理` }
        if (failing > 0) return { phase: 'failed', text: `${folders.length} 个目录中有 ${failing} 个失败；点击管理` }
        if (resumable) return { phase: 'resumable', text: `${folders.length} 个目录可恢复；点击管理` }
        return { phase: 'disconnected', text: `${folders.length} 个目录未连接；点击管理` }
      }
      const settle = () => { status = resting(); notify() }
      // What each saved folder says about itself, in the window and (when it is the only one)
      // on the row. The single-folder sentences are the ones this feature always had.
      const describe = (folder, suffix = '') => {
        if (folder.state === 'connected') return `已挂载 ${folder.name}（读写${suffix}）；点击断开`
        if (folder.state === 'resumable') return `刷新前挂载的是 ${folder.name}；点击恢复`
        if (folder.state === 'disconnected') return folder.ready === false
          ? `已断开 ${folder.name}；点击重新选择目录`
          : `已断开 ${folder.name}；点击重连`
        return folder.note
      }
      const records = createRecordStore()
      // stash writes the WHOLE list: a folder's identity, its workspace mapping and whatever
      // capability is alive for it. It is the record the next document boots from.
      const stash = () => records.save(folders.map(folder => ({
        version: RECORD_VERSION, key: folder.key, name: folder.name, workspaceId: folder.workspaceId,
        token: folder.token, mountpoint: folder.mountpoint, at: folder.at, handle: folder.handle,
      })))

      // ── mount lifecycle ────────────────────────────────────────────────────────────
      // disposeShare stops serving one mount locally (no gateway call): the poll loop ends,
      // and everything waiting on it fails at once.
      const dropShare = key => {
        const share = shares.get(key)
        if (!share) return
        share.stopped = true
        share.controller.abort()
        shares.delete(key)
      }
      // closeShare asks the gateway to release one mount. A successful close is the only
      // proof it is gone; a failed one keeps the capability so the operator can retry.
      const closeShare = async (folder, { purge = false } = {}) => {
        const share = shares.get(folder.key)
        const token = share?.token || folder.token
        if (!token) { dropShare(folder.key); return true }
        setStatus('disconnecting', '断开中…（保持页面打开）')
        folder.state = 'disconnecting'
        notify()
        dropShare(folder.key)
        try {
          try {
            await call('close', purge ? { token, purge: true } : { token }, 55000)
          } catch (error) {
            if (!purge || !isUnknownField(error)) throw error
            // The gateway does not know `purge`: it removes a mount point on every close
            // anyway, so an older gateway releases the path by itself.
            await call('close', { token }, 55000)
          }
          folder.token = null
          folder.mountpoint = null
          folder.at = 0
          folder.error = ''
          folder.retry = null
          folder.state = 'disconnected'
          folder.note = describe(folder)
          await stash()
          settle()
          return true
        } catch (error) {
          // Retain capability for an explicit cleanup retry; never fake success.
          folder.token = token
          folder.state = 'error'
          folder.retry = 'close'
          folder.error = explain(error)
          folder.note = `清理未确认：${explain(error)}；点击重试断开`
          ctx.logger?.warn?.('browser-workspace: cleanup unconfirmed; retry close or await gateway lease cleanup')
          settle()
          return false
        }
      }
      // reconnect keeps this page serving its mount across a failure it can recover from —
      // the transport, not the mount, is what broke (a dropped connection, a worker restart,
      // a gateway restart behind a proxy). The gateway holds the mount for its own grace
      // window, so asking to resume is what turns a broken page into one that heals itself,
      // with no reload and no second directory choice.
      const reconnect = async (share, error) => {
        const folder = folderOf(share)
        if (!live(share) || !folder) return false
        const wasSettled = folder.state === 'connected'
        folder.note = `连接中断（${error.message}），正在重连…`
        setStatus('resuming', folder.note)
        const deadline = Date.now() + RECONNECT_GRACE_MS - 5000
        while (live(share) && Date.now() < deadline) {
          await delay(1000).catch(() => {})
          if (!live(share)) return false
          if (share.controller.signal.aborted) return false
          try {
            await call('resume', { token: share.token }, 10000)
            if (wasSettled) { folder.note = describe(folder, '，已重连'); settle() }
            return true
          } catch (resumeError) {
            // "unknown directory capability" is the gateway saying this mount is finished:
            // it expired, or something closed it. Retrying that would loop on a dead token
            // for the whole window, so the token goes and the operator is told the truth.
            if (resumeError.message === 'unknown directory capability' || resumeError.message === 'directory revoked') {
              folder.token = null
              folder.at = 0
              await stash()
              return false
            }
          }
        }
        return false
      }
      const poll = async share => {
        const folder = folderOf(share)
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
          if (!live(share) || !folder) return
          if (error.message === 'directory revoked') {
            // Another page resumed this mount while this one was parked: this page is no
            // longer its browser side, and it must not fight the page that is.
            dropShare(share.key)
            folder.state = 'elsewhere'
            folder.error = '该目录已在另一个页面恢复'
            folder.note = `该目录已由另一个页面服务；请在那个页面使用 ${folder.name}`
            settle()
            return
          }
          if (await reconnect(share, error)) {
            // The mount is ours again with the same path and the same worker binding: the
            // only thing to restore is this page's own serving loop.
            await poll(share)
            return
          }
          if (!live(share)) return
          // Out of recovery: the mount is finished, so it is released and the folder goes
          // back to "disconnected" where the operator can connect it again.
          if (await closeShare(folder)) {
            folder.error = explain(error)
            folder.note = `已断线：${explain(error)}；点击重连`
            settle()
          }
        }
      }
      // Register the Workspace while the mount point is GUARANTEED to exist.
      //
      // `open` creates (or reuses) the mount point and any teardown removes it again, while
      // the activation restart takes seconds. Registering after that restart raced the
      // teardown and failed with workspace/invalid-path: ENOENT realpath on a directory that
      // had just been removed. create(path) is idempotent, unlike opening a new session — and
      // with a stable directory key it returns the SAME workspace for this local directory,
      // which is what keeps its title and its sessions across reconnects.
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
            if (created?.ok) return created.value
            lastError = created?.error || failure('EIO', 'workspace registration failed')
          } catch (error) { lastError = error }
          finally { clearTimeout(timer) }
          await delay(500, share.controller.signal)
        }
        throw lastError
      }
      // A failed mount releases its capability: nothing was connected to the workspace the
      // mount registered, so forgetting it is safe. A workspace this mount did NOT create is
      // left alone — deleting it would throw away the sessions grouped under that local
      // directory's mapping, which is the one thing the stable key exists to protect.
      const forgetWorkspace = async (registration) => {
        if (!registration?.created || !registration.workspace?.workspaceId) return
        // `ctx.remote.workspace` is the generated request-object remote: every method takes ONE
        // object (the same `create({path})` / `rename({workspaceId,title})` shape this plugin
        // already uses). A bare id is rejected by its schema, and the deletion then silently
        // does nothing — which is exactly what a real run caught, leaving dead workspace rows.
        try { await ctx.remote.workspace.delete({ workspaceId: registration.workspace.workspaceId }) }
        catch { ctx.logger?.warn?.('browser-workspace: the failed mount left its workspace behind') }
      }
      // ConnectionHandle state/generation/reconnect are public DSH APIs. The gateway's worker
      // restart completes before the browser wire reconnects, so anything that has to reach
      // the worker waits for a NEW generation instead of racing the restart.
      const awaitConnected = async (priorGeneration, { signal, timeoutMs = 30000 } = {}) => {
        const connection = ctx.connection
        if (connection.generation.getSnapshot()?.id === priorGeneration || connection.state.getSnapshot() !== 'connected') connection.reconnect()
        const deadline = Date.now() + timeoutMs
        while (!disposed && Date.now() < deadline) {
          if (connection.state.getSnapshot() === 'connected' && connection.generation.getSnapshot()?.id !== priorGeneration) return true
          await delay(500, signal).catch(() => {})
        }
        return false
      }
      // The activation restart must be followed by a NEW connected generation before anything
      // talks to the worker again.
      const awaitReconnect = async (share, priorGeneration) => {
        if (!await awaitConnected(priorGeneration, { signal: share.controller.signal })) {
          throw failure('ETIMEDOUT', '等待工作区连接超时')
        }
      }
      // findWorkspace locates the GUI's own entry for a path — the fallback for a resume whose
      // record lost the id. It never creates anything: registering a second workspace for the
      // same directory would give the operator two identical rows.
      const findWorkspace = async path => {
        try {
          const listed = await ctx.remote.workspace.list()
          const items = listed?.value?.workspaces || listed?.workspaces || []
          const match = items.find(item => item?.path === path)
          return match?.workspaceId || match?.id || null
        } catch { return null }
      }
      // mountFolder performs a FRESH mount of a saved folder: open → serve → register →
      // activate (the worker restart) → reconnect → rename → open the workspace.
      const mountFolder = async folder => {
        const share = {
          key: folder.key, token: null, id: null, mountpoint: null, root: folder.handle,
          execute: createExecutor(folder.handle, { writable: true }), controller: new AbortController(),
          stopped: false, closing: null, workspaceId: folder.workspaceId, resumed: false,
        }
        let registration = null
        folder.state = 'connecting'
        setStatus('mounting', `挂载中…（${folder.name}，worker 将重启）`)
        try {
          let opened
          try {
            opened = await call('open', { name: folder.name, writable: true, key: folder.key })
          } catch (error) {
            if (!isUnknownField(error)) throw error
            // An older gateway cannot take the stable key: mount anyway, with the per-mount
            // random path it knows, and say so rather than leaving the operator with a failure.
            ctx.logger?.warn?.('browser-workspace: this gateway does not know stable directory keys; mounting without one')
            opened = await call('open', { name: folder.name, writable: true })
          }
          if (!opened?.token || !opened?.mountpoint) throw failure('EIO', 'invalid open handshake')
          share.token = opened.token
          share.id = opened.id
          share.mountpoint = opened.mountpoint
          shares.set(folder.key, share)
          folder.token = opened.token
          folder.mountpoint = opened.mountpoint
          folder.at = Date.now()
          // The token is stored before anything else happens: a reload in the middle of a
          // mount must still find the capability the gateway is holding.
          await stash()
          if (disposed) { await closeShare(folder); return null }
          // The poll loop must already be answering before ANYTHING asks the worker about
          // this path. The mount is visible inside the tenant's sandbox, so the worker's own
          // workspace.create() realpath stats the mount point: with nobody polling, that stat
          // blocks for the whole FUSE timeout and registration dies as
          // "workspace registration timed out".
          void poll(share)
          registration = await createWorkspace(share, share.mountpoint)
          if (!live(share)) return null
          const workspaceId = registration.workspace.workspaceId
          const priorGeneration = ctx.connection.generation.getSnapshot()?.id
          await call('activate', { token: share.token }, 55000, share.controller.signal)
          if (!live(share)) return null
          setStatus('activating', '挂载完成；等待 worker 重连…')
          await awaitReconnect(share, priorGeneration)
          if (!live(share)) return null
          try {
            const renamed = await ctx.remote.workspace.rename({ workspaceId, title: `本地: ${folder.name}` })
            if (!renamed?.ok) ctx.logger?.warn?.('browser-workspace: workspace rename failed')
          } catch { ctx.logger?.warn?.('browser-workspace: workspace rename failed') }
          if (!live(share)) return null
          folder.workspaceId = workspaceId
          share.workspaceId = workspaceId
          await ctx.uiWorkspace.connectWorkspace(workspaceId)
          if (!live(share)) return { workspaceId }
          folder.state = 'connected'
          folder.error = ''
          folder.retry = null
          folder.note = describe(folder)
          await stash()
          settle()
          scheduleAutoClose()
          return { workspaceId }
        } catch (error) {
          await forgetWorkspace(registration)
          throw error
        }
      }
      // resumeFolder takes back the mount the previous document (or the gateway's grace
      // window) still holds for this folder. The mount may already be carrying operations the
      // worker queued while nobody was serving it, so the loop starts before anything else.
      const resumeFolder = async folder => {
        const share = {
          key: folder.key, token: folder.token, id: folder.id, mountpoint: folder.mountpoint, root: folder.handle,
          execute: createExecutor(folder.handle, { writable: true }), controller: new AbortController(),
          stopped: false, closing: null, workspaceId: folder.workspaceId, resumed: true,
        }
        setStatus('resuming', `恢复中…（${folder.name}）`)
        folder.state = 'connecting'
        notify()
        // Ownership first: until the gateway has handed the mount back, this page is just
        // another client asking, and a poll sent before that would fail.
        const answer = await call('resume', { token: share.token }, 20000, share.controller.signal)
        if (disposed) { share.stopped = true; return null }
        if (answer?.mountpoint) share.mountpoint = answer.mountpoint
        shares.set(folder.key, share)
        folder.mountpoint = share.mountpoint
        folder.at = Date.now()
        await stash()
        void poll(share)
        // The workspace is already registered under this path, so there is nothing to
        // activate and no worker to restart: the operator gets the old workspace back, with
        // the same path, id, title and sessions.
        const workspaceId = folder.workspaceId || await findWorkspace(share.mountpoint)
        folder.state = 'connected'
        folder.error = ''
        folder.retry = null
        folder.workspaceId = workspaceId
        share.workspaceId = workspaceId
        folder.note = describe(folder, '，已恢复')
        await stash()
        settle()
        scheduleAutoClose()
        if (workspaceId) {
          try { await ctx.uiWorkspace.connectWorkspace(workspaceId) }
          catch (error) { ctx.logger?.warn?.('browser-workspace: reconnecting the restored workspace failed: ' + (error?.message || error)) }
        } else {
          ctx.logger?.warn?.('browser-workspace: the restored mount has no workspace entry to reopen')
        }
        return share
      }
      // connect brings one saved folder online, preferring the mount the gateway may still be
      // holding for it.
      const connect = folder => serial(async () => {
        if (disposed || folder.busy) return false
        folder.busy = Promise.resolve()
        folder.error = ''
        folder.retry = null
        try {
          if (folder.token && recordIsFresh({ at: folder.at })) {
            try {
              await resumeFolder(folder)
              return true
            } catch (error) {
              const message = error?.message || String(error)
              if (message.includes('directory already served by another page') || message.includes('directory revoked')) {
                folder.state = 'elsewhere'
                folder.error = '该目录正在另一个页面服务'
                folder.note = `该目录正在另一个页面服务；请在那个页面使用 ${folder.name}`
                settle()
                return false
              }
              if (!message.includes('unknown directory capability') && !message.includes('directory disconnected')) {
                folder.state = 'error'
                folder.error = explain(error)
                folder.note = `连接失败：${explain(error)}`
                settle()
                return false
              }
              folder.token = null
              folder.at = 0
              await stash()
            }
          }
          await mountFolder(folder)
          return true
        } catch (error) {
          if (isAbort(error) && disposed) return false
          // The mount may have been opened and then failed: it is released, and the token is
          // kept only when the gateway did not confirm that release.
          if (shares.has(folder.key) || folder.token) {
            const released = await closeShare(folder)
            folder.error = explain(error)
            if (released) {
              folder.state = 'error'
              folder.retry = 'connect'
              folder.note = `挂载失败：${explain(error)}`
            }
          } else {
            folder.state = 'error'
            folder.retry = 'connect'
            folder.error = explain(error)
            folder.note = `挂载失败：${explain(error)}`
          }
          settle()
          return false
        } finally {
          folder.busy = null
          settle()
        }
      })
      // disconnect takes one folder offline and LEAVES IT SAVED. The mount point is not
      // removed by the gateway either: the empty directory at the same path is what keeps the
      // workspace entry (and the sessions it groups) mapped to this local directory, so
      // connecting again restores the same workspace instead of building a new one.
      const disconnect = folder => serial(async () => {
        if (disposed || folder.busy) return false
        folder.busy = Promise.resolve()
        try {
          const released = await closeShare(folder)
          if (released) {
            folder.note = describe(folder)
            settle()
          }
          return released
        } finally {
          folder.busy = null
          settle()
        }
      })
      // remove is the operator forgetting a local directory for good: the mount goes, the
      // gateway releases the virtual path, and the workspace entry goes with it.
      const remove = folder => serial(async () => {
        if (disposed || folder.busy) return false
        folder.busy = Promise.resolve()
        try {
          // The workspace registration goes FIRST, while the account's worker is still the one
          // this page is talking to. Releasing the mount restarts that worker, and a deletion
          // sent into a restarting (or reconnecting) worker is refused or lost — a real run
          // left the dead workspace row behind that way. The registration is metadata: removing
          // it while the mount is still bound changes nothing about the worker's own file
          // access, and the close below still releases the mount and its path.
          let unremoved = null
          if (folder.workspaceId) {
            const workspaceId = folder.workspaceId
            let removed = false
            for (let attempt = 0; attempt < 3 && !removed; attempt++) {
              if (attempt > 0) await delay(1000).catch(() => {})
              try {
                const deleted = await ctx.remote.workspace.delete({ workspaceId })
                if (deleted && deleted.ok === false) unremoved = explain(deleted.error || failure('EIO', 'workspace deletion refused'))
                // A later attempt that lands clears the earlier failure: this folder IS gone
                // from DSH's registry, and reporting it as surviving would be a lie.
                else { removed = true; unremoved = null }
              } catch (error) { unremoved = explain(error) }
            }
            if (removed) folder.workspaceId = null
          }
          if (shares.has(folder.key) || folder.token !== null) {
            const released = await closeShare(folder, { purge: true })
            if (!released) return false
          }
          if (unremoved !== null) {
            // Never report a folder as removed while its workspace row is still there: the
            // operator keeps the entry and can try again (the mount is already released).
            folder.state = 'error'
            folder.retry = null
            folder.error = `工作区条目未移除：${unremoved}`
            folder.note = `删除未完成：${unremoved}；请再试一次删除`
            await stash()
            settle()
            ctx.logger?.warn?.('browser-workspace: the workspace entry survived the folder deletion: ' + unremoved)
            return false
          }
          const index = folders.indexOf(folder)
          if (index >= 0) folders.splice(index, 1)
          await stash()
          settle()
          return true
        } finally {
          folder.busy = null
          settle()
        }
      })
      // ── choosing a local directory ────────────────────────────────────────────────
      // showDirectoryPicker needs the click's transient user activation (about five seconds
      // in Chromium), so it is called synchronously inside the click, with no await in front
      // of it — nothing that can be known beforehand may be awaited first.
      const permissionGranted = async handle => {
        if (!handle || typeof handle.queryPermission !== 'function') return !!handle
        try { return await handle.queryPermission({ mode: 'readwrite' }) === 'granted' } catch { return false }
      }
      const sameEntry = async (left, right) => {
        if (!left || !right) return false
        try { return typeof left.isSameEntry === 'function' ? await left.isSameEntry(right) : left.name === right.name }
        catch { return false }
      }
      // adoptHandle records the directory a person picked and starts serving it. A directory
      // that is already saved is not mounted twice: it resolves to the folder that owns it.
      const adoptHandle = async (folder, handle) => {
        if (disposed) return
        for (const other of folders) {
          if (other !== folder && await sameEntry(other.handle, handle)) {
            if (folder.handle === null) {
              // "添加文件夹" picked a directory that is already in the list.
              setStatus('failed', `${handle.name} 已在列表中：改为连接那个条目`)
              await connect(other)
              return
            }
            folder.error = `${handle.name} 已在列表中，请选择其它目录`
            folder.state = 'error'
            folder.retry = 'connect'
            folder.note = `连接失败：${folder.error}`
            settle()
            return
          }
        }
        folder.handle = handle
        folder.name = handle.name || folder.name
        folder.ready = await permissionGranted(handle)
        if (!folder.ready) {
          folder.state = 'error'
          folder.retry = 'connect'
          folder.error = '目录授权未授予：请重新选择目录'
          folder.note = `连接失败：${folder.error}`
          await stash()
          settle()
          return
        }
        const index = folders.indexOf(folder)
        if (index < 0) folders.push(folder)
        await stash()
        await connect(folder)
      }
      const chooseFolder = (folder, { autoClose = false } = {}) => {
        if (disposed || picking) return
        if (!window.isSecureContext || !window.showDirectoryPicker) {
          openDialog()
          setStatus('failed', '需要 HTTPS 和支持目录访问的浏览器')
          return
        }
        picking = true
        setStatus('picking', '等待选择目录…')
        let picked
        try { picked = window.showDirectoryPicker({ mode: 'readwrite' }) }
        catch (error) {
          picking = false
          setStatus('failed', `挂载失败：${error?.message || String(error)}`)
          return
        }
        Promise.resolve(picked).then(
          handle => { picking = false; return adoptHandle(folder, handle) },
          error => {
            picking = false
            // A cancelled picker changed nothing: no capability, no mount, no failure to
            // report — the row goes back to exactly what it said before the click.
            if (isAbort(error)) { settle(); if (dialogAutoCloses) closeDialog(); return }
            folder.state = 'error'
            folder.retry = 'connect'
            folder.error = explain(error)
            folder.note = `挂载失败：${explain(error)}`
            settle()
          },
        )
      }
      // 添加文件夹: one new saved entry, then the picker for it. The entry is only saved once
      // a directory was actually chosen, so a cancelled picker leaves no empty row behind.
      const addFolder = ({ autoClose = false } = {}) => {
        if (disposed || picking) return
        if (folders.length >= MAX_FOLDERS) { setStatus('failed', `最多保存 ${MAX_FOLDERS} 个目录：请先删除一个`); return }
        const folder = { key: newKey(), id: null, name: '', handle: null, ready: false, workspaceId: null, token: null, mountpoint: null, at: 0, state: 'disconnected', note: '', error: '', retry: null, busy: null }
        chooseFolder(folder, { autoClose })
      }
      // ── what a click does ─────────────────────────────────────────────────────────
      // act is the single-folder decision, shared by the row and the folder list.
      const act = (folder, { autoClose = false } = {}) => {
        if (disposed || folder.busy) return
        // A failed close is the one failure whose retry is not "connect again": the mount is
        // still there and must be released first.
        if (folder.retry === 'close' || folder.state === 'disconnecting') { if (autoClose) openDialog({ autoClose }); void disconnect(folder); return }
        if (folder.state === 'connected') { void disconnect(folder); return }
        if (autoClose) openDialog({ autoClose })
        if (!folder.ready) { chooseFolder(folder, { autoClose }); return }
        void connect(folder)
      }
      // One click, one decision. With no saved folder it is the original gesture: open the
      // picker and mount what the person chooses. With exactly one it is that folder's
      // connect/disconnect. With several, "disconnect which one?" has no obvious answer, so
      // the click opens the folder list instead — the icon at the row's right always does.
      const choose = () => {
        if (disposed || folderBusy()) return
        if (folders.length === 0) { openDialog({ autoClose: true }); addFolder({ autoClose: true }); return }
        if (folders.length === 1) { act(folders[0], { autoClose: true }); return }
        openDialog()
      }
      const armDelete = folder => {
        if (armedTimer !== null) { clearTimeout(armedTimer); armedTimer = null }
        armedDelete = armedDelete === folder.key ? null : folder.key
        notify()
        if (armedDelete !== null) {
          armedTimer = setTimeout(() => { armedTimer = null; armedDelete = null; if (!disposed) notify() }, 4000)
        }
      }
      // ── surfaces ──────────────────────────────────────────────────────────────────
      // The sidebar entry: one row body plus the folder icon at its right. The body click is
      // the whole gesture in the common cases — no consent step, no second confirmation.
      function Action({ wide } = {}) {
        const [, update] = React.useState(0)
        React.useEffect(() => { const listener = () => update(n => n + 1); listeners.add(listener); return () => listeners.delete(listener) }, [])
        // The collapsed rail is one icon column: the text cannot fit there, so it is dropped
        // (and so is the folder icon, which needs a label to make sense) and the row's
        // tooltip carries the state instead.
        const rail = wide === false
        const pressed = connected().length > 0
        return React.createElement('div', {
          className: rail ? 'dshgw-bw-row dshgw-bw-row-rail' : 'dshgw-bw-row',
          'data-dshgw-state': status.phase,
        }, [
          React.createElement('button', {
            key: 'action',
            type: 'button',
            className: rail ? 'dshgw-bw-action dshgw-bw-action-rail' : 'dshgw-bw-action',
            title: entryTitle(),
            'aria-pressed': pressed,
            'aria-label': status.text === '' ? '浏览器工作区' : `浏览器工作区：${status.text}`,
            'data-dshgw-state': status.phase,
            onClick: choose,
          }, [
            React.createElement('span', { key: 'icon', 'aria-hidden': 'true' }, '🖥'),
            React.createElement('span', { key: 'label', className: 'dshgw-bw-label' }, '浏览器工作区'),
            status.text === '' ? null : React.createElement('span', { key: 'note', className: 'dshgw-bw-state' }, status.text),
          ]),
          React.createElement('button', {
            key: 'manage',
            type: 'button',
            className: 'dshgw-bw-manage',
            title: '管理文件夹（添加/删除、连接/断开）',
            'aria-label': '管理文件夹',
            'data-dshgw-manage': 'folders',
            onClick: () => { if (!disposed) openDialog() },
          }, '🗂'),
        ])
      }
      const folderLabel = folder => {
        if (folder.busy !== null || folder.state === 'connecting') return '连接中…'
        if (folder.state === 'disconnecting') return '断开中…'
        if (folder.state === 'connected') return '已连接'
        if (folder.state === 'resumable') return '可恢复（点击连接）'
        if (folder.state === 'elsewhere') return '另一个页面正在使用'
        if (folder.state === 'error') return '失败'
        return folder.ready === false ? '未连接（需要重新选择）' : '未连接'
      }
      const folderRow = folder => {
        const actions = []
        const busy = folder.busy !== null
        if (folder.state === 'connected') {
          actions.push(React.createElement('button', {
            key: 'open', type: 'button', disabled: busy, 'data-dshgw-folder-action': 'open',
            onClick: () => {
              if (!folder.workspaceId) return
              ctx.logger?.info?.('browser-workspace: reopening the folder workspace')
              Promise.resolve(ctx.uiWorkspace.connectWorkspace(folder.workspaceId)).catch(error => {
                folder.error = explain(error)
                settle()
              })
            },
          }, '打开'))
        }
        actions.push(React.createElement('button', {
          key: 'toggle',
          type: 'button',
          disabled: busy,
          'data-dshgw-folder-action': folder.state === 'connected' || folder.retry === 'close' ? 'disconnect' : 'connect',
          onClick: () => act(folder),
        }, folder.retry === 'close' ? '重试断开' : folder.state === 'connected' ? '断开' : '连接'))
        actions.push(React.createElement('button', {
          key: 'delete',
          type: 'button',
          disabled: busy,
          'data-dshgw-folder-action': armedDelete === folder.key ? 'confirm-delete' : 'delete',
          onClick: () => {
            if (armedDelete === folder.key) { armedDelete = null; if (armedTimer !== null) { clearTimeout(armedTimer); armedTimer = null } void remove(folder) }
            else armDelete(folder)
          },
        }, armedDelete === folder.key ? '确认删除' : '删除'))
        return React.createElement('div', {
          key: folder.key,
          className: 'dshgw-bw-folder',
          'data-dshgw-folder': folder.key,
          'data-dshgw-folder-state': folder.state,
        }, [
          React.createElement('div', { key: 'name', className: 'dshgw-bw-folder-name' }, [
            React.createElement('span', { key: 'label' }, folder.name === '' ? '（未选择目录）' : folder.name),
            React.createElement('span', { key: 'state', className: 'dshgw-bw-folder-state' }, folderLabel(folder)),
            folder.error === '' ? null : React.createElement('span', { key: 'error', className: 'dshgw-bw-folder-error' }, folder.error),
          ]),
          React.createElement('div', { key: 'actions', className: 'dshgw-bw-folder-actions' }, actions),
        ])
      }
      // The folder window, in the frame-wide overlay list slot: the same status the row
      // carries, plus one row per saved folder with its own connect/disconnect/delete.
      function Dialog() {
        const [, update] = React.useState(0)
        React.useEffect(() => { const listener = () => update(n => n + 1); listeners.add(listener); return () => listeners.delete(listener) }, [])
        if (!dialogOpen) return null
        const failed = status.phase === 'failed'
        const mounted = connected().length > 0
        const resumable = folders.some(folder => folder.state === 'resumable')
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
          }, status.text === '' ? '还没有目录：点击「添加文件夹」把本机目录挂载成工作区。' : status.text),
          React.createElement('div', { className: 'dshgw-bw-folders', key: 'folders' },
            folders.length === 0
              ? React.createElement('div', { className: 'dshgw-bw-empty', key: 'empty' }, `还没有保存的目录（最多 ${MAX_FOLDERS} 个）。`)
              : folders.map(folderRow)),
          mounted ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'auto' }, '断开只卸载目录并保留工作区：再次连接会回到同一个路径与同一个工作区。') : null,
          resumable ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'resume' }, '「可恢复」的目录只发一条 resume 就接回刷新前挂载的同一个挂载，不会重新挂载，也不会重启 worker。') : null,
          failed ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'retry' }, '修复后可以再点一次「连接」重试。') : null,
          React.createElement('div', { className: 'dshgw-bw-footer', key: 'footer' }, [
            React.createElement('button', {
              key: 'add', type: 'button', disabled: picking || folders.length >= MAX_FOLDERS,
              'data-dshgw-action': 'add', onClick: () => addFolder(),
            }, picking ? '等待选择目录…' : '添加文件夹'),
            React.createElement('button', { key: 'close', type: 'button', onClick: closeDialog }, '关闭'),
          ]),
        ]))
      }
      // What the previous document left behind, read once per page: the saved folders, each
      // with a directory handle this browser has already granted. It only moves each row into
      // its "可恢复"/"未连接" state — the connect itself is the operator's click.
      ctx.effect(() => {
        let cancelled = false
        records.load().then(async stored => {
          if (cancelled || disposed || stored.length === 0) return
          for (const row of stored) {
            // One key names one mount point, so a duplicate record (a half-written list, a
            // hand-edited store) must not become two folders fighting over the same path.
            if (folders.some(existing => existing.key === row.key)) continue
            const folder = {
              // The handle is the live truth about the directory's name: the stored one was
              // written by whichever document saved the folder.
              key: row.key, id: row.id || null, name: row.handle?.name || row.name || '目录',
              handle: row.handle, ready: false, workspaceId: row.workspaceId || null,
              token: null, mountpoint: row.mountpoint || null, at: 0,
              state: 'disconnected', note: '', error: '', retry: null, busy: null,
            }
            // The token is only worth keeping while the gateway could still hold that mount;
            // an expired one is dropped HERE, and the folder simply stays saved and offline.
            if (typeof row.token === 'string' && row.token !== '' && recordIsFresh(row)) {
              folder.token = row.token
              folder.at = row.at
            }
            const granted = await permissionGranted(row.handle)
            if (cancelled || disposed) return
            folder.ready = granted
            if (folder.token && granted) folder.state = 'resumable'
            folder.note = describe(folder)
            folders.push(folder)
          }
          await stash()
          if (cancelled || disposed) return
          settle()
        }).catch(error => { ctx.logger?.warn?.('browser-workspace: reading the saved folders failed: ' + (error?.message || error)) })
        return () => { cancelled = true }
      }, 'browser-workspace: stored folders lookup')
      // order 90 keeps this row above the ssh-workspace entry (order 100); order 210 keeps
      // this window above the ssh-workspace one (200) when both happen to be open.
      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({ name: 'sidebar.footer.action', id: 'browser-workspace', order: 90, label: '浏览器工作区' }, Action)), 'browser-workspace: sidebar entry')
      ctx.effect(() => ctx.slots.inject('shell.overlay', () => ctx.slots.register({ name: 'shell.overlay', id: 'browser-workspace-dialog', order: 210, label: '浏览器工作区' }, Dialog)), 'browser-workspace: mount dialog')
      // Disposal is a page going away, NOT a decision to unmount: a reload lands here too, and
      // the whole point of the saved folders is that the next document can take those mounts
      // back. So the records are left alone on purpose — only an explicit stop clears a token.
      ctx.effect(() => () => {
        disposed = true
        clearAutoClose()
        if (armedTimer !== null) { clearTimeout(armedTimer); armedTimer = null }
        for (const key of [...shares.keys()]) {
          const share = shares.get(key)
          share.stopped = true
          share.controller.abort()
          // Best effort: the gateway's grace window and lease reaper are what make an
          // unacknowledged close recoverable, so this is reported and then left to them. The
          // record keeps its token on purpose — the next document can still resume the mount
          // the gateway is holding.
          void call('close', { token: share.token }, 55000).catch(error => {
            ctx.logger?.warn?.('browser-workspace: cleanup unconfirmed on unload; the gateway lease reaper remains necessary: ' + (error?.message || error))
          })
        }
        shares.clear()
        listeners.clear()
      }, 'browser-workspace: cleanup')
    }
    // 'remote' AND 'remote.workspace' are both required: Cordis resolves the dotted name
    // as its own service, but every `ctx.remote.workspace.*` call below first reads the
    // parent `ctx.remote`, and without 'remote' in this list that read throws
    // `cannot get property "remote" without inject` inside a real DSH GUI (the same pair
    // @deepseek-ai/dsh-api-workspace-controller declares). A mocked ctx that hands the
    // plugin a ready-made `remote` object cannot catch this.
    return { inject: ['slots', 'connection', 'remote', 'remote.workspace', 'uiWorkspace'], apply, createExecutor, checkRelativePath, createTransport, errorOf, createRecordStore, reopenHandle, recordIsFresh, newKey, MAX_FOLDERS, AUTO_CLOSE_MS, RECONNECT_GRACE_MS }
  },
})
