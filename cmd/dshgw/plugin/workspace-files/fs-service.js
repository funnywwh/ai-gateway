// Workspace-scoped file service for dshgw-workspace-files.
//
// One root, one clamp, one error vocabulary. Every path the browser sends is a POSIX-style path
// relative to the workspace root: nothing here accepts an absolute path from the wire, `..` is
// refused rather than normalised away, and a symlink whose target leaves the root is refused
// rather than followed. Reads and writes are bounded by the configured limits, so one page can
// never ask the host to slurp a file of unbounded size into memory.
//
// This module imports nothing from dsh, so the whole surface is unit-tested against a temporary
// directory (see test/host.test.mjs).

import { promises as fsp } from 'node:fs'
import { basename, dirname, join, sep } from 'node:path'

/** Stable failure codes; the endpoint table passes them through to the browser verbatim. */
export const CODES = {
  badPath: 'files/bad-path',
  outsideRoot: 'files/outside-root',
  notFound: 'files/not-found',
  notDirectory: 'files/not-a-directory',
  isDirectory: 'files/is-a-directory',
  notFile: 'files/not-a-file',
  tooLarge: 'files/too-large',
  binary: 'files/binary',
  exists: 'files/exists',
  readOnly: 'files/read-only',
  notEmpty: 'files/not-empty',
  denied: 'files/denied',
  io: 'files/io',
}

/** Longest single path segment this service will create or accept. */
export const MAX_SEGMENT_BYTES = 255
/** Longest whole relative path this service will accept. */
export const MAX_PATH_CHARS = 4096

const BINARY_SNIFF_BYTES = 8192

/** Build one coded failure. */
export function fail(code, message, details = {}) {
  const error = new Error(message)
  error.code = code
  error.details = details
  return error
}

/** Clamp a value to an integer inside a range, falling back to a default. */
export function clampInt(value, fallback, min, max) {
  const parsed = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(parsed)) return fallback
  return Math.max(min, Math.min(max, Math.floor(parsed)))
}

/** Map a node errno onto this service's vocabulary, keeping the original message. */
function fromNodeError(error, path) {
  const code = typeof error?.code === 'string' ? error.code : ''
  const message = error instanceof Error ? error.message : String(error)
  // An error that already speaks this vocabulary (the clamp, most often) passes through untouched:
  // re-wrapping it would turn "outside the workspace" into a generic I/O failure.
  if (code.startsWith('files/')) return error
  switch (code) {
    case 'ENOENT': return fail(CODES.notFound, `找不到 ${path || '该路径'}`, { path })
    case 'ENOTDIR': return fail(CODES.notDirectory, `${path} 不是目录`, { path })
    case 'EISDIR': return fail(CODES.isDirectory, `${path} 是目录`, { path })
    case 'EEXIST': return fail(CODES.exists, `${path} 已存在`, { path })
    case 'ENOTEMPTY': return fail(CODES.notEmpty, `${path} 不是空目录`, { path })
    case 'EACCES':
    case 'EPERM': return fail(CODES.denied, `没有权限访问 ${path}`, { path })
    case 'ENAMETOOLONG': return fail(CODES.badPath, `路径过长：${path}`, { path })
    default: return fail(CODES.io, message, { path })
  }
}

/**
 * Validate one client-supplied relative path.
 *
 * The wire contract is POSIX-style and root-relative: `''` is the workspace root itself. `.`,
 * `..`, empty segments, backslashes and NUL are rejected outright — a path is never silently
 * rewritten into something else, so a caller cannot probe for a path it did not ask for.
 */
export function normalizeRelPath(input) {
  if (input === undefined || input === null || input === '') return ''
  if (typeof input !== 'string') throw fail(CODES.badPath, 'path 必须是字符串', { path: String(input) })
  if (input.length > MAX_PATH_CHARS) throw fail(CODES.badPath, '路径过长', { path: input.slice(0, 200) })
  if (input.includes('\0')) throw fail(CODES.badPath, '路径包含 NUL', { path: '' })
  if (input.includes('\\')) throw fail(CODES.badPath, '路径不能包含反斜杠', { path: input })
  if (input.startsWith('/')) throw fail(CODES.badPath, '路径必须是相对于工作区的相对路径', { path: input })
  const trimmed = input.replace(/\/+$/, '')
  if (trimmed === '') return ''
  const segments = trimmed.split('/')
  for (const segment of segments) {
    if (segment === '' || segment === '.' || segment === '..') {
      throw fail(CODES.badPath, `路径段不允许是 ${JSON.stringify(segment)}`, { path: input })
    }
    if (Buffer.byteLength(segment) > MAX_SEGMENT_BYTES) throw fail(CODES.badPath, '路径段过长', { path: input })
  }
  return segments.join('/')
}

/**
 * Validate one new name a mutation is about to create (a folder, a rename target).
 *
 * Same rules as a single path segment, plus the refusal of anything filesystem-special, so the
 * browser cannot create a hidden or self-referential entry by accident.
 */
export function normalizeName(input) {
  if (typeof input !== 'string') throw fail(CODES.badPath, '名称必须是字符串', {})
  const name = input.trim()
  if (name === '' || name === '.' || name === '..') throw fail(CODES.badPath, '名称无效', { name: input })
  if (name.includes('/') || name.includes('\\') || name.includes('\0')) {
    throw fail(CODES.badPath, '名称不能包含路径分隔符', { name: input })
  }
  if (Buffer.byteLength(name) > MAX_SEGMENT_BYTES) throw fail(CODES.badPath, '名称过长', { name: input })
  return name
}

/** Join one relative path onto the root for display and comparison. */
export function joinRel(parent, name) {
  return parent === '' ? name : `${parent}/${name}`
}

/** Split one relative path into its parent and its final segment. */
export function splitRel(path) {
  const clean = normalizeRelPath(path)
  if (clean === '') return { parent: null, name: '' }
  const cut = clean.lastIndexOf('/')
  return cut === -1 ? { parent: '', name: clean } : { parent: clean.slice(0, cut), name: clean.slice(cut + 1) }
}

/** Whether a buffer looks binary: a NUL byte in the sniff window, or invalid UTF-8 sequences. */
export function looksBinary(buffer) {
  const window = buffer.subarray(0, Math.min(buffer.length, BINARY_SNIFF_BYTES))
  if (window.includes(0)) return true
  // A byte that cannot start a UTF-8 sequence where a decoder would expect one is the second
  // signal: text editors open such a file as mojibake, so the browser is told it is not text.
  try {
    new TextDecoder('utf-8', { fatal: true }).decode(window)
    return false
  } catch {
    return true
  }
}

/** Kind vocabulary shared with the browser. */
export const KIND_DIR = 'dir'
export const KIND_FILE = 'file'
export const KIND_SYMLINK_BROKEN = 'broken'
export const KIND_OTHER = 'other'

/**
 * The workspace file service.
 *
 * Construction is async because the root is resolved through `realpath`: the clamp compares real
 * paths, so a workspace reached through a symlinked parent cannot be escaped by symlinking an
 * inner path back out to the same ancestor.
 */
export class WorkspaceFiles {
  /**
   * @param options - the resolved workspace root and the configured limits.
   */
  constructor({ root, rootLabel = null, readOnly = false, limits = {}, log = () => {} }) {
    this.root = root
    this.rootLabel = rootLabel === null || rootLabel === '' ? basename(root) : rootLabel
    this.readOnly = readOnly === true
    this.log = log
    this.limits = {
      maxTextBytes: limits.maxTextBytes,
      chunkBytes: limits.chunkBytes,
      maxListEntries: limits.maxListEntries,
      maxUploadBytes: limits.maxUploadBytes,
      maxSearchResults: limits.maxSearchResults,
      maxSearchDepth: limits.maxSearchDepth,
      maxSearchVisits: limits.maxSearchVisits,
    }
  }

  /**
   * Resolve the root once at activation: a missing or non-directory root fails loudly here rather
   * than turning every later call into a 404.
   *
   * @param options - the same options as the constructor.
   * @returns the live service.
   */
  static async create(options) {
    const requested = typeof options?.root === 'string' && options.root !== '' ? options.root : process.cwd()
    let real
    try {
      real = await fsp.realpath(requested)
    } catch (error) {
      throw fail(CODES.io, `工作区根目录不可用：${requested}（${error instanceof Error ? error.message : String(error)}）`, {})
    }
    const stats = await fsp.stat(real)
    if (!stats.isDirectory()) throw fail(CODES.notDirectory, `工作区根目录不是目录：${real}`, {})
    return new WorkspaceFiles({ ...options, root: real })
  }

  /** Whether one absolute path is the root or lives under it. */
  inside(absolute) {
    return absolute === this.root || absolute.startsWith(this.root.endsWith(sep) ? this.root : `${this.root}${sep}`)
  }

  /** Lexical resolution of one relative path: no filesystem access, no escaping the root. */
  resolve(input) {
    const rel = normalizeRelPath(input)
    const absolute = rel === '' ? this.root : join(this.root, rel)
    if (!this.inside(absolute)) throw fail(CODES.outsideRoot, '路径不在工作区内', { path: rel })
    return { rel, abs: absolute }
  }

  /**
   * Real-path resolution with the clamp applied to the real target.
   *
   * `mustExist` is for operations that read; otherwise the parent directory is resolved (it does
   * exist — it is where the new entry lands) and the final segment is appended unresolved, which
   * is the only way to test a path that is about to be created.
   */
  async locate(input, { mustExist = true, createParent = false } = {}) {
    const { rel, abs } = this.resolve(input)
    if (rel === '') return { rel, abs, real: this.root }
    if (mustExist) {
      try {
        return { rel, abs, real: await this.realTarget(abs, rel) }
      } catch (error) {
        throw fromNodeError(error, rel)
      }
    }
    if (createParent) {
      try {
        const parentReal = await fsp.realpath(dirname(abs))
        if (!this.inside(parentReal)) throw fail(CODES.outsideRoot, '父目录不在工作区内', { path: rel })
        return { rel, abs, real: join(parentReal, basename(abs)) }
      } catch (error) {
        if (error?.code === CODES.outsideRoot) throw error
        throw fromNodeError(error, rel)
      }
    }
    return { rel, abs, real: abs }
  }

  /** `realpath` followed by the clamp, so a symlink that leaves the root is an error, not a path. */
  async realTarget(absolute, label) {
    const real = await fsp.realpath(absolute)
    if (!this.inside(real)) throw fail(CODES.outsideRoot, '路径指向工作区之外', { path: label })
    return real
  }

  /** Describe one entry for the browser. */
  async describe(abs, rel, stats) {
    const symlink = stats.isSymbolicLink()
    let kind = KIND_OTHER
    let size = stats.size
    let target = null
    if (symlink) {
      try {
        const followed = await fsp.stat(abs)
        if (followed.isDirectory()) kind = KIND_DIR
        else if (followed.isFile()) { kind = KIND_FILE; size = followed.size }
        target = await fsp.realpath(abs).catch(() => null)
      } catch {
        kind = KIND_SYMLINK_BROKEN
      }
    } else if (stats.isDirectory()) kind = KIND_DIR
    else if (stats.isFile()) kind = KIND_FILE
    const name = basename(abs)
    return {
      name,
      path: rel,
      kind,
      size: kind === KIND_DIR ? 0 : size,
      mtimeMs: Math.round(stats.mtimeMs),
      symlink,
      hidden: name.startsWith('.'),
      outside: target !== null && !this.inside(target),
    }
  }

  /** List one directory. */
  async list(input, { showHidden = false } = {}) {
    const { rel, real } = await this.locate(input)
    const stats = await this.statReal(real, rel)
    if (!stats.isDirectory()) throw fail(CODES.notDirectory, `${rel || '工作区根'} 不是目录`, { path: rel })
    let dirents
    try {
      dirents = await fsp.readdir(real, { withFileTypes: true })
    } catch (error) {
      throw fromNodeError(error, rel)
    }
    const entries = []
    let hidden = 0
    for (const dirent of dirents) {
      if (dirent.name.startsWith('.')) {
        hidden += 1
        if (!showHidden) continue
      }
      const childRel = joinRel(rel, dirent.name)
      let childStats
      try {
        childStats = await fsp.lstat(join(real, dirent.name))
      } catch {
        continue // A racing delete costs one row, never the whole listing.
      }
      entries.push(await this.describe(join(real, dirent.name), childRel, childStats))
    }
    entries.sort(compareEntries)
    const truncated = entries.length > this.limits.maxListEntries
    const shown = truncated ? entries.slice(0, this.limits.maxListEntries) : entries
    return {
      path: rel,
      absolute: real,
      parent: splitRel(rel).parent,
      entries: shown,
      total: entries.length,
      hidden,
      truncated,
    }
  }

  /** `stat` one path, following symlinks but never leaving the root. */
  async stat(input) {
    const { rel, real } = await this.locate(input)
    const stats = await this.statReal(real, rel)
    const described = await this.describe(real, rel, await fsp.lstat(real).catch(() => stats))
    return {
      path: rel,
      absolute: real,
      name: rel === '' ? this.rootLabel : described.name,
      kind: rel === '' ? KIND_DIR : described.kind,
      size: described.size,
      mtimeMs: Math.round(stats.mtimeMs),
      symlink: described.symlink,
    }
  }

  async statReal(real, rel) {
    try {
      return await fsp.stat(real)
    } catch (error) {
      throw fromNodeError(error, rel || '工作区根')
    }
  }

  /** Read a text file whole, refusing anything past the text limit or that is not decodable text. */
  async readText(input, { maxBytes } = {}) {
    const limit = clampInt(maxBytes, this.limits.maxTextBytes, 1024, this.limits.maxTextBytes)
    const { rel, real } = await this.locate(input)
    const stats = await this.statReal(real, rel)
    if (stats.isDirectory()) throw fail(CODES.isDirectory, `${rel} 是目录`, { path: rel })
    if (!stats.isFile()) throw fail(CODES.notFile, `${rel} 不是普通文件`, { path: rel })
    if (stats.size > limit) {
      throw fail(CODES.tooLarge, `文件 ${Math.round(stats.size / 1024)} KiB 超过编辑器上限 ${Math.round(limit / 1024)} KiB`, {
        path: rel, size: stats.size, limit,
      })
    }
    let buffer
    try {
      buffer = await fsp.readFile(real)
    } catch (error) {
      throw fromNodeError(error, rel)
    }
    if (looksBinary(buffer)) throw fail(CODES.binary, `${rel} 不是文本文件`, { path: rel, size: stats.size })
    return {
      path: rel,
      size: stats.size,
      mtimeMs: Math.round(stats.mtimeMs),
      text: buffer.toString('utf8'),
      limit,
    }
  }

  /** Write a text file, optionally refusing to clobber a version the browser has not seen. */
  async writeText(input, text, { expectedMtimeMs = null } = {}) {
    this.assertWritable()
    if (typeof text !== 'string') throw fail(CODES.io, 'text 必须是字符串', {})
    const bytes = Buffer.byteLength(text)
    if (bytes > this.limits.maxTextBytes) {
      throw fail(CODES.tooLarge, `内容 ${Math.round(bytes / 1024)} KiB 超过编辑器上限`, { size: bytes })
    }
    const { rel, real } = await this.locate(input, { mustExist: false, createParent: true })
    if (rel === '') throw fail(CODES.badPath, '不能写入工作区根目录', {})
    const existing = await fsp.lstat(real).catch(() => null)
    if (existing !== null && existing.isDirectory()) throw fail(CODES.isDirectory, `${rel} 是目录`, { path: rel })
    if (existing !== null && expectedMtimeMs !== null && Math.round(existing.mtimeMs) !== Math.round(Number(expectedMtimeMs))) {
      throw fail(CODES.exists, `${rel} 已被其他改动覆盖，请重新打开后再保存`, {
        path: rel, mtimeMs: Math.round(existing.mtimeMs),
      })
    }
    try {
      await fsp.writeFile(real, text, 'utf8')
    } catch (error) {
      throw fromNodeError(error, rel)
    }
    const stats = await fsp.stat(real).catch(() => null)
    return { path: rel, size: stats?.size ?? bytes, mtimeMs: stats === null ? Date.now() : Math.round(stats.mtimeMs), created: existing === null }
  }

  /** Read one byte range as base64: the download and preview primitive. */
  async readChunk(input, { offset = 0, length } = {}) {
    const size = clampInt(length, this.limits.chunkBytes, 1, this.limits.chunkBytes)
    const { rel, real } = await this.locate(input)
    const stats = await this.statReal(real, rel)
    if (stats.isDirectory()) throw fail(CODES.isDirectory, `${rel} 是目录`, { path: rel })
    if (!stats.isFile()) throw fail(CODES.notFile, `${rel} 不是普通文件`, { path: rel })
    const from = clampInt(offset, 0, 0, stats.size)
    const want = Math.max(0, Math.min(size, stats.size - from))
    const buffer = Buffer.allocUnsafe(want)
    let handle
    try {
      handle = await fsp.open(real, 'r')
      const { bytesRead } = want === 0 ? { bytesRead: 0 } : await handle.read(buffer, 0, want, from)
      return {
        path: rel,
        offset: from,
        size: stats.size,
        mtimeMs: Math.round(stats.mtimeMs),
        bytes: buffer.subarray(0, bytesRead).toString('base64'),
        eof: from + bytesRead >= stats.size,
      }
    } catch (error) {
      throw fromNodeError(error, rel)
    } finally {
      await handle?.close().catch(() => {})
    }
  }

  /** Write one base64 byte range: the upload primitive. Offset 0 truncates the file. */
  async writeChunk(input, { offset = 0, data = '', } = {}) {
    this.assertWritable()
    const from = clampInt(offset, 0, 0, Number.MAX_SAFE_INTEGER)
    const payload = Buffer.from(typeof data === 'string' ? data : '', 'base64')
    if (payload.length > this.limits.chunkBytes) {
      throw fail(CODES.tooLarge, `单次写入 ${payload.length} 字节超过上限 ${this.limits.chunkBytes} 字节`, {
        size: payload.length, limit: this.limits.chunkBytes,
      })
    }
    const { rel, real } = await this.locate(input, { mustExist: false, createParent: true })
    if (rel === '') throw fail(CODES.badPath, '不能写入工作区根目录', {})
    const existing = await fsp.lstat(real).catch(() => null)
    if (existing !== null && existing.isDirectory()) throw fail(CODES.isDirectory, `${rel} 是目录`, { path: rel })
    if (from > this.limits.maxUploadBytes) throw fail(CODES.tooLarge, `写入位置 ${from} 超过上限`, {})
    let handle
    try {
      handle = await fsp.open(real, from === 0 ? 'w' : 'r+')
      if (payload.length > 0) await handle.write(payload, 0, payload.length, from)
    } catch (error) {
      throw fromNodeError(error, rel)
    } finally {
      await handle?.close().catch(() => {})
    }
    const stats = await fsp.stat(real).catch(() => null)
    return { path: rel, offset: from, written: payload.length, size: stats?.size ?? from + payload.length }
  }

  /** Create one directory (with any missing parents inside the workspace). */
  async mkdir(input, { name = null } = {}) {
    this.assertWritable()
    const target = name === null ? input : joinRel(normalizeRelPath(input), normalizeName(name))
    const { rel, real } = await this.locate(target, { mustExist: false, createParent: true })
    if (rel === '') throw fail(CODES.badPath, '工作区根目录已存在', {})
    try {
      await fsp.mkdir(real, { recursive: false })
    } catch (error) {
      throw fromNodeError(error, rel)
    }
    return this.stat(rel)
  }

  /** Rename one entry in place (same directory, new name); moving is not offered yet. */
  async rename(input, { name } = {}) {
    this.assertWritable()
    const clean = normalizeName(name)
    const { rel, real } = await this.locate(input)
    if (rel === '') throw fail(CODES.badPath, '不能重命名工作区根目录', {})
    const target = join(dirname(real), clean)
    if (!this.inside(target)) throw fail(CODES.outsideRoot, '目标不在工作区内', {})
    const clash = await fsp.lstat(target).catch(() => null)
    if (clash !== null) throw fail(CODES.exists, `${clean} 已存在`, { path: joinRel(splitRel(rel).parent, clean) })
    try {
      await fsp.rename(real, target)
    } catch (error) {
      throw fromNodeError(error, rel)
    }
    return this.stat(joinRel(splitRel(rel).parent, clean))
  }

  /** Delete one entry; a directory needs `recursive`, and the root is never removable. */
  async remove(input, { recursive = false } = {}) {
    this.assertWritable()
    const { rel, real } = await this.locate(input)
    if (rel === '') throw fail(CODES.badPath, '不能删除工作区根目录', {})
    const stats = await fsp.lstat(real).catch((error) => { throw fromNodeError(error, rel) })
    try {
      // `rmdir` (not `rm`) is the non-recursive directory path: it is the call that reports
      // ENOTEMPTY, which is the answer the dialog's promise ("空目录才能删") is built on.
      if (stats.isDirectory()) await (recursive === true ? fsp.rm(real, { recursive: true, force: false }) : fsp.rmdir(real))
      else await fsp.unlink(real)
    } catch (error) {
      throw fromNodeError(error, rel)
    }
    return { path: rel, kind: stats.isDirectory() ? KIND_DIR : KIND_FILE }
  }

  /**
   * Search file and directory names under the workspace, breadth-first and bounded.
   *
   * Symlinked directories and hidden directories are not descended into, so a workspace holding a
   * repository checkout cannot turn one query into an unbounded walk.
   */
  async find(query, { limit, maxDepth, showHidden = false } = {}) {
    const needle = typeof query === 'string' ? query.trim().toLowerCase() : ''
    if (needle === '') return { query: '', matches: [], visited: 0, truncated: false }
    const cap = clampInt(limit, this.limits.maxSearchResults, 1, this.limits.maxSearchResults)
    const depthCap = clampInt(maxDepth, this.limits.maxSearchDepth, 1, this.limits.maxSearchDepth)
    const matches = []
    const queue = [{ rel: '', depth: 0 }]
    let visited = 0
    let truncated = false
    while (queue.length > 0) {
      const current = queue.shift()
      if (current.depth >= depthCap) continue
      let dirents
      try {
        dirents = await fsp.readdir(join(this.root, current.rel), { withFileTypes: true })
      } catch {
        continue
      }
      for (const dirent of dirents) {
        visited += 1
        if (visited > this.limits.maxSearchVisits) { truncated = true; break }
        if (dirent.name.startsWith('.') && !showHidden) continue
        const rel = joinRel(current.rel, dirent.name)
        if (dirent.name.toLowerCase().includes(needle)) {
          try {
            matches.push(await this.describe(join(this.root, rel), rel, await fsp.lstat(join(this.root, rel))))
          } catch {
            // A racing delete costs one hit, never the whole search.
          }
          if (matches.length >= cap) { truncated = true; break }
        }
        if (dirent.isDirectory() && !dirent.isSymbolicLink()) queue.push({ rel, depth: current.depth + 1 })
      }
      if (truncated) break
    }
    return { query: needle, matches, visited, truncated }
  }

  /** Refuse every mutation when the plugin is configured read-only. */
  assertWritable() {
    if (this.readOnly) throw fail(CODES.readOnly, '这个文件管理器是只读模式', {})
  }

  /** What the browser needs to label the surface and size its requests. */
  hello() {
    return {
      root: this.root,
      rootLabel: this.rootLabel,
      readOnly: this.readOnly,
      limits: this.limits,
    }
  }
}

/** Directory rows first, then case-insensitive name order with numbers compared numerically. */
export function compareEntries(left, right) {
  const leftDir = left.kind === KIND_DIR ? 0 : 1
  const rightDir = right.kind === KIND_DIR ? 0 : 1
  if (leftDir !== rightDir) return leftDir - rightDir
  return left.name.localeCompare(right.name, 'zh-Hans-CN', { numeric: true, sensitivity: 'base' })
}
