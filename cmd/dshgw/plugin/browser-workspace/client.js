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
        if (!['open', 'poll', 'respond', 'close', 'activate'].includes(endpoint)) throw failure('EINVAL', 'invalid endpoint')
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
      // Phases: idle | stopping | picking | mounting | activating | mounted | failed. The
      // phase rides on the row as `data-dshgw-state` so a real browser run can assert it
      // without reading colours or parsing the note.
      let status = { phase: 'idle', text: '' }
      let dialogOpen = false
      let autoCloseTimer = null
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
      // One click, one picker. showDirectoryPicker() needs transient user activation (about
      // five seconds in Chromium), so it is called synchronously by the click — anything
      // awaited first, including a consent step of its own, risks "Must be handling a user
      // gesture". The warning is not a gate: it lives on the row (title) and in the dialog
      // that this click opens. Opening that dialog is a synchronous store write, so the
      // picker still runs inside the click's own task.
      const choose = async () => {
        if (disposed || choosing) return
        if (active) { choosing = true; try { await stop() } finally { choosing = false }; return }
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
          const share = { token: opened.token, name: root.name, execute: createExecutor(root, { writable: true }), controller: new AbortController(), stopped: false, closing: null }
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
          const workspace = await createWorkspace(share, opened.mountpoint)
          if (!live(share)) return
          try {
            const priorGeneration = ctx.connection.generation.getSnapshot()?.id
            const activated = await call('activate', { token: share.token }, 55000, share.controller.signal)
            if (!live(share)) return
            setStatus('activating', '挂载完成；等待 worker 重连…')
            await awaitReconnect(share, priorGeneration)
            if (!live(share)) return
            try {
              const renamed = await ctx.remote.workspace.rename({ workspaceId: workspace.workspaceId, title: `本地: ${root.name}` })
              if (!renamed?.ok) ctx.logger?.warn?.('browser-workspace: workspace rename failed')
            } catch { ctx.logger?.warn?.('browser-workspace: workspace rename failed') }
            if (!live(share)) return
            await ctx.uiWorkspace.connectWorkspace(workspace.workspaceId)
            if (live(share)) {
              setStatus('mounted', `已挂载 ${root.name}（读写）；点击断开`)
              if (dialogOpen) scheduleAutoClose()
            }
          } catch (error) {
            await forgetWorkspace(workspace)
            throw error
          }
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
          failed ? React.createElement('p', { className: 'dshgw-bw-muted', key: 'retry' }, '修复后可以再点一次侧栏那一行重试。') : null,
          React.createElement('div', { className: 'dshgw-bw-footer', key: 'footer' },
            React.createElement('button', { key: 'close', type: 'button', onClick: closeDialog }, '关闭')),
        ]))
      }
      // order 90 keeps this row above the ssh-workspace entry (order 100); order 210 keeps
      // this dialog above the ssh-workspace one (200) when both happen to be open.
      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({ name: 'sidebar.footer.action', id: 'browser-workspace', order: 90, label: '浏览器工作区' }, Action)), 'browser-workspace: sidebar entry')
      ctx.effect(() => ctx.slots.inject('shell.overlay', () => ctx.slots.register({ name: 'shell.overlay', id: 'browser-workspace-dialog', order: 210, label: '浏览器工作区' }, Dialog)), 'browser-workspace: mount dialog')
      ctx.effect(() => () => { disposed = true; clearAutoClose(); void stop(); listeners.clear() }, 'browser-workspace: cleanup')
    }
    // 'remote' AND 'remote.workspace' are both required: Cordis resolves the dotted name
    // as its own service, but every `ctx.remote.workspace.*` call below first reads the
    // parent `ctx.remote`, and without 'remote' in this list that read throws
    // `cannot get property "remote" without inject` inside a real DSH GUI (the same pair
    // @deepseek-ai/dsh-api-workspace-controller declares). A mocked ctx that hands the
    // plugin a ready-made `remote` object cannot catch this.
    return { inject: ['slots', 'connection', 'remote', 'remote.workspace', 'uiWorkspace'], apply, createExecutor, checkRelativePath, createTransport, errorOf, AUTO_CLOSE_MS }
  },
})
