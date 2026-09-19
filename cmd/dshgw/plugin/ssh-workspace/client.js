// Browser half of the ssh-workspace feature (M64).
//
// Prebuilt by hand on purpose: this installation ships no bundler, and dsh serves a client
// plugin's ./client file byte for byte. The contract is exactly what any shipped surface
// does — call window.__ModuleLoader__.load with this package's name, and export `apply` plus
// the service names the plugin needs.
//
// The surface is two list slots, so nothing shipped is shadowed: a compact action in the
// sidebar foot opens a dialog that lives in the frame-wide overlay layer. Nothing here mounts
// anything: the plugin asks the gateway through the mailbox (index.js) and, once the gateway
// confirms the mount and the account's dsh has been restarted with it, this half registers the
// resulting directory as a workspace and opens a session in it.

window.__ModuleLoader__.load({
  id: 'dshgw-ssh-workspace',
  factory: (require) => {
    // The wrapper every shipped bundle uses: the page's loader hands the factory a CommonJS
    // style `require`, and it must return `module.exports`.
    var module = { exports: {} }
    var exports = module.exports
    const React = require('react')
    const h = React.createElement

    /** Required services: slot registry, authenticated RPC carrier, workspace control. */
    const inject = ['slots', 'connection', 'remote.workspace', 'uiWorkspace']

    const RPC_CHANNEL = '/ssh-workspace'
    const POLL_MS = 2000
    const MAX_KEY_BYTES = 64 * 1024
    function composeHost(address, username = '', port = '') {
      const raw = address.trim()
      if (!raw) throw new Error('请输入主机地址')
      const m = /^(?:([^@\s:]+)@)?([^@:\s]+)(?::([0-9]+))?$/.exec(raw)
      if (!m) throw new Error('主机格式无效')
      const user = username.trim() || m[1] || ''
      if (user && !/^[^@\s:]+$/.test(user)) throw new Error('用户名无效')
      const selected = port.trim() || m[3] || ''
      if (selected && (!/^\d+$/.test(selected) || Number(selected) < 1 || Number(selected) > 65535)) throw new Error('SSH 端口必须为 1..65535')
      return `${user ? `${user}@` : ''}${m[2]}${selected ? `:${Number(selected)}` : ''}`
    }
    const CSS = `
.dshgw-ssh-action { display: flex; align-items: center; gap: 6px; width: 100%; background: none; border: 0; color: inherit; font: inherit; cursor: pointer; padding: 6px 8px; border-radius: 6px; }
.dshgw-ssh-action:hover { background: rgba(127,127,127,.14); }
.dshgw-ssh-action-rail .dshgw-ssh-label { display: none; }
/* The shell renders this whole slot as one flex ROW, which leaves this row and the
   browser-workspace one sharing a foot that only fits one. A plugin owns no wrapper element
   around its own row: the list slot wraps every registration in a classless div, so the
   shell's container is TWO levels up, and :has() is the only selector that can reach it from
   here. Both shapes are covered, and both rows ship this rule, so it holds whichever of the
   two plugins a deployment enables. */
div:has(> .dshgw-bw-action), div:has(> div > .dshgw-bw-action),
div:has(> .dshgw-ssh-action), div:has(> div > .dshgw-ssh-action) { flex-direction: column; }
.dshgw-ssh-backdrop { position: fixed; inset: 0; display: flex; align-items: center; justify-content: center; background: rgba(0,0,0,.45); z-index: 40; }
.dshgw-ssh-dialog { width: min(720px, 92vw); max-height: 86vh; overflow: auto; background: var(--dsh-bg, #1b1c1f); color: var(--dsh-fg, #e6e6e6); border: 1px solid rgba(127,127,127,.35); border-radius: 10px; padding: 16px 18px; font-size: 13px; line-height: 1.5; }
.dshgw-ssh-dialog h2 { margin: 0 0 4px; font-size: 15px; }
.dshgw-ssh-dialog p.hint { margin: 0 0 12px; opacity: .7; }
.dshgw-ssh-row { display: flex; gap: 8px; align-items: center; margin: 8px 0; }
.dshgw-ssh-row label { min-width: 68px; opacity: .8; }
.dshgw-ssh-dialog input[type=text] { flex: 1; background: rgba(0,0,0,.25); color: inherit; border: 1px solid rgba(127,127,127,.4); border-radius: 6px; padding: 6px 8px; font: inherit; }
.dshgw-ssh-dialog button { background: rgba(127,127,127,.18); color: inherit; border: 1px solid rgba(127,127,127,.35); border-radius: 6px; padding: 6px 10px; font: inherit; cursor: pointer; }
.dshgw-ssh-dialog button[disabled] { opacity: .5; cursor: default; }
.dshgw-ssh-dialog button.primary { background: #2f6feb; border-color: #2f6feb; color: #fff; }
.dshgw-ssh-list { border: 1px solid rgba(127,127,127,.3); border-radius: 6px; max-height: 220px; overflow: auto; margin: 6px 0; }
.dshgw-ssh-item { display: flex; gap: 8px; align-items: center; justify-content: space-between; padding: 4px 8px; border-bottom: 1px solid rgba(127,127,127,.14); }
.dshgw-ssh-item:last-child { border-bottom: 0; }
.dshgw-ssh-item button { padding: 2px 8px; }
.dshgw-ssh-crumbs { display: flex; flex-wrap: wrap; gap: 4px; opacity: .85; margin: 4px 0; }
.dshgw-ssh-crumbs button { padding: 2px 6px; }
.dshgw-ssh-error { border: 1px solid #b3453c; background: rgba(179,69,60,.18); border-radius: 6px; padding: 8px 10px; margin: 8px 0; white-space: pre-wrap; }
.dshgw-ssh-notice { border: 1px solid #3a7d44; background: rgba(58,125,68,.18); border-radius: 6px; padding: 8px 10px; margin: 8px 0; }
.dshgw-ssh-muted { opacity: .65; }
.dshgw-ssh-footer { display: flex; justify-content: flex-end; gap: 8px; margin-top: 12px; }
.dshgw-ssh-section { margin-top: 14px; border-top: 1px solid rgba(127,127,127,.25); padding-top: 10px; }
`

    /** Tiny observable store shared by the two slots (they are separate registrations). */
    const listeners = new Set()
    let state = {
      open: false,
      busy: '',
      error: '',
      notice: '',
      host: '',
      username: '',
      port: '',
      composedHost: '',
      identityStatus: { default: { configured: false }, host: { configured: false }, effective: 'none' },
      identityScope: 'default',
      identityKey: '',
      remote: '',
      aliases: [],
      allowList: [],
      home: '',
      mountSubdir: 'ssh',
      identity: false,
      remoteHome: '',
      listing: null,
      newName: '',
      mounts: [],
      replies: [],
      mirror: false,
    }
    const emit = () => {
      for (const listener of [...listeners]) listener()
    }
    const patch = (next) => {
      state = { ...state, ...next }
      emit()
    }

    /** Human text for one business failure. */
    const textOf = (error) => {
      if (error === null || error === undefined) return 'unknown failure'
      const code = error.code ? `${error.code}: ` : ''
      return `${code}${error.message || String(error)}`
    }

    /** Install the stylesheet once. */
    function installCSS() {
      if (document.querySelector('style[data-plugin-css="dshgw-ssh-workspace/client.css"]') !== null) return
      const tag = document.createElement('style')
      tag.dataset.pluginCss = 'dshgw-ssh-workspace/client.css'
      tag.textContent = CSS
      document.head.appendChild(tag)
    }

    /**
     * Plugin body.
     * @param ctx - client root context.
     */
    function apply(ctx) {
      installCSS()

      // One RPC call, unwrapped into a promise that rejects with the business failure.
      const call = async (endpoint, payload) => {
        const result = await ctx.connection.rpc.call(RPC_CHANNEL, endpoint, payload)
        if (!result || result.ok !== true) throw (result && result.error) || { code: 'gateway/unavailable', message: 'the account-side plugin did not answer' }
        return result.value
      }

      // The workspace registration path: create (idempotent by path), give it the remote
      // spelling as its title, then open a session in it.
      const openWorkspace = async (mountpoint, title) => {
        const created = await ctx.remote.workspace.create({ path: mountpoint })
        if (created.ok !== true) throw created.error
        const workspace = created.value.workspace
        if (workspace.title !== title) {
          // Best effort: the registry defaults the title to the last path segment, which says
          // nothing about where the directory actually lives.
          const renamed = await ctx.remote.workspace.rename({ workspaceId: workspace.workspaceId, title })
          if (renamed.ok !== true) ctx.logger?.warn?.(`ssh-workspace: rename failed: ${textOf(renamed.error)}`)
        }
        await ctx.uiWorkspace.connectWorkspace(workspace.workspaceId)
        patch({ open: false, notice: '' })
      }

      let hostRevision = 0
      const currentHost = () => composeHost(state.host, state.username, state.port)
      const refreshIdentity = async (host = '') => {
        const rev = ++hostRevision
        try { const value = await call('identityStatus', host ? { host } : {}); if (rev === hostRevision) patch({ identityStatus: value }) }
        catch (error) { if (rev === hostRevision) patch({ error: textOf(error) }) }
      }
      const refreshHosts = async () => {
        const rev = ++hostRevision
        const value = await call('hosts', {})
        if (rev !== hostRevision) return
        patch({
          aliases: value.aliases ?? [],
          allowList: value.allowList ?? [],
          home: value.home ?? '',
          mountSubdir: value.mountSubdir ?? 'ssh',
          identity: value.identity === true,
          host: state.host || (value.aliases ?? [])[0]?.name || '',
        })
        const address = state.host || (value.aliases ?? [])[0]?.name || ''
        if (!address) { await refreshIdentity(); return }
        const host = composeHost(address, state.username, state.port)
        patch({ composedHost: host })
        await refreshIdentity(host)
      }

      const refreshMounts = async () => {
        const value = await call('mounts', {})
        patch({ mounts: value.mounts ?? [], replies: value.replies ?? [], mirror: value.mirror === true })
      }

      const probe = async () => {
        patch({ busy: 'probe', error: '', notice: '' })
        try {
          const value = await call('probe', { host: currentHost() })
          patch({ busy: '', remoteHome: value.home ?? '', remote: state.remote || value.home || '' })
          if (state.remote || value.home) await browse(state.remote || value.home)
        } catch (error) {
          patch({ busy: '', error: textOf(error) })
        }
      }

      const browse = async (path) => {
        patch({ busy: 'list', error: '' })
        try {
          const value = await call('list', { host: currentHost(), path })
          patch({ busy: '', listing: value, remote: value.path })
        } catch (error) {
          patch({ busy: '', error: textOf(error) })
        }
      }

      const createDirectory = async () => {
        const name = state.newName.trim()
        if (name === '' || state.listing === null) return
        patch({ busy: 'mkdir', error: '' })
        try {
          await call('mkdir', { host: currentHost(), path: state.listing.path, name })
          patch({ busy: '', newName: '' })
          await browse(state.listing.path)
        } catch (error) {
          patch({ busy: '', error: textOf(error) })
        }
      }

      const uploadIdentity = async () => {
        if (state.identityKey.length > MAX_KEY_BYTES) { patch({ error: '私钥不能超过 64 KiB' }); return }
        if (!state.identityKey.trim()) return
        if (!window.confirm('确认上传并替换此范围的 SSH 私钥？')) return
        if (/ENCRYPTED|BEGIN OPENSSH PRIVATE KEY/.test(state.identityKey) && /ENCRYPTED/.test(state.identityKey)) { patch({ error: '不支持带密码私钥' }); return }
        const rev = ++hostRevision
        patch({ busy: 'identity', error: '' })
        try { const host = state.host.trim() ? currentHost() : ''; const value = await call('identityUpload', { scope: state.identityScope, ...(host ? { host } : {}), privateKey: state.identityKey }); patch({ busy: '', identityKey: '', ...(rev === hostRevision ? { identityStatus: value } : {}) }) }
        catch (error) { patch({ busy: '', error: textOf(error) }) }
      }
      const deleteIdentity = async () => {
        if (!window.confirm('确认删除此 SSH 私钥？')) return
        const rev = ++hostRevision
        patch({ busy: 'identity', error: '' })
        try { const host = state.host.trim() ? currentHost() : ''; const value = await call('identityDelete', { scope: state.identityScope, ...(host ? { host } : {}) }); patch({ busy: '', ...(rev === hostRevision ? { identityStatus: value } : {}) }) }
        catch (error) { patch({ busy: '', error: textOf(error) }) }
      }

      // Ask the gateway to mount. Nothing is registered here: the account's dsh is restarted
      // with the new binding, so the dialog waits for the mount to appear and then offers it.
      const requestMount = async () => {
        patch({ busy: 'open', error: '', notice: '' })
        try {
          const value = await call('open', { host: currentHost(), remote: state.remote })
          patch({
            busy: '',
            notice: `已请求挂载 ${value.host}:${value.remote}。网关完成挂载后会重载该账号的 DSH，届时点“打开”即可进入工作区（页面可能需要刷新）。`,
          })
          if (pollTimer === null) {
            pollTimer = window.setInterval(() => {
              refreshMounts().catch(() => {})
            }, POLL_MS)
          }
          await refreshMounts()
        } catch (error) {
          patch({ busy: '', error: textOf(error) })
        }
      }

      const unmount = async (mountpoint) => {
        patch({ busy: 'close', error: '', notice: '' })
        try {
          await call('close', { mountpoint })
          patch({ busy: '', notice: '已请求卸载；该账号的 DSH 会再次重载以移除绑定。' })
        } catch (error) {
          patch({ busy: '', error: textOf(error) })
        }
      }

      const acknowledge = async (id) => {
        try {
          await call('ack', { id })
          await refreshMounts()
        } catch (error) {
          patch({ error: textOf(error) })
        }
      }

      let pollTimer = null

      /** The sidebar-foot action: the entry point for the whole feature. */
      function SidebarAction({ wide } = {}) {
        const current = React.useSyncExternalStore
          ? React.useSyncExternalStore((listener) => {
            listeners.add(listener)
            return () => listeners.delete(listener)
          }, () => state)
          : state
        return h('button', {
          type: 'button',
          // The collapsed rail is one icon column: the label cannot fit there.
          className: wide === false ? 'dshgw-ssh-action dshgw-ssh-action-rail' : 'dshgw-ssh-action',
          title: 'SSH 工作区',
          'aria-pressed': current.open,
          onClick: async () => {
            patch({ open: true, error: '', notice: '' })
            try {
              await refreshHosts()
              await refreshMounts()
            } catch (error) {
              patch({ error: textOf(error) })
            }
          },
        }, h('span', { 'aria-hidden': 'true' }, '⇄'), h('span', { className: 'dshgw-ssh-label' }, 'SSH 工作区'))
      }

      /** The dialog, in the frame-wide overlay list slot. */
      function Dialog() {
        const current = React.useSyncExternalStore
          ? React.useSyncExternalStore((listener) => {
            listeners.add(listener)
            return () => listeners.delete(listener)
          }, () => state)
          : state
        if (!current.open) return null
        const crumbs = []
        let accumulated = ''
        for (const segment of (current.listing?.path ?? '').split('/').filter((part) => part !== '')) {
          accumulated += `/${segment}`
          crumbs.push({ name: segment, path: accumulated })
        }
        const rows = []
        rows.push(h('div', { className: 'dshgw-ssh-row', key: 'host' }, [
          h('label', { key: 'label' }, '主机'),
          h('input', {
            key: 'input',
            type: 'text',
            list: 'dshgw-ssh-hosts',
            value: current.host,
            placeholder: 'gpt001 或 user@host',
            onChange: (event) => { patch({ host: event.target.value }); refreshIdentitySafe() },
          }),
          h('datalist', { id: 'dshgw-ssh-hosts', key: 'list' },
            current.aliases.map((alias) => h('option', { key: alias.name, value: alias.name }, alias.hostName || alias.name))),
          h('button', { key: 'probe', type: 'button', disabled: current.busy !== '', onClick: probe }, '探测'),
        ]))
        rows.push(h('div', { className: 'dshgw-ssh-row', key: 'login' }, [
          h('label', { key: 'user-label' }, '用户名'), h('input', { key: 'user', type: 'text', disabled: current.busy !== '', value: current.username, placeholder: '可选', onChange: (e) => { patch({ username: e.target.value }); hostRevision++; refreshIdentitySafe() } }),
          h('label', { key: 'port-label' }, '端口'), h('input', { key: 'port', type: 'text', disabled: current.busy !== '', value: current.port, placeholder: 'SSH config / 22', onChange: (e) => { patch({ port: e.target.value }); hostRevision++; refreshIdentitySafe() } }),
        ]))
        const refreshIdentitySafe = async () => { const rev = ++hostRevision; patch({ listing: null, remoteHome: '', remote: '', identityStatus: { default: { configured: false }, host: { configured: false }, effective: 'none' }, composedHost: '', identityKey: '' }); try { const host = currentHost(); patch({ composedHost: host }); const value = await call('identityStatus', { host }); if (rev === hostRevision) patch({ identityStatus: value }) } catch (error) { if (rev === hostRevision) patch({ error: textOf(error) }) } }
        rows.push(h('div', { className: 'dshgw-ssh-section', key: 'identity' }, [
          h('h3', { key: 'title', style: { margin: '0 0 6px', fontSize: '13px' } }, 'SSH 私钥'),
          h('p', { key: 'status', className: 'dshgw-ssh-muted' }, `绑定主机：${current.composedHost || '（未填写）'}。当前生效：${current.identityStatus.effective || 'none'}；默认 ${current.identityStatus.default?.configured ? '已配置' : '未配置'}${current.identityStatus.default?.fingerprint ? ` (${current.identityStatus.default.fingerprint})` : ''}；主机专用 ${current.identityStatus.host?.configured ? '已配置' : '未配置'}${current.identityStatus.host?.fingerprint ? ` (${current.identityStatus.host.fingerprint})` : ''}`),
          h('select', { key: 'scope', disabled: current.busy !== '', value: current.identityScope, onChange: (e) => patch({ identityScope: e.target.value, identityKey: '' }) }, [h('option', { key: 'default', value: 'default' }, '账号默认'), h('option', { key: 'host', value: 'host' }, '当前主机专用')]),
          h('input', { key: 'key', type: 'file', disabled: current.busy !== '', accept: '.pem,.key,id_rsa,id_ed25519', onChange: async (e) => { const f=e.target.files?.[0]; patch({ identityKey: '' }); if (!f) return; if (f.size > MAX_KEY_BYTES) { patch({ error: '私钥不能超过 64 KiB' }); return }; patch({ busy: 'identity' }); try { const text=await f.text(); patch({ busy: '', identityKey: text }) } catch (error) { patch({ busy: '', error: textOf(error) }) } } }),
          h('button', { key: 'upload', type: 'button', disabled: current.busy !== '' || !current.identityKey, onClick: uploadIdentity }, '上传/替换'),
          h('button', { key: 'delete', type: 'button', disabled: current.busy !== '' || !(current.identityStatus[current.identityScope]?.configured), onClick: deleteIdentity }, '删除'),
        ]))
        if (current.remoteHome !== '') {
          rows.push(h('p', { className: 'dshgw-ssh-muted', key: 'remote-home' }, `远端 HOME：${current.remoteHome}`))
        }
        rows.push(h('div', { className: 'dshgw-ssh-row', key: 'path' }, [
          h('label', { key: 'label' }, '远端目录'),
          h('input', {
            key: 'input',
            type: 'text',
            value: current.remote,
            placeholder: '/opt/app',
            onChange: (event) => patch({ remote: event.target.value }),
            onKeyDown: (event) => {
              if (event.key === 'Enter') browse(current.remote).catch(() => {})
            },
          }),
          h('button', { key: 'browse', type: 'button', disabled: current.busy !== '', onClick: () => browse(current.remote).catch(() => {}) }, '浏览'),
        ]))
        if (crumbs.length > 0) {
          rows.push(h('div', { className: 'dshgw-ssh-crumbs', key: 'crumbs' },
            crumbs.map((crumb) => h('button', { key: crumb.path, type: 'button', onClick: () => browse(crumb.path).catch(() => {}) }, crumb.name))))
        }
        if (current.listing !== null) {
          const entries = current.listing.entries ?? []
          rows.push(h('div', { className: 'dshgw-ssh-list', key: 'listing' },
            entries.length === 0
              ? h('div', { className: 'dshgw-ssh-item' }, h('span', { className: 'dshgw-ssh-muted' }, '（没有子目录）'))
              : entries.map((entry) => h('div', { className: 'dshgw-ssh-item', key: entry.path }, [
                h('span', { key: 'name' }, `${entry.hidden ? '· ' : ''}${entry.name}/`),
                h('button', { key: 'enter', type: 'button', onClick: () => browse(entry.path).catch(() => {}) }, '进入'),
              ]))))
          if (current.listing.truncated === true) {
            rows.push(h('p', { className: 'dshgw-ssh-muted', key: 'truncated' }, '目录项过多，已截断。'))
          }
        }
        rows.push(h('div', { className: 'dshgw-ssh-row', key: 'mkdir' }, [
          h('label', { key: 'label' }, '新建目录'),
          h('input', {
            key: 'input',
            type: 'text',
            value: current.newName,
            placeholder: '在当前列出的目录下新建',
            onChange: (event) => patch({ newName: event.target.value }),
            onKeyDown: (event) => {
              if (event.key === 'Enter') createDirectory().catch(() => {})
            },
          }),
          h('button', { key: 'create', type: 'button', disabled: current.busy !== '' || current.listing === null, onClick: () => createDirectory().catch(() => {}) }, '创建'),
        ]))

        const mounted = current.mounts ?? []
        const mountedSection = h('div', { className: 'dshgw-ssh-section', key: 'mounted' }, [
          h('h3', { key: 'title', style: { margin: '0 0 6px', fontSize: '13px' } }, `已挂载的远端目录（${mounted.length}）`),
          mounted.length === 0
            ? h('p', { className: 'dshgw-ssh-muted', key: 'empty' }, current.mirror ? '暂无。' : '网关还没有写下挂载记录。')
            : h('div', { className: 'dshgw-ssh-list', key: 'list' }, mounted.map((mount) => h('div', { className: 'dshgw-ssh-item', key: mount.path }, [
              h('span', { key: 'label' }, `${mount.host}:${mount.requested || mount.remote}`),
              h('span', { key: 'actions' }, [
                h('button', {
                  key: 'open',
                  type: 'button',
                  disabled: current.busy !== '',
                  onClick: () => {
                    patch({ busy: 'workspace', error: '' })
                    openWorkspace(mount.path, `${mount.host}:${mount.requested || mount.remote}`)
                      .then(() => patch({ busy: '' }))
                      .catch((error) => patch({ busy: '', error: textOf(error) }))
                  },
                }, '打开'),
                h('button', {
                  key: 'unmount',
                  type: 'button',
                  disabled: current.busy !== '',
                  onClick: () => unmount(mount.path).catch(() => {}),
                }, '卸载'),
              ]),
            ]))),
        ])

        const replies = current.replies ?? []
        const replySection = replies.length === 0 ? null : h('div', { className: 'dshgw-ssh-section', key: 'replies' },
          replies.map((reply) => h(reply.ok === true ? 'div' : 'div', {
            key: reply.id,
            className: reply.ok === true ? 'dshgw-ssh-notice' : 'dshgw-ssh-error',
          }, [
            h('span', { key: 'text' }, reply.ok === true
              ? `挂载完成：${reply.mountpoint}${reply.restarted === true ? '（该账号的 DSH 已重载）' : ''}`
              : `失败 ${reply.code || ''}：${reply.error || ''}`),
            h('button', { key: 'ack', type: 'button', style: { marginLeft: '8px' }, onClick: () => acknowledge(reply.id).catch(() => {}) }, '知道了'),
          ])))

        return h('div', {
          className: 'dshgw-ssh-backdrop',
          style: { pointerEvents: 'auto' },
          role: 'dialog',
          'aria-modal': 'true',
          'aria-label': 'SSH 工作区',
          onClick: (event) => {
            if (event.target === event.currentTarget) patch({ open: false })
          },
        }, h('div', { className: 'dshgw-ssh-dialog' }, [
          h('h2', { key: 'title' }, 'SSH 工作区'),
          h('p', { className: 'hint', key: 'hint' }, current.identity
            ? '用本账号的 ssh 身份浏览远端目录，并把选中的目录挂到本账号的工作区里。挂载由网关执行，完成后该账号的 DSH 会重载。'
            : '本账号还没有 ssh 身份（~/.ssh/id_rsa）：请让运维为这个账号配置密钥后再试。'),
          current.error !== '' ? h('div', { className: 'dshgw-ssh-error', key: 'error' }, current.error) : null,
          current.notice !== '' ? h('div', { className: 'dshgw-ssh-notice', key: 'notice' }, current.notice) : null,
          ...rows,
          mountedSection,
          replySection,
          h('div', { className: 'dshgw-ssh-footer', key: 'footer' }, [
            h('button', { key: 'close', type: 'button', onClick: () => patch({ open: false }) }, '关闭'),
            h('button', {
              key: 'open',
              type: 'button',
              className: 'primary',
              disabled: current.busy !== '' || current.host === '' || current.remote === '',
              onClick: () => requestMount().catch(() => {}),
            }, current.busy === 'open' ? '请求中…' : '挂载并打开'),
          ]),
        ]))
      }

      ctx.effect(() => ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({
        name: 'sidebar.footer.action',
        id: 'ssh-workspace',
        order: 100,
        label: 'SSH 工作区',
      }, SidebarAction)), 'ssh-workspace: sidebar entry')

      ctx.effect(() => ctx.slots.inject('shell.overlay', () => ctx.slots.register({
        name: 'shell.overlay',
        id: 'ssh-workspace-dialog',
        order: 200,
        label: 'SSH 工作区',
      }, Dialog)), 'ssh-workspace: dialog')

      // Stop polling when the page goes away; the dialog restarts it on demand.
      ctx.effect(() => () => {
        if (pollTimer !== null) window.clearInterval(pollTimer)
        pollTimer = null
      }, 'ssh-workspace: poll cleanup')

      ctx.logger?.info?.('ssh-workspace: client surface registered')
    }

    exports.apply = apply
    exports.inject = inject
    return module.exports
  },
})
