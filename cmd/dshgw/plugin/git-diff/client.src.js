// Browser half of dshgw-git-diff: a 「变更」 View in the session's main area — the changed files on
// the left, a two-column side-by-side diff on the right.
//
// Prebuilt by hand, like every out-of-tree surface in this deployment: the installation ships no
// bundler and dsh serves a client plugin's ./client file byte for byte, so build-client.mjs stamps
// this file into client.js. This half vendors nothing: React comes from the shell's frozen module
// table and every git fact arrives over one RPC call on the authenticated channel the host half
// registers.
//
// Why a View and not a floating panel: the shell's conversation area is the only seat that gives a
// side-by-side diff the width it needs, and `conversation.view` is the additive list slot that
// shipped already has two occupants in (对话, 轨迹) — a third id is added beside them, replacing
// nothing. The View mounts only while its tab is selected, so the poll loop is scoped to the tab.
//
// The list is deliberately two-tiered. The staged set is one index-level query and paints in about
// a second; the worktree set can only come from a scan, and on this sshfs-mounted AOSP checkout that
// scan is minutes of network `lstat`, so it runs in the background on the host and this half paints
// whatever has arrived chunk by chunk.

;(function () {
  window.__ModuleLoader__.load({
    id: 'dshgw-git-diff',
    factory: (require) => {
      // The wrapper every shipped bundle uses: the page's loader hands the factory a CommonJS
      // style `require`, and it must return `module.exports`.
      var module = { exports: {} }
      var exports = module.exports
      const React = require('react')
      const h = React.createElement
      const { useCallback, useEffect, useRef, useState } = React

      /** Required services: the slot registry and the authenticated browser RPC carrier. */
      const inject = ['slots', 'connection']

      const RPC_CHANNEL = '/dshgw-git-diff'
      const STORAGE_UI = 'dshgw-git-diff:ui'
      const STORAGE_VERSION = 1
      /** How often the tab polls a running scan. */
      const POLL_MS = 1000
      /** Rows rendered before the list and the diff say "narrow it down" instead of growing forever. */
      const MAX_LIST_ROWS = 1500
      const MAX_DIFF_ROWS = 4000
      const MIN_LEFT = 240
      const MAX_LEFT = 720
      const DEFAULT_LEFT = 360
      /** Loaded diffs kept per tab, keyed by repo/side/path: switching back is instant. */
      const DIFF_CACHE_LIMIT = 24

      const SIDE_LABEL = {
        unstaged: '未暂存 索引↔工作区',
        staged: '已暂存 HEAD↔索引',
        combined: '全部 HEAD↔工作区',
        untracked: '新增 未跟踪→工作区',
      }
      const SIDE_SHORT = { unstaged: '未暂存', staged: '已暂存', combined: '全部', untracked: '新增' }

      /** One flat stylesheet; every rule is scoped to this plugin's `dshgw-gd-` prefix. */
      const CSS = `
.dshgw-gd-root { flex: 1 1 auto; min-height: 0; height: 100%; display: flex; flex-direction: column;
  color: var(--dsw-alias-label-primary, #e6e6e6); font-size: 12px; }
.dshgw-gd-head { display: flex; align-items: center; gap: 6px; padding: 6px 10px; flex: none; flex-wrap: wrap;
  border-bottom: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.24)); }
.dshgw-gd-title { font-weight: 600; opacity: .9; margin-right: 2px; white-space: nowrap; }
.dshgw-gd-select { background: var(--dsw-alias-bg-base, rgba(0,0,0,.22)); color: inherit; font: inherit;
  border: 1px solid var(--dsw-alias-border-l3, rgba(127,127,127,.35)); border-radius: 5px; padding: 3px 6px; max-width: 260px; }
.dshgw-gd-input { background: var(--dsw-alias-bg-base, rgba(0,0,0,.22)); color: inherit; font: inherit;
  border: 1px solid var(--dsw-alias-border-l3, rgba(127,127,127,.35)); border-radius: 5px; padding: 3px 7px; outline: none; }
.dshgw-gd-input:focus { border-color: var(--dsw-alias-brand-primary, #4a86f7); }
.dshgw-gd-filter { width: 170px; }
.dshgw-gd-scope { width: 190px; }
.dshgw-gd-btn { display: inline-flex; align-items: center; gap: 4px; height: 24px; padding: 0 9px; flex: none;
  background: none; border: 1px solid var(--dsw-alias-border-l3, rgba(127,127,127,.35)); border-radius: 5px;
  color: inherit; font: inherit; cursor: pointer; white-space: nowrap; opacity: .9; }
.dshgw-gd-btn:hover { background: var(--dsw-alias-interactive-bg-hover, rgba(127,127,127,.22)); opacity: 1; }
.dshgw-gd-btn[disabled] { opacity: .4; cursor: default; }
.dshgw-gd-btn[data-primary="true"] { background: var(--dsw-alias-brand-primary, #4a86f7); border-color: transparent; color: #fff; }
.dshgw-gd-btn[data-primary="true"]:hover { filter: brightness(1.08); }
.dshgw-gd-btn[data-active="true"] { background: var(--dsw-alias-interactive-bg-hover, rgba(127,127,127,.28)); opacity: 1; }
.dshgw-gd-spacer { flex: 1; min-width: 0; }
.dshgw-gd-meta { font-size: 11px; opacity: .65; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 40%; }
.dshgw-gd-body { flex: 1; min-height: 0; display: flex; align-items: stretch; }
.dshgw-gd-list { flex: none; min-width: 0; display: flex; flex-direction: column; min-height: 0;
  border-right: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.24)); }
.dshgw-gd-chips { display: flex; align-items: center; gap: 4px; padding: 5px 8px; flex: none; flex-wrap: wrap;
  border-bottom: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.18)); }
.dshgw-gd-chip { border: 1px solid transparent; background: rgba(127,127,127,.14); color: inherit; font: inherit;
  font-size: 11px; padding: 2px 7px; border-radius: 10px; cursor: pointer; opacity: .55; white-space: nowrap; }
.dshgw-gd-chip[data-on="true"] { opacity: 1; border-color: var(--dsw-alias-border-l3, rgba(127,127,127,.4)); }
.dshgw-gd-chip[disabled] { cursor: default; opacity: .3; }
.dshgw-gd-rows { flex: 1; min-height: 0; overflow: auto; padding: 3px 0; }
.dshgw-gd-row { display: flex; align-items: center; gap: 6px; padding: 2px 8px; cursor: pointer; border-radius: 0; }
.dshgw-gd-row:hover { background: rgba(127,127,127,.14); }
.dshgw-gd-row[data-selected="true"] { background: var(--dsw-alias-interactive-bg-hover, rgba(127,127,127,.26)); }
.dshgw-gd-badge { flex: none; width: 18px; text-align: center; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 10px; font-weight: 600; border-radius: 4px; padding: 1px 0; background: rgba(127,127,127,.2); }
.dshgw-gd-badge[data-kind="M"] { background: rgba(230,180,80,.28); color: var(--dsw-alias-state-warning-primary, #e0b050); }
.dshgw-gd-badge[data-kind="A"] { background: rgba(106,168,79,.28); color: var(--dsw-alias-state-success-primary, #78b45c); }
.dshgw-gd-badge[data-kind="D"] { background: rgba(212,105,92,.28); color: var(--dsw-alias-state-error-primary, #e0705f); }
.dshgw-gd-badge[data-kind="?"] { background: rgba(97,175,239,.26); color: #6fa8dc; }
.dshgw-gd-path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; direction: rtl; text-align: left; }
.dshgw-gd-dir { opacity: .55; }
.dshgw-gd-name { opacity: .95; }
/* Directory-first: RTL truncation keeps a long path's tail visible, so each half is a bidi isolate. */
.dshgw-gd-path .dshgw-gd-dir, .dshgw-gd-path .dshgw-gd-name { unicode-bidi: plaintext; }
.dshgw-gd-tags { flex: none; display: flex; gap: 3px; font-size: 10px; opacity: .8; }
.dshgw-gd-tag { padding: 1px 5px; border-radius: 8px; background: rgba(127,127,127,.18); white-space: nowrap; }
.dshgw-gd-rowstat { flex: none; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 10px; opacity: .8; }
.dshgw-gd-add { color: var(--dsw-alias-state-success-primary, #78b45c); }
.dshgw-gd-del { color: var(--dsw-alias-state-error-primary, #e0705f); }
.dshgw-gd-empty { padding: 24px 14px; text-align: center; opacity: .6; line-height: 1.7; }
.dshgw-gd-empty code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; opacity: .9; }
.dshgw-gd-status { flex: none; display: flex; align-items: center; gap: 6px; padding: 4px 8px; font-size: 11px;
  border-top: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.24)); opacity: .85; flex-wrap: wrap; }
.dshgw-gd-status-grow { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; opacity: .7; }
.dshgw-gd-error { color: var(--dsw-alias-state-error-primary, #e0705f); }
.dshgw-gd-ok { color: var(--dsw-alias-state-success-primary, #78b45c); }
.dshgw-gd-warn { color: var(--dsw-alias-state-warning-primary, #e0b050); }
.dshgw-gd-track { width: 84px; height: 5px; border-radius: 3px; background: rgba(127,127,127,.28); overflow: hidden; flex: none; }
.dshgw-gd-fill { height: 100%; background: var(--dsw-alias-brand-primary, #4a86f7); transition: width .2s ease; }
.dshgw-gd-grip { flex: none; width: 5px; cursor: ew-resize; background: transparent; }
.dshgw-gd-grip:hover { background: var(--dsw-alias-border-l3, rgba(127,127,127,.3)); }
.dshgw-gd-diff { flex: 1; min-width: 0; display: flex; flex-direction: column; min-height: 0; }
.dshgw-gd-dhead { flex: none; display: flex; align-items: center; gap: 6px; padding: 5px 10px; flex-wrap: wrap;
  border-bottom: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.24)); }
.dshgw-gd-dpath { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 11.5px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 46%; opacity: .95; }
.dshgw-gd-dsides { display: flex; gap: 3px; }
.dshgw-gd-tab { border: 1px solid transparent; background: rgba(127,127,127,.14); color: inherit; font: inherit;
  font-size: 11px; padding: 2px 8px; border-radius: 5px; cursor: pointer; opacity: .7; white-space: nowrap; }
.dshgw-gd-tab:hover { opacity: 1; }
.dshgw-gd-tab[data-active="true"] { opacity: 1; background: var(--dsw-alias-interactive-bg-hover, rgba(127,127,127,.3));
  border-color: var(--dsw-alias-border-l3, rgba(127,127,127,.4)); }
.dshgw-gd-dbody { flex: 1; min-height: 0; overflow: auto; background: var(--dsw-alias-bg-base, rgba(0,0,0,.18)); }
.dshgw-gd-rowsplit { display: grid; grid-template-columns: 1fr 1fr; align-items: stretch; }
.dshgw-gd-rowsplit > .dshgw-gd-cell:first-child { border-right: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.3)); }
.dshgw-gd-cell { display: flex; align-items: flex-start; min-width: 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px; line-height: 1.5; content-visibility: auto; contain-intrinsic-size: auto 18px; }
.dshgw-gd-cell[data-type="del"] { background: rgba(212,105,92,.14); }
.dshgw-gd-cell[data-type="add"] { background: rgba(106,168,79,.14); }
.dshgw-gd-cell[data-type="ctx"] { background: transparent; }
.dshgw-gd-cell[data-empty="true"] { background: rgba(127,127,127,.07); }
.dshgw-gd-cell[data-para="odd"] { background-color: rgba(127,127,127,.045); }
.dshgw-gd-cell[data-para="odd"][data-type="del"] { background-color: rgba(212,105,92,.18); }
.dshgw-gd-cell[data-para="odd"][data-type="add"] { background-color: rgba(106,168,79,.18); }
.dshgw-gd-ln { flex: none; width: 52px; text-align: right; padding: 0 7px 0 4px; opacity: .4; user-select: none;
  font-size: 11px; white-space: nowrap; }
.dshgw-gd-code { flex: 1; min-width: 0; white-space: pre; padding-right: 10px; }
.dshgw-gd-wrap .dshgw-gd-code { white-space: pre-wrap; word-break: break-all; }
.dshgw-gd-mark { background: rgba(240,200,80,.3); border-radius: 2px; color: inherit; }
.dshgw-gd-hunk { grid-column: 1 / -1; padding: 3px 10px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px; opacity: .75; background: var(--dsw-alias-bg-layer-2, rgba(127,127,127,.12));
  border-top: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.2)); border-bottom: 1px solid var(--dsw-alias-border-l2, rgba(127,127,127,.2)); }
.dshgw-gd-note { grid-column: 1 / -1; padding: 2px 10px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px; opacity: .55; }
.dshgw-gd-unified .dshgw-gd-line { display: flex; align-items: flex-start; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px; line-height: 1.5; }
.dshgw-gd-unified .dshgw-gd-line[data-type="del"] { background: rgba(212,105,92,.14); }
.dshgw-gd-unified .dshgw-gd-line[data-type="add"] { background: rgba(106,168,79,.14); }
.dshgw-gd-unified .dshgw-gd-ln { width: 46px; }
.dshgw-gd-more { padding: 8px 12px; opacity: .7; font-size: 11px; }
`

      /** Install the one stylesheet this plugin owns. */
      function installCSS() {
        if (document.querySelector('style[data-plugin-css="dshgw-git-diff/client.css"]') !== null) return
        const tag = document.createElement('style')
        tag.dataset.pluginCss = 'dshgw-git-diff/client.css'
        tag.textContent = CSS
        document.head.appendChild(tag)
      }

      // ---- pure helpers (exported at the bottom for the tests) -------------------------------

      /** Strip a diff's `a/` or `b/` prefix and its trailing timestamp, if any. */
      function stripDiffPrefix(header) {
        let text = header.trim()
        const tab = text.indexOf('\t')
        if (tab !== -1) text = text.slice(0, tab)
        if (text === '/dev/null') return text
        if (text.startsWith('a/') || text.startsWith('b/')) return text.slice(2)
        return text
      }

      /**
       * Turn one unified diff into hunks of row records.
       *
       * This is the whole rendering contract: the host ships git's own text and this function is the
       * only interpretation of it, so what the panel shows is what git said — including the awkward
       * cases (`\ No newline at end of file`, a new or deleted file, a mode-only change, and a
       * binary file, which git refuses to diff at all).
       */
      function parseUnifiedDiff(text) {
        const result = {
          hunks: [], binary: false, newFile: false, deletedFile: false, modeOnly: false,
          oldPath: null, newPath: null, notes: [],
        }
        if (typeof text !== 'string' || text === '') return result
        const lines = text.split('\n')
        if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop()
        let current = null
        let oldNo = 0
        let newNo = 0
        let sawMode = false
        let sawContent = false
        for (const line of lines) {
          if (line.startsWith('diff --git ')) { current = null; continue }
          if (line.startsWith('--- ')) { result.oldPath = stripDiffPrefix(line.slice(4)); continue }
          if (line.startsWith('+++ ')) { result.newPath = stripDiffPrefix(line.slice(4)); continue }
          if (line.startsWith('new file mode')) { result.newFile = true; sawMode = true; continue }
          if (line.startsWith('deleted file mode')) { result.deletedFile = true; sawMode = true; continue }
          if (line.startsWith('old mode') || line.startsWith('new mode')) { sawMode = true; continue }
          if (line.startsWith('Binary files ') || line.startsWith('GIT binary patch')) { result.binary = true; continue }
          if (line.startsWith('index ') || line.startsWith('similarity index') || line.startsWith('dissimilarity index')) continue
          if (line.startsWith('rename from') || line.startsWith('rename to') || line.startsWith('copy from') || line.startsWith('copy to')) continue
          if (line.startsWith('@@')) {
            const match = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(line)
            if (match === null) continue
            current = { header: line, oldStart: Number(match[1]), newStart: Number(match[3]), rows: [] }
            result.hunks.push(current)
            oldNo = Number(match[1])
            newNo = Number(match[3])
            continue
          }
          if (current === null) continue
          if (line.startsWith('\\')) {
            current.rows.push({ kind: 'note', text: line.slice(1).trim() })
            continue
          }
          if (line.startsWith('+')) {
            current.rows.push({ kind: 'add', oldNo: null, newNo, text: line.slice(1) })
            newNo += 1
            sawContent = true
            continue
          }
          if (line.startsWith('-')) {
            current.rows.push({ kind: 'del', oldNo, newNo: null, text: line.slice(1) })
            oldNo += 1
            sawContent = true
            continue
          }
          // A context line, including the empty one that renders as a single space.
          current.rows.push({ kind: 'ctx', oldNo, newNo, text: line.slice(1) })
          oldNo += 1
          newNo += 1
        }
        if (sawMode && !sawContent && result.hunks.length === 0 && !result.binary) result.modeOnly = true
        return result
      }

      /** The character ranges that differ between two paired lines, after trimming both ends. */
      function inlineSpans(leftText, rightText) {
        if (typeof leftText !== 'string' || typeof rightText !== 'string') return { left: [], right: [] }
        const limit = Math.min(leftText.length, rightText.length)
        let start = 0
        while (start < limit && leftText[start] === rightText[start]) start += 1
        let tail = 0
        while (tail < limit - start && leftText[leftText.length - 1 - tail] === rightText[rightText.length - 1 - tail]) tail += 1
        const leftEnd = leftText.length - tail
        const rightEnd = rightText.length - tail
        return {
          left: leftEnd > start ? [[start, leftEnd]] : [],
          right: rightEnd > start ? [[start, rightEnd]] : [],
        }
      }

      /**
       * Pair a hunk's removed and added lines into two-column rows.
       *
       * git emits each change block as its removed lines followed by its added lines, so the two
       * runs are zipped positionally and the longer run leaves empty filler cells on the other side
       * — which is what makes a rewrite (3 lines replaced by 1) read as a rewrite instead of as two
       * unrelated blocks.
       */
      function pairRows(hunk) {
        const rows = []
        const body = hunk.rows ?? []
        let index = 0
        while (index < body.length) {
          const row = body[index]
          if (row.kind === 'note') {
            rows.push({ kind: 'note', text: row.text })
            index += 1
            continue
          }
          if (row.kind === 'ctx') {
            rows.push({
              kind: 'pair',
              left: { no: row.oldNo, text: row.text, type: 'ctx' },
              right: { no: row.newNo, text: row.text, type: 'ctx' },
              spans: { left: [], right: [] },
            })
            index += 1
            continue
          }
          const removals = []
          const additions = []
          while (index < body.length && (body[index].kind === 'del' || body[index].kind === 'add')) {
            if (body[index].kind === 'del') removals.push(body[index])
            else additions.push(body[index])
            index += 1
          }
          const count = Math.max(removals.length, additions.length)
          for (let offset = 0; offset < count; offset += 1) {
            const left = removals[offset] ?? null
            const right = additions[offset] ?? null
            rows.push({
              kind: 'pair',
              left: left === null ? null : { no: left.oldNo, text: left.text, type: 'del' },
              right: right === null ? null : { no: right.newNo, text: right.text, type: 'add' },
              spans: left !== null && right !== null ? inlineSpans(left.text, right.text) : { left: [], right: [] },
            })
          }
        }
        return rows
      }

      /** Every logical row of a parsed diff, in order, tagged with its hunk. */
      function diffRows(parsed) {
        const rows = []
        for (const hunk of parsed.hunks) {
          rows.push({ kind: 'hunk', text: hunk.header })
          for (const row of pairRows(hunk)) rows.push(row)
        }
        return rows
      }

      /**
       * One row per changed path, with the sides it participates in.
       *
       * A path staged and then modified again is one row carrying both facts — the diff header
       * offers both comparisons — because two rows for one file is how a list stops matching the
       * worktree it describes.
       */
      function buildRows(staged, scanned) {
        const map = new Map()
        const entryOf = (path) => {
          let entry = map.get(path)
          if (entry === undefined) {
            entry = { path, staged: null, unstaged: null, untracked: false }
            map.set(path, entry)
          }
          return entry
        }
        for (const record of staged ?? []) entryOf(record.path).staged = record.status
        for (const record of scanned ?? []) {
          const entry = entryOf(record.path)
          if (record.side === 'untracked') entry.untracked = true
          else entry.unstaged = record.status
        }
        return [...map.values()]
      }

      /** The comparisons a row can offer, most specific first. */
      function sidesOf(row) {
        if (row === null) return []
        if (row.untracked && row.unstaged === null && row.staged === null) return ['untracked']
        const sides = []
        if (row.unstaged !== null) sides.push('unstaged')
        if (row.staged !== null) sides.push('staged')
        if (row.unstaged !== null && row.staged !== null) sides.push('combined')
        if (row.untracked) sides.push('untracked')
        return sides
      }

      /** The badge letter a row shows: the worktree state wins, then the staged one. */
      function badgeOf(row) {
        if (row.unstaged !== null) return row.unstaged
        if (row.staged !== null) return row.staged
        if (row.untracked) return '?'
        return '·'
      }

      /** Split one path into the directory part and the file name, for two-tone rendering. */
      function splitPath(path) {
        const cut = path.lastIndexOf('/')
        if (cut === -1) return { dir: '', name: path }
        return { dir: path.slice(0, cut + 1), name: path.slice(cut + 1) }
      }

      /** Human duration for a scan's elapsed time. */
      function formatDuration(ms) {
        if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '—'
        if (ms < 1000) return `${Math.round(ms)}ms`
        const seconds = ms / 1000
        if (seconds < 60) return `${seconds < 10 ? seconds.toFixed(1) : Math.round(seconds)}s`
        const minutes = Math.floor(seconds / 60)
        return `${minutes}m${String(Math.round(seconds - minutes * 60)).padStart(2, '0')}s`
      }

      /** Clock time for a timestamp, or an empty string. */
      function formatClock(ms) {
        if (typeof ms !== 'number' || !Number.isFinite(ms)) return ''
        const date = new Date(ms)
        const pad = (value) => String(value).padStart(2, '0')
        return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
      }

      // ---- persistence -----------------------------------------------------------------------

      /**
       * The remembered View preferences.
       *
       * `remembered` says whether this browser has a stored choice at all: the host's configured
       * untracked default is only adopted when the user has never touched the 未跟踪 chip, so an
       * explicit local choice is never overridden by configuration.
       */
      function loadUI() {
        const fallback = {
          repo: null, leftWidth: DEFAULT_LEFT, mode: 'split', wrap: false,
          showStaged: true, showUnstaged: true, showUntracked: false, remembered: false,
        }
        try {
          const raw = window.localStorage.getItem(STORAGE_UI)
          if (raw === null) return fallback
          const parsed = JSON.parse(raw)
          if (parsed === null || typeof parsed !== 'object' || parsed.version !== STORAGE_VERSION) return fallback
          const number = typeof parsed.leftWidth === 'number' && Number.isFinite(parsed.leftWidth) ? parsed.leftWidth : DEFAULT_LEFT
          return {
            repo: typeof parsed.repo === 'string' ? parsed.repo : null,
            leftWidth: Math.min(MAX_LEFT, Math.max(MIN_LEFT, number)),
            mode: parsed.mode === 'unified' ? 'unified' : 'split',
            wrap: parsed.wrap === true,
            showStaged: parsed.showStaged !== false,
            showUnstaged: parsed.showUnstaged !== false,
            showUntracked: parsed.showUntracked === true,
            remembered: true,
          }
        } catch {
          return fallback
        }
      }

      function saveUI(ui) {
        try {
          window.localStorage.setItem(STORAGE_UI, JSON.stringify({ version: STORAGE_VERSION, ...ui }))
        } catch {
          // A blocked localStorage only costs the remembered layout.
        }
      }

      // ---- the plugin body --------------------------------------------------------------------

      function apply(ctx) {
        installCSS()

        /**
         * One RPC call, unwrapped. A host failure arrives as `{ok:false,error}` and is thrown as an
         * Error carrying the host's code, so callers branch on codes instead of parsing messages.
         *
         * `payload ?? {}` is load-bearing, not defensive noise. The shell builds the request body as
         * `JSON.stringify({ type: 'client-request', rpcId, method, payload })` and the host validates
         * that envelope with zod 4, where the schema's `payload: z.unknown()` is **not optional** —
         * a body without the key fails with `invalid_type: expected nonoptional, received undefined`
         * and the host answers "invalid client-request message" without ever reaching the endpoint.
         * `JSON.stringify` drops keys whose value is `undefined`, so the argument-less handshake
         * `call('hello')` used to be rejected exactly that way, and the 变更 tab could only show the
         * error plus its empty state ("在工作区里没有找到 git 仓库"). Defaulting here, at the one place
         * every endpoint goes through, is what keeps the next argument-less endpoint from repeating it.
         */
        const call = async (endpoint, payload) => {
          const result = await ctx.connection.rpc.call(RPC_CHANNEL, endpoint, payload ?? {})
          if (result === null || typeof result !== 'object' || result.ok !== true) {
            const error = (result !== null && typeof result === 'object' && result.error) || null
            const failure = new Error(error === null ? 'git 服务没有响应' : String(error.message))
            failure.code = error === null ? 'transport' : String(error.code)
            failure.details = error?.details ?? {}
            throw failure
          }
          return result.value
        }

        // ---- shared view state -----------------------------------------------------------
        const EMPTY_SCAN = {
          jobId: null, state: 'idle', plan: [], chunksDone: 0, chunksTotal: 0,
          files: [], total: 0, truncated: false, elapsedMs: null, completedAt: null, error: null,
        }
        let state = {
          ui: loadUI(),
          hello: null,
          repos: [],
          repo: null,
          meta: null,
          staged: [],
          scan: EMPTY_SCAN,
          rows: [],
          selected: null,
          side: null,
          diff: null,
          diffBusy: false,
          busy: false,
          scope: '',
          filter: '',
          error: '',
          fatal: '',
          notice: '',
        }
        const listeners = new Set()
        const diffs = new Map()
        let pollTimer = null
        let mounted = 0
        let booted = false

        const notify = () => {
          for (const listener of [...listeners]) {
            try {
              listener(state)
            } catch (error) {
              ctx.logger?.warn?.(`git-diff: listener failed: ${error?.message ?? error}`)
            }
          }
        }
        const setState = (patch) => { state = { ...state, ...patch }; notify() }
        const setUI = (patch) => {
          const ui = { ...state.ui, ...patch }
          setState({ ui })
          saveUI(ui)
        }
        const failWith = (error) => setState({ error: `${error?.message ?? error}` })

        // ---- data loading ----------------------------------------------------------------

        /** Load the change sets of one repository: the fast staged set plus whatever the cache holds. */
        const loadRepo = async (repoPath, { resetSelection = true } = {}) => {
          setState({ busy: true, error: '', ...(resetSelection ? { selected: null, side: null, diff: null } : {}) })
          try {
            const status = await call('status', { repo: repoPath })
            const rows = buildRows(status.staged, status.scan.files)
            setState({
              busy: false, repo: repoPath, meta: status.meta, staged: status.staged,
              scan: { ...EMPTY_SCAN, ...status.scan }, rows,
            })
            setUI({ repo: repoPath })
          } catch (error) {
            setState({ busy: false })
            failWith(error)
          }
        }

        const loadRepos = async ({ refresh = false } = {}) => {
          const result = await call('repos', { refresh })
          const repos = Array.isArray(result?.repos) ? result.repos : []
          const remembered = state.ui.repo
          const pinned = state.hello?.pinnedRepo?.path ?? state.hello?.repo?.path ?? null
          const preferred = repos.find((repo) => repo.path === remembered)
            ?? repos.find((repo) => repo.path === pinned)
            ?? repos[0]
            ?? null
          setState({ repos })
          if (preferred !== null) await loadRepo(preferred.path)
          else setState({ repo: null, rows: [], meta: null })
          return preferred
        }

        /** First paint: who we are, where the repositories are, and what is already known. */
        const bootstrap = async () => {
          if (booted) return
          booted = true
          try {
            const hello = await call('hello')
            setState({ hello })
            if (hello.git?.available !== true) {
              setState({ fatal: `宿主机的 git 不可用（${hello.git?.version ?? '未检测到'}）` })
              return
            }
            if (state.ui.remembered !== true && hello.untracked === true && state.ui.showUntracked !== true) {
              setUI({ showUntracked: true, remembered: true })
            }
            const preferred = await loadRepos({})
            if (preferred === null) return
            if (state.hello?.autoScan !== false && state.scan.state === 'idle') void startScan({ includeUntracked: state.ui.showUntracked })
          } catch (error) {
            failWith(error)
          } finally {
            booted = false
          }
        }

        /** Start (or join) the host's background scan. */
        const startScan = async ({ scopes = null, includeUntracked = false, force = false } = {}) => {
          if (state.repo === null) return
          setState({ error: '', notice: '' })
          try {
            const job = await call('scanStart', { repo: state.repo, scopes, includeUntracked, force })
            setState({ scan: { ...EMPTY_SCAN, ...job } })
            ensurePolling()
          } catch (error) {
            failWith(error)
          }
        }

        const cancelScan = async () => {
          try {
            const snapshot = await call('scanCancel', { jobId: state.scan.jobId })
            setState({ scan: { ...state.scan, ...snapshot } })
            stopPolling()
          } catch (error) {
            failWith(error)
          }
        }

        /** One poll of the running scan; the loop stops itself when the job settles. */
        const pollScan = async () => {
          try {
            const snapshot = await call('scanStatus', { jobId: state.scan.jobId })
            const scan = { ...EMPTY_SCAN, ...snapshot }
            setState({ scan, rows: buildRows(state.staged, scan.files) })
            if (scan.state !== 'running') {
              stopPolling()
              if (scan.state === 'done' && state.repo !== null) await loadRepo(state.repo, { resetSelection: false })
            }
          } catch (error) {
            stopPolling()
            failWith(error)
          }
        }

        const ensurePolling = () => {
          if (pollTimer !== null) return
          pollTimer = setInterval(() => { void pollScan() }, POLL_MS)
        }
        const stopPolling = () => {
          if (pollTimer === null) return
          clearInterval(pollTimer)
          pollTimer = null
        }

        /** Load one file's diff for one comparison, from the cache when it is already there. */
        const loadDiff = async (row, side) => {
          if (row === null) return
          const target = side ?? sidesOf(row)[0] ?? 'unstaged'
          const key = `${state.repo}\0${target}\0${row.path}`
          setState({ selected: row.path, side: target, diffBusy: true, error: '' })
          const cached = diffs.get(key)
          if (cached !== undefined) {
            setState({ diff: cached, diffBusy: false })
            return
          }
          try {
            const payload = await call('diff', { repo: state.repo, path: row.path, side: target })
            const enriched = { ...payload, parsed: parseUnifiedDiff(payload.unified), rows: null }
            enriched.rows = diffRows(enriched.parsed)
            if (diffs.size >= DIFF_CACHE_LIMIT) diffs.delete(diffs.keys().next().value)
            diffs.set(key, enriched)
            if (state.selected === row.path && state.side === target) setState({ diff: enriched, diffBusy: false })
            else setState({ diffBusy: false })
          } catch (error) {
            if (state.selected === row.path && state.side === target) setState({ diff: null, diffBusy: false })
            failWith(error)
          }
        }

        // ---- components ------------------------------------------------------------------

        /** The two-column (or unified) body of one loaded diff. */
        function DiffBody({ diff, wrap }) {
          if (diff === null) {
            return h('div', { className: 'dshgw-gd-empty' }, '从左侧选择一个文件查看修改对比。')
          }
          const parsed = diff.parsed
          if (diff.binary === true || parsed.binary === true) {
            return h('div', { className: 'dshgw-gd-empty' }, [
              h('div', { key: 'a' }, '二进制文件，无法显示文本对比。'),
              h('div', { key: 'b' }, `${diff.oldLabel} → ${diff.newLabel}`),
            ])
          }
          if (parsed.hunks.length === 0) {
            return h('div', { className: 'dshgw-gd-empty' }, [
              h('div', { key: 'a' }, parsed.modeOnly === true ? '只有文件模式或权限发生变化，没有文本差异。' : '这个对比没有文本差异。'),
              h('div', { key: 'b' }, diff.newFile === true ? '（新增文件）' : diff.deletedFile === true ? '（文件已删除）' : ''),
            ])
          }
          const rows = diff.rows ?? diffRows(parsed)
          const shown = rows.slice(0, MAX_DIFF_ROWS)
          const body = []
          for (let index = 0; index < shown.length; index += 1) {
            const row = shown[index]
            if (row.kind === 'hunk') {
              body.push(h('div', { key: `h${index}`, className: 'dshgw-gd-hunk' }, row.text))
              continue
            }
            if (row.kind === 'note') {
              body.push(h('div', { key: `n${index}`, className: 'dshgw-gd-note' }, row.text))
              continue
            }
            const cell = (side, which) => {
              const value = row[side]
              if (value === null || value === undefined) {
                return h('div', { key: which, className: 'dshgw-gd-cell', 'data-empty': 'true' },
                  h('span', { className: 'dshgw-gd-ln' }, ''), h('span', { className: 'dshgw-gd-code' }, ''))
              }
              const spans = row.spans?.[side] ?? []
              return h('div', {
                key: which, className: 'dshgw-gd-cell', 'data-type': value.type,
              }, [
                h('span', { key: 'ln', className: 'dshgw-gd-ln' }, value.no === null || value.no === undefined ? '' : String(value.no)),
                h('span', { key: 'code', className: 'dshgw-gd-code' }, textWithMarks(value.text, spans, `${index}-${which}`)),
              ])
            }
            body.push(h('div', { key: `r${index}`, className: 'dshgw-gd-rowsplit' }, [
              cell('left', 'l'),
              cell('right', 'r'),
            ]))
          }
          if (rows.length > shown.length) {
            body.push(h('div', { key: 'more', className: 'dshgw-gd-more' },
              `差异过大，只渲染前 ${MAX_DIFF_ROWS} 行（共 ${rows.length} 行）。`))
          }
          return h('div', { className: wrap ? 'dshgw-gd-wrap' : undefined }, body)
        }

        /** One unified-diff column, for the 统一 mode. */
        function UnifiedBody({ diff, wrap }) {
          const parsed = diff?.parsed
          if (parsed === undefined || parsed === null) return h('div', { className: 'dshgw-gd-empty' }, '没有内容。')
          const body = []
          let index = 0
          for (const hunk of parsed.hunks) {
            body.push(h('div', { key: `h${index}`, className: 'dshgw-gd-hunk' }, hunk.header))
            index += 1
            for (const row of hunk.rows) {
              if (row.kind === 'note') {
                body.push(h('div', { key: `n${index}`, className: 'dshgw-gd-note' }, row.text))
                index += 1
                continue
              }
              const number = row.kind === 'add' ? row.newNo : row.oldNo
              body.push(h('div', {
                key: `l${index}`, className: 'dshgw-gd-line', 'data-type': row.kind,
              }, [
                h('span', { key: 'ln', className: 'dshgw-gd-ln' }, number === null || number === undefined ? '' : String(number)),
                h('span', { key: 'code', className: 'dshgw-gd-code' }, `${row.kind === 'add' ? '+' : row.kind === 'del' ? '-' : ' '}${row.text}`),
              ]))
              index += 1
            }
          }
          return h('div', { className: `${wrap ? 'dshgw-gd-wrap ' : ''}dshgw-gd-unified` }, body)
        }

        /** The whole tab. */
        function GitDiffView() {
          const [snapshot, setSnapshot] = useState(state)
          const dragRef = useRef(null)

          useEffect(() => {
            const listener = (next) => setSnapshot(next)
            listeners.add(listener)
            mounted += 1
            void bootstrap()
            if (state.scan.state === 'running') ensurePolling()
            return () => {
              listeners.delete(listener)
              mounted = Math.max(0, mounted - 1)
              if (mounted === 0) stopPolling()
            }
          }, [])

          // Keyboard: up/down move through the filtered list, Enter loads that file's diff.
          const onKeyDown = useCallback((event) => {
            if (snapshot.rows.length === 0) return
            if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp' && event.key !== 'Enter') return
            const visible = visibleRowsOf(snapshot)
            if (visible.length === 0) return
            event.preventDefault()
            const current = visible.findIndex((row) => row.path === snapshot.selected)
            if (event.key === 'Enter') {
              if (current >= 0) void loadDiff(visible[current], snapshot.side)
              return
            }
            const step = event.key === 'ArrowDown' ? 1 : -1
            const next = visible[Math.min(visible.length - 1, Math.max(0, (current < 0 ? (step > 0 ? -1 : 0) : current) + step))]
            void loadDiff(next, null)
          }, [snapshot])

          const beginDrag = (event) => {
            event.preventDefault()
            dragRef.current = { startX: event.clientX, startWidth: snapshot.ui.leftWidth }
            const move = (moveEvent) => {
              const drag = dragRef.current
              if (drag === null) return
              const next = Math.min(MAX_LEFT, Math.max(MIN_LEFT, drag.startWidth + (moveEvent.clientX - drag.startX)))
              setUI({ leftWidth: next })
            }
            const up = () => {
              dragRef.current = null
              window.removeEventListener('pointermove', move)
              window.removeEventListener('pointerup', up)
            }
            window.addEventListener('pointermove', move)
            window.addEventListener('pointerup', up)
          }

          if (snapshot.fatal !== '') {
            return h('div', { className: 'dshgw-gd-root' }, h('div', { className: 'dshgw-gd-empty' }, snapshot.fatal))
          }

          const visible = visibleRowsOf(snapshot)
          const counts = countsOf(snapshot.rows)
          const scan = snapshot.scan
          const running = scan.state === 'running'
          const truncatedList = visible.length > MAX_LIST_ROWS
          const listed = visible.slice(0, MAX_LIST_ROWS)
          const rows = listed.map((row) => {
            const parts = splitPath(row.path)
            const selected = snapshot.selected === row.path
            return h('div', {
              key: row.path,
              className: 'dshgw-gd-row',
              'data-selected': selected ? 'true' : 'false',
              title: row.path,
              role: 'button',
              tabIndex: 0,
              onClick: () => { void loadDiff(row, null) },
            }, [
              h('span', { key: 'b', className: 'dshgw-gd-badge', 'data-kind': badgeOf(row) }, badgeOf(row)),
              h('span', { key: 'p', className: 'dshgw-gd-path' }, [
                parts.dir === '' ? null : h('span', { key: 'd', className: 'dshgw-gd-dir' }, parts.dir),
                h('span', { key: 'n', className: 'dshgw-gd-name' }, parts.name),
              ]),
              h('span', { key: 't', className: 'dshgw-gd-tags' }, [
                row.staged !== null ? h('span', { key: 's', className: 'dshgw-gd-tag' }, '已暂存') : null,
                row.untracked ? h('span', { key: 'u', className: 'dshgw-gd-tag' }, '未跟踪') : null,
              ]),
            ])
          })

          const head = h('div', { className: 'dshgw-gd-head' }, [
            h('span', { key: 't', className: 'dshgw-gd-title' }, '变更'),
            snapshot.repos.length === 0
              ? h('span', { key: 'norepo', className: 'dshgw-gd-meta' }, '未发现 git 仓库')
              : h('select', {
                  key: 'repo',
                  className: 'dshgw-gd-select',
                  value: snapshot.repo ?? '',
                  'aria-label': '选择仓库',
                  onChange: (event) => { void loadRepo(event.target.value) },
                }, snapshot.repos.map((repo) => h('option', { key: repo.path, value: repo.path },
                    `${repo.rel}${repo.writable === false ? '（只读）' : ''}`))),
            h('button', {
              key: 'scan', type: 'button', className: 'dshgw-gd-btn', 'data-primary': 'true',
              disabled: running || snapshot.repo === null, title: '扫描工作区改动（分块进行，可随时取消）',
              onClick: () => {
                const scope = String(snapshot.scope ?? '').trim()
                void startScan({ scopes: scope === '' ? null : [scope], includeUntracked: snapshot.ui.showUntracked })
              },
            }, running ? '扫描中…' : '扫描'),
            h('button', {
              key: 'cancel', type: 'button', className: 'dshgw-gd-btn', disabled: !running,
              title: '停止后台扫描（已完成的部分保留）',
              onClick: () => { void cancelScan() },
            }, '取消'),
            h('button', {
              key: 'refresh', type: 'button', className: 'dshgw-gd-btn', disabled: snapshot.repo === null,
              title: '重新读取索引级变更（很快）',
              onClick: () => { if (snapshot.repo !== null) void loadRepo(snapshot.repo, { resetSelection: false }) },
            }, '刷新'),
            h('input', {
              key: 'scope', className: 'dshgw-gd-input dshgw-gd-scope', placeholder: '扫描范围（可选，如 frameworks/base）',
              'aria-label': '扫描范围', value: snapshot.scope ?? '',
              onChange: (event) => setState({ scope: event.target.value }),
            }),
            h('span', { key: 'spacer', className: 'dshgw-gd-spacer' }),
            h('span', { key: 'meta', className: 'dshgw-gd-meta', title: snapshot.repo ?? '' },
              snapshot.meta === null
                ? ''
                : `${snapshot.meta.branch ?? ''} @ ${snapshot.meta.short ?? ''}${snapshot.meta.detached ? '（游离）' : ''}`),
            snapshot.error === ''
              ? null
              : h('span', { key: 'err', className: 'dshgw-gd-error', title: snapshot.error },
                  `错误：${snapshot.error.length > 90 ? `${snapshot.error.slice(0, 90)}…` : snapshot.error}`),
          ])

          const chip = (key, label, on, onClick, disabled) => h('button', {
            key, type: 'button', className: 'dshgw-gd-chip', 'data-on': on ? 'true' : 'false',
            disabled: disabled === true,
            onClick,
          }, label)

          const left = h('div', { className: 'dshgw-gd-list', style: { width: `${snapshot.ui.leftWidth}px` } }, [
            h('div', { key: 'chips', className: 'dshgw-gd-chips' }, [
              chip('staged', `已暂存 ${counts.staged}`, snapshot.ui.showStaged, () => setUI({ showStaged: !snapshot.ui.showStaged })),
              chip('unstaged', `未暂存 ${counts.unstaged}`, snapshot.ui.showUnstaged, () => setUI({ showUnstaged: !snapshot.ui.showUnstaged })),
              chip('untracked', `未跟踪 ${counts.untracked}`, snapshot.ui.showUntracked,
                () => {
                  const next = !snapshot.ui.showUntracked
                  setUI({ showUntracked: next })
                  // Untracked files are only knowable by scanning, so turning the chip on rescans;
                  // this is the one action here that can be slow, hence the explicit chip.
                  if (next && !scan.files.some((file) => file.side === 'untracked')) {
                    void startScan({ includeUntracked: true, force: true })
                  }
                },
                running),
              h('input', {
                key: 'filter', className: 'dshgw-gd-input dshgw-gd-filter', placeholder: '过滤路径…',
                'aria-label': '过滤路径', value: snapshot.filter ?? '',
                onChange: (event) => setState({ filter: event.target.value }),
              }),
            ]),
            h('div', { key: 'rows', className: 'dshgw-gd-rows', tabIndex: 0, onKeyDown }, [
              rows.length > 0
                ? rows
                : h('div', { className: 'dshgw-gd-empty' }, emptyMessageOf(snapshot)),
              truncatedList
                ? h('div', { key: 'more', className: 'dshgw-gd-more' },
                    `共 ${visible.length} 条，只显示前 ${MAX_LIST_ROWS} 条；请用过滤缩小范围。`)
                : null,
            ]),
            h('div', { key: 'status', className: 'dshgw-gd-status' }, [
              h('span', { key: 'state' }, scanLabel(scan)),
              running
                ? h('span', { key: 'track', className: 'dshgw-gd-track' },
                    h('span', { className: 'dshgw-gd-fill', style: { width: `${progressOf(scan)}%` } }))
                : null,
              h('span', { key: 'count' }, `${counts.total} 个文件`),
              h('span', { key: 'grow', className: 'dshgw-gd-status-grow' },
                scan.completedAt === null
                  ? (scan.state === 'idle' ? '尚未扫描工作区改动' : '')
                  : `上次扫描 ${formatClock(scan.completedAt)} 用时 ${formatDuration(scan.elapsedMs)}`),
              scan.truncated === true ? h('span', { key: 'trunc', className: 'dshgw-gd-warn' }, '已截断') : null,
            ]),
          ])

          const diffHead = snapshot.diff === null
            ? null
            : h('div', { key: 'dh', className: 'dshgw-gd-dhead' }, [
                h('span', { key: 'p', className: 'dshgw-gd-dpath', title: snapshot.diff.path }, snapshot.diff.path),
                h('span', { key: 'sides', className: 'dshgw-gd-dsides' },
                  (sidesOf(snapshot.rows.find((row) => row.path === snapshot.diff.path) ?? null)).map((side) => h('button', {
                    key: side, type: 'button', className: 'dshgw-gd-tab', 'data-active': snapshot.side === side ? 'true' : 'false',
                    onClick: () => { void loadDiff(snapshot.rows.find((row) => row.path === snapshot.diff.path) ?? null, side) },
                  }, SIDE_SHORT[side]))),
                h('span', { key: 'labels', className: 'dshgw-gd-meta' }, `${snapshot.diff.oldLabel} → ${snapshot.diff.newLabel}`),
                h('span', { key: 'stats', className: 'dshgw-gd-rowstat' }, [
                  h('span', { key: 'a', className: 'dshgw-gd-add' }, `+${snapshot.diff.stats?.additions ?? 0}`),
                  ' ',
                  h('span', { key: 'd', className: 'dshgw-gd-del' }, `−${snapshot.diff.stats?.deletions ?? 0}`),
                ]),
                snapshot.diff.truncated === true ? h('span', { key: 't', className: 'dshgw-gd-warn' }, '已截断') : null,
                h('span', { key: 'spacer', className: 'dshgw-gd-spacer' }),
                h('button', {
                  key: 'mode', type: 'button', className: 'dshgw-gd-tab',
                  title: '在并排与统一两种视图间切换',
                  onClick: () => setUI({ mode: snapshot.ui.mode === 'split' ? 'unified' : 'split' }),
                }, snapshot.ui.mode === 'split' ? '并排' : '统一'),
                h('button', {
                  key: 'wrap', type: 'button', className: 'dshgw-gd-tab', 'data-active': snapshot.ui.wrap ? 'true' : 'false',
                  title: '长行是否折行', onClick: () => setUI({ wrap: !snapshot.ui.wrap }),
                }, '换行'),
              ])

          const diffPane = h('div', { key: 'diff', className: 'dshgw-gd-diff' }, [
            diffHead,
            h('div', { key: 'body', className: 'dshgw-gd-dbody' }, snapshot.diffBusy && snapshot.diff === null
              ? h('div', { className: 'dshgw-gd-empty' }, '正在读取差异…')
              : snapshot.ui.mode === 'unified'
                ? h(UnifiedBody, { diff: snapshot.diff, wrap: snapshot.ui.wrap })
                : h(DiffBody, { diff: snapshot.diff, wrap: snapshot.ui.wrap })),
          ])

          return h('div', { className: 'dshgw-gd-root', role: 'region', 'aria-label': '工作区 git 变更' }, [
            head,
            h('div', { key: 'body', className: 'dshgw-gd-body' }, [
              left,
              h('div', { key: 'grip', className: 'dshgw-gd-grip', onPointerDown: beginDrag, title: '拖动调整列表宽度' }),
              diffPane,
            ]),
          ])
        }

        // ---- small projections used by the view -------------------------------------------

        /**
         * The rows the active filters admit.
         *
         * A row stays visible while ANY of its sides is enabled, so turning off 已暂存 narrows the
         * list down to the worktree changes without also swallowing the files that are staged *and*
         * modified again — those are exactly the ones a reviewer is looking for.
         */
        function visibleRowsOf(snapshot) {
          const query = String(snapshot.filter ?? '').trim().toLowerCase()
          const tracked = (row) => row.staged !== null || row.unstaged !== null
          return snapshot.rows.filter((row) => {
            const visible = (row.staged !== null && snapshot.ui.showStaged === true)
              || (row.unstaged !== null && snapshot.ui.showUnstaged === true)
              || (row.untracked && !tracked(row) && snapshot.ui.showUntracked === true)
            if (!visible) return false
            if (query !== '' && !row.path.toLowerCase().includes(query)) return false
            return true
          })
        }

        /** Per-side totals for the chips. */
        function countsOf(rows) {
          let staged = 0
          let unstaged = 0
          let untracked = 0
          for (const row of rows) {
            if (row.staged !== null) staged += 1
            if (row.unstaged !== null) unstaged += 1
            if (row.untracked) untracked += 1
          }
          return { staged, unstaged, untracked, total: rows.length }
        }

        /** Progress of a running scan, as a percentage of its chunk plan. */
        function progressOf(scan) {
          if (typeof scan.chunksTotal !== 'number' || scan.chunksTotal === 0) return 0
          return Math.min(100, Math.round(((scan.chunksDone ?? 0) / scan.chunksTotal) * 100))
        }

        /** One line describing a scan. */
        function scanLabel(scan) {
          if (scan.state === 'running') {
            return `扫描中 ${scan.chunksDone ?? 0}/${scan.chunksTotal ?? 0} 个目录 · ${formatDuration(scan.elapsedMs)}`
          }
          if (scan.state === 'done') return '扫描完成'
          if (scan.state === 'cancelled') return '扫描已取消'
          if (scan.state === 'failed') return '扫描失败'
          return '未扫描'
        }

        /** What the list says when it has nothing to show. */
        function emptyMessageOf(snapshot) {
          if (snapshot.busy) return '正在读取…'
          if (snapshot.rows.length > 0) return '当前过滤条件下没有文件。'
          if (snapshot.scan.state === 'running') return '正在扫描工作区改动，结果会逐步显示…'
          if (snapshot.scan.state === 'idle') {
            return snapshot.repo === null
              ? '在工作区里没有找到 git 仓库。'
              : '还没有扫描工作区改动：点击「扫描」开始（大仓库会分块进行，可随时取消）。'
          }
          return '没有检测到变更。'
        }

        // ---- registration ----------------------------------------------------------------

        ctx.slots.inject('conversation.view', () => ctx.slots.register({
          name: 'conversation.view',
          id: 'git-diff',
          order: 20,
          label: '变更',
        }, GitDiffView))

        ctx.logger?.info?.('git-diff: client view registered')
      }

      /** Render one line of code, wrapping the character ranges that actually changed. */
      function textWithMarks(text, spans, keyPrefix) {
        if (spans === undefined || spans === null || spans.length === 0) return [text]
        const parts = []
        let cursor = 0
        spans.forEach((range, index) => {
          const [start, end] = range
          if (start > cursor) parts.push(text.slice(cursor, start))
          parts.push(h('mark', { key: `${keyPrefix}-${index}`, className: 'dshgw-gd-mark' }, text.slice(start, end)))
          cursor = end
        })
        if (cursor < text.length) parts.push(text.slice(cursor))
        return parts
      }

      exports.apply = apply
      exports.inject = inject
      /**
       * The pure functions, exported for the tests only.
       *
       * The diff parser and the row pairing are the parts of this half that are worth asserting on
       * directly, and they touch neither the DOM nor the RPC channel — so they are reachable here
       * rather than only through a rendered tree.
       */
      exports.__pure = { parseUnifiedDiff, pairRows, diffRows, buildRows, sidesOf, badgeOf, inlineSpans, splitPath, formatDuration }
      return module.exports
    },
  })
})()
