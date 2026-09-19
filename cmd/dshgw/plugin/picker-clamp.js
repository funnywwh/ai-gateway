// Root-clamped host half for dsh's DirectoryPicker seam.
// Resolve dependencies from the active dsh release: an external file://
// module cannot resolve dsh's bare package imports from its own directory.
import { createRequire } from 'node:module'
import { realpathSync, statSync } from 'node:fs'
import { mkdir, opendir, realpath, stat } from 'node:fs/promises'
import { homedir } from 'node:os'
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'
import { pathToFileURL } from 'node:url'

const anchor = process.env.DSHGW_DSH_ANCHOR || process.argv[1]
if (!anchor || !isAbsolute(anchor)) throw new Error('picker-clamp: DSH anchor is unavailable')
const requireFromDsh = createRequire(pathToFileURL(anchor))
const loadFromDsh = (name) => import(pathToFileURL(requireFromDsh.resolve(name)).href)
// Sequential imports avoid a Node 22 CJS/ESM module-status assertion observed
// when these specific packages are loaded concurrently.
const browse = await loadFromDsh('@deepseek-ai/dsh-host-directory-picker-browse')
const { DirectoryPicker, DirectoryPickerError } = await loadFromDsh('@deepseek-ai/dsh-host-directory-picker')
const { default: z } = await loadFromDsh('@deepseek-ai/schemastery')
const { boundedInsert, fullyQualified, raceAbort } = browse
if (typeof DirectoryPicker !== 'function' || typeof DirectoryPickerError !== 'function' ||
    typeof boundedInsert !== 'function' || typeof fullyQualified !== 'function' ||
    typeof raceAbort !== 'function') throw new Error('picker-clamp: incompatible DirectoryPicker API')

const messageOf = (error) => error instanceof Error ? error.message : String(error)
const swallow = () => {}
const within = (root, candidate) => {
  const rel = relative(root, candidate)
  return rel === '' || (rel !== '..' && !rel.startsWith(`..${sep}`) && !isAbsolute(rel))
}
const crumbsFrom = (root, target) => {
  const crumbs = []
  for (let current = target;; current = dirname(current)) {
    crumbs.unshift({ name: current === root ? root : basename(current), path: current, hidden: false })
    if (current === root) return crumbs
    const parent = dirname(current)
    if (parent === current || !within(root, parent)) throw new Error(`cannot build crumbs for ${target}`)
  }
}

class PickerClamp extends DirectoryPicker {
  static Config = z.object({
    root: z.string().default(homedir()),
    maxEntries: z.natural().min(1).default(1000),
  })

  constructor(ctx, config) {
    super(ctx)
    this.config = config
    this.rootPath = config.root
    try {
      if (!fullyQualified(this.rootPath)) throw new Error(`root is not fully qualified: ${this.rootPath}`)
      this.rootPath = resolve(this.rootPath)
      this.realRoot = realpathSync(this.rootPath)
      if (!statSync(this.realRoot).isDirectory()) throw new Error(`${this.rootPath} is not a directory`)
    } catch (error) {
      this.startupError = error
      this.realRoot = this.rootPath
      ctx.logger.error(new Error(`picker-clamp is deny-only: ${messageOf(error)}`, { cause: error }))
    }
    this.browseCapability = Object.freeze({
      kind: 'browse',
      list: (path, signal) => this.list(path, signal),
      createDirectory: (path, name) => this.createDirectory(path, name),
    })
  }

  capability() { return this.browseCapability }

  assertReady(code, path) {
    if (this.startupError) {
      throw new DirectoryPickerError(code, path, `clamped picker unavailable: ${messageOf(this.startupError)}`)
    }
  }

  async checkedTarget(path, code, signal) {
    const subject = path ?? this.rootPath
    this.assertReady(code, subject)
    if (!fullyQualified(subject)) {
      throw new DirectoryPickerError(code, subject, `path is not fully qualified: ${subject}`)
    }
    const display = resolve(subject)
    if (!within(this.rootPath, display)) {
      throw new DirectoryPickerError(code, display, `path is outside picker root ${this.rootPath}: ${display}`)
    }
    let canonical
    try {
      canonical = await raceAbort(realpath(display), signal)
    } catch (error) {
      signal?.throwIfAborted()
      throw new DirectoryPickerError(code, display, `cannot resolve ${display}: ${messageOf(error)}`)
    }
    if (!within(this.realRoot, canonical)) {
      throw new DirectoryPickerError(code, display, `path escapes picker root ${this.rootPath}: ${display}`)
    }
    return { display, canonical }
  }

  async directoryRow(parent, candidate, signal) {
    if (!candidate.isDirectory) {
      try {
        const canonical = await raceAbort(realpath(join(parent.canonical, candidate.name)), signal)
        if (!within(this.realRoot, canonical)) return null
        if (!(await raceAbort(stat(canonical), signal)).isDirectory()) return null
      } catch {
        signal?.throwIfAborted()
        return null
      }
    }
    return { name: candidate.name, path: join(parent.display, candidate.name), hidden: candidate.name.startsWith('.') }
  }

  // A directory under the picker root can legitimately vanish: a browser or SSH
  // mount is torn down when its owner disconnects, and the gateway removes the mount
  // point with it. The dialog that asked for that directory must still open, so a
  // listing falls back to the nearest existing ancestor INSIDE the root (the root
  // always exists) instead of failing the whole listing. The returned value carries
  // the directory actually listed, so the caller is never told it is somewhere it is
  // not. createDirectory stays strict: a parent that is gone is a caller mistake.
  async listTarget(path, signal) {
    const subject = path ?? this.rootPath
    this.assertReady('directory-unreadable', subject)
    if (!fullyQualified(subject)) {
      throw new DirectoryPickerError('directory-unreadable', subject, `path is not fully qualified: ${subject}`)
    }
    const display = resolve(subject)
    if (!within(this.rootPath, display)) {
      throw new DirectoryPickerError('directory-unreadable', display, `path is outside picker root ${this.rootPath}: ${display}`)
    }
    for (let candidate = display;; candidate = dirname(candidate)) {
      let canonical
      try {
        canonical = await raceAbort(realpath(candidate), signal)
      } catch (error) {
        signal?.throwIfAborted()
        if (candidate === this.rootPath) {
          throw new DirectoryPickerError('directory-unreadable', display, `cannot resolve ${display}: ${messageOf(error)}`)
        }
        continue
      }
      if (!within(this.realRoot, canonical)) {
        throw new DirectoryPickerError('directory-unreadable', display, `path escapes picker root ${this.rootPath}: ${display}`)
      }
      return { display: candidate, canonical }
    }
  }

  async list(path, signal) {
    const target = await this.listTarget(path, signal)
    const keep = this.config.maxEntries + 1
    const window = []
    let evicted = false
    try {
      const opening = opendir(target.canonical)
      const level = await raceAbort(opening, signal).catch((error) => {
        opening.then((dir) => dir.close().catch(swallow), swallow)
        throw error
      })
      try {
        for (;;) {
          const dirent = await raceAbort(level.read(), signal)
          if (dirent === null) break
          if (!dirent.isDirectory() && !dirent.isSymbolicLink()) continue
          if (boundedInsert(window, {
            name: dirent.name,
            isDirectory: dirent.isDirectory(),
            isSymbolicLink: dirent.isSymbolicLink(),
          }, keep)) evicted = true
        }
      } finally {
        const closing = level.close()
        if (signal?.aborted) closing.catch(swallow)
        else await closing
      }
    } catch (error) {
      signal?.throwIfAborted()
      throw new DirectoryPickerError('directory-unreadable', target.display,
        `cannot list ${target.display}: ${messageOf(error)}`)
    }
    const entries = []
    let truncated = evicted
    for (const candidate of window) {
      signal?.throwIfAborted()
      const row = await this.directoryRow(target, candidate, signal)
      if (row === null) continue
      if (entries.length === this.config.maxEntries) {
        truncated = true
        break
      }
      entries.push(row)
    }
    return {
      path: target.display,
      home: this.rootPath,
      crumbs: crumbsFrom(this.rootPath, target.display),
      entries,
      truncated,
    }
  }

  async createDirectory(path, name) {
    const parent = await this.checkedTarget(path, 'directory-create-failed')
    const target = join(parent.display, name)
    if (name.trim() === '' || name === '.' || name === '..' || /[/\\]/.test(name)) {
      throw new DirectoryPickerError('directory-create-failed', target, `"${name}" is not a single path segment`)
    }
    try {
      // Use the already canonicalized parent, not a display symlink.
      await mkdir(join(parent.canonical, name))
      return target
    } catch (error) {
      if (error && typeof error === 'object' && error.code === 'EEXIST') {
        throw new DirectoryPickerError('directory-exists', target, `${target} already exists`)
      }
      throw new DirectoryPickerError('directory-create-failed', target,
        `cannot create ${target}: ${messageOf(error)}`)
    }
  }
}

export default PickerClamp
