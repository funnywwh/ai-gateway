// Hand-built ModuleLoader bundle, served byte-for-byte like ssh-workspace.
window.__ModuleLoader__.load({
  id: 'dshgw-browser-workspace',
  factory: require => {
    const React = require('react')
    const MAX_BYTES = 1024 * 1024
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
      const call = createTransport()
      let active = null, disposed = false, choosing = false
      const listeners = new Set()
      let status = '挂载本地目录（读写）'
      const show = text => { status = text; for (const listener of listeners) listener() }
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
        show('正在断开并清理挂载；请保持页面打开')
        share.closing = (async () => {
          try {
            await call('close', { token: share.token }, 55000)
            if (active === share) active = null
            show('已断开；挂载本地目录（读写）')
            return true
          } catch (error) {
            // Retain capability for an explicit cleanup retry; never fake success.
            show(`清理未确认：${error.message}；点击重试断开，请勿关闭页面`)
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
          if (live(share) && await stop()) show(`已断线：${error.message}；请重新选择目录`)
        }
      }
      const register = async (share, path, priorGeneration) => {
        // ConnectionHandle state/generation/reconnect are public DSH APIs. The
        // gateway's worker restart completes before the browser wire reconnects.
        const connection = ctx.connection
        if (connection.generation.getSnapshot()?.id === priorGeneration || connection.state.getSnapshot() !== 'connected') connection.reconnect()
        const deadline = Date.now() + 30000
        let lastError = failure('ETIMEDOUT', '等待工作区连接超时')
        while (live(share) && Date.now() < deadline) {
          if (connection.state.getSnapshot() === 'connected' && connection.generation.getSnapshot()?.id !== priorGeneration) {
            let timer
            try {
              // create(path) is idempotent, unlike opening a new workspace session.
              const created = await Promise.race([
                ctx.remote.workspace.create({ path }),
                new Promise((_, reject) => { timer = setTimeout(() => reject(failure('ETIMEDOUT', 'workspace registration timed out')), 5000) }),
              ])
              if (created?.ok) return created.value.workspace
              lastError = created?.error || failure('EIO', 'workspace registration failed')
            } catch (error) { lastError = error }
            finally { clearTimeout(timer) }
          }
          await delay(500, share.controller.signal)
        }
        throw lastError
      }
      const choose = async () => {
        if (disposed || choosing) return
        if (active) { choosing = true; try { await stop() } finally { choosing = false }; return }
        if (!window.isSecureContext || !window.showDirectoryPicker) { show('需要 HTTPS 和支持目录访问的浏览器'); return }
        if (!window.confirm('允许远程 AI 读取、修改、重命名和删除所选目录中的文件；读取的文件内容可能发送给配置的 AI 模型服务商。挂载及卸载会重启账户 worker，可能中断当前任务或连接。请选择专用目录并先备份，不要选择密钥或敏感目录。保持此页面打开；断线会导致远程文件操作失败。是否继续？')) return
        choosing = true
        try {
          const root = await window.showDirectoryPicker({ mode: 'readwrite' })
          if (disposed) return
          const opened = await call('open', { name: root.name, writable: true })
          if (!opened?.token) throw failure('EIO', 'invalid open handshake')
          const share = { token: opened.token, execute: createExecutor(root, { writable: true }), controller: new AbortController(), stopped: false, closing: null }
          active = share
          if (disposed) { await stop(); return }
          if (!opened.mountpoint) throw failure('EIO', 'invalid open mountpoint')
          const priorGeneration = ctx.connection.generation.getSnapshot()?.id
          show(`正在挂载 ${root.name} 并重启 worker；请保持页面打开`)
          void poll(share)
          const activated = await call('activate', { token: share.token }, 55000, share.controller.signal)
          if (!live(share)) return
          show('挂载完成；等待 worker 重新连接并注册工作区')
          const workspace = await register(share, activated.mountpoint || opened.mountpoint, priorGeneration)
          if (!live(share)) return
          try {
            const renamed = await ctx.remote.workspace.rename({ workspaceId: workspace.workspaceId, title: `本地: ${root.name}` })
            if (!renamed?.ok) ctx.logger?.warn?.('browser-workspace: workspace rename failed')
          } catch { ctx.logger?.warn?.('browser-workspace: workspace rename failed') }
          if (!live(share)) return
          await ctx.uiWorkspace.connectWorkspace(workspace.workspaceId)
          if (live(share)) show(`断开 ${root.name}（读写；保持页面打开）`)
        } catch (error) {
          // AbortError is harmless only when the picker was cancelled before open;
          // an activation timeout still requires cleanup of the retained token.
          if (active) { if (await stop()) show(`挂载失败：${error.message || String(error)}`) }
          else if (error.name !== 'AbortError') show(`挂载失败：${error.message || String(error)}`)
        } finally { choosing = false }
      }
      function Action() {
        const [, update] = React.useState(0)
        React.useEffect(() => { const listener = () => update(n => n + 1); listeners.add(listener); return () => listeners.delete(listener) }, [])
        return React.createElement('button', { type: 'button', onClick: choose, title: '文件内容可能发送给 AI 模型；挂载/卸载重启 worker；关闭页面会断开挂载' }, status)
      }
      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({ name: 'sidebar.footer.action', id: 'browser-workspace', order: 110, label: '本地目录' }, Action)), 'browser-workspace: sidebar entry')
      ctx.effect(() => () => { disposed = true; void stop(); listeners.clear() }, 'browser-workspace: cleanup')
    }
    return { inject: ['slots', 'connection', 'remote.workspace', 'uiWorkspace'], apply, createExecutor, checkRelativePath, createTransport, errorOf }
  },
})
