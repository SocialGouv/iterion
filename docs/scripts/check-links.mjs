// Fail the build if any link on the site would 404.
//
// VitePress's own dead-link check covers doc-to-doc markdown links, but NOT the
// links defined in config (sidebar/nav/home `link:`) nor the code-links this
// site rewrites to github.com. This audits the BUILT site deterministically:
//   - internal links resolve to a built page in dist/
//   - github blob/tree links to this repo resolve to a real path in the tree
// Run as the postbuild step so "no link 404s" stays enforced, not one-off.
//
// The checker BLOCKS publishing, so a false positive costs as much as a hole
// and is harder to see. Every predicate here is exported and exercised against
// a built fixture by check-links.test.mjs — `task docs:links:test`.
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { dirname, join } from 'node:path'
import posixpath from 'node:path/posix'

import { PUBLIC_ROOT, publicSitePath } from '../.vitepress/public-links.mjs'

const docsRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
const repoRoot = join(docsRoot, '..')
const distDir = join(docsRoot, '.vitepress', 'dist')

export const BLOB = 'https://github.com/SocialGouv/iterion/blob/main/'
export const TREE = 'https://github.com/SocialGouv/iterion/tree/main/'
export const BASE = '/iterion/'

// An href that names no location in this site: a URL with a scheme (mailto:,
// tel:, javascript:, data:, http:) or a protocol-relative one. config.ts
// recognises the same two forms — `/^([a-z]+:)?\/\//i` — and the two files
// disagreeing is what reported `//example.com/x` as an absolute link without
// a base.
const NOT_A_PATH = /^([a-z][a-z0-9+.-]*:|\/\/)/i

// An href is percent-encoded; dist keys are the bytes readdir returns. A
// malformed escape is left as written rather than thrown: the checker's job
// is to report the link, not to die on it.
function decodeHref(s) {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

// Set of dist-relative file paths that exist (the built site).
export function collectDistFiles(dist) {
  const files = new Set()
  ;(function walk(d) {
    for (const e of readdirSync(d)) {
      const p = join(d, e)
      statSync(p).isDirectory() ? walk(p) : files.add(p.slice(dist.length + 1).replace(/\\/g, '/'))
    }
  })(dist)
  return files
}

export function siteExists(distFiles, sitePath) {
  // '.' and './' both name the site root: normalize() collapses `..` and `../`
  // to one or the other depending only on the href's trailing slash, from any
  // depth. dist keys carry neither prefix (path.join never emits one), so both
  // must map to '' or the root is reported dead from every subdirectory.
  const p = sitePath === '.' ? '' : sitePath.replace(/^\.\//, '')
  if (p === '' || p.endsWith('/')) return distFiles.has(p + 'index.html')
  // No extension heuristic. A page name can carry a dot — `probe.v1.2` is a
  // version, and `/\.[a-z0-9]+$/` read `.2` as a file extension — so the
  // built site is asked for all three spellings instead of one of them being
  // chosen from the href. Strictly more permissive than the branch it
  // replaces: it can only turn a false positive into a pass, never hide a
  // dead link, since every arm still has to name a file dist really holds.
  return distFiles.has(p) || distFiles.has(p + '.html') || distFiles.has(p + '/index.html')
}

// trackedPaths is the set of paths github.com serves for this repository: the
// files git tracks, plus every directory one of them lies in (a tree URL
// names a directory, and git has no object for one that holds no file).
//
// The question is "does GIT serve this path", and the disk cannot answer it:
// a gitignored directory, an untracked one, and an empty one — git cannot
// track an empty directory at all — each satisfy existsSync and 404 for a
// reader.
//
// It reads the checkout, never a ref: `actions/checkout` at depth 1 has the
// commit and not the history, and not necessarily a ref named after the
// default branch, so asking git for `main:<path>` would flag every github
// link in CI — a false positive in a tool that blocks publishing.
export function trackedPaths(repoRoot) {
  let listing
  try {
    listing = execFileSync('git', ['-C', repoRoot, 'ls-files', '-z'], {
      encoding: 'utf8',
      maxBuffer: 256 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
  } catch (err) {
    throw new Error(
      `check-links: cannot list the tracked files of ${repoRoot} (${String(err.message).trim()}) — ` +
        'the github half of this check asks what git serves, and falling back to the disk would ' +
        'certify the very paths it exists to catch',
    )
  }
  const paths = new Set()
  for (const file of listing.split('\0')) {
    if (!file) continue
    paths.add(file)
    for (let dir = posixpath.dirname(file); dir && dir !== '.'; dir = posixpath.dirname(dir)) paths.add(dir)
  }
  if (paths.size === 0) {
    throw new Error(
      `check-links: git tracks no file under ${repoRoot} — every github link would be reported broken, ` +
        'which says something about this invocation and nothing about the links',
    )
  }
  return paths
}

// repoResolver is the predicate brokenLinks asks about a github target. The
// listing is read once: a subprocess per link would be 600 of them.
export function repoResolver(repoRoot) {
  const tracked = trackedPaths(repoRoot)
  return (relpath) => tracked.has(relpath)
}

// brokenLinks audits every built page of dist and returns href -> Set(pages).
// `repoHas(relpath)` answers whether github.com serves that path of this
// repository.
export function brokenLinks({ dist, distFiles, repoHas }) {
  const hrefRe = /href="([^"]+)"/g
  const broken = new Map()
  const misrouted = new Map()
  const add = (into) => (href, page) => {
    if (!into.has(href)) into.set(href, new Set())
    into.get(href).add(page)
  }
  const flag = add(broken)
  const misroute = add(misrouted)

  for (const rel of distFiles) {
    if (!rel.endsWith('.html')) continue
    const page = rel
    const pagedir = posixpath.dirname(page)
    const html = readFileSync(join(dist, rel), 'utf8')
    let m
    hrefRe.lastIndex = 0
    while ((m = hrefRe.exec(html))) {
      const raw = m[1]
      // A fragment names a place on the page and a query string is part of no
      // file name: both are cut off before anything looks the rest up, or the
      // query rides into the lookup key and `./philosophy?utm=1` is reported
      // dead.
      const base = raw.split('#')[0].split('?')[0]
      if (base === '') continue

      if (base.startsWith(BLOB) || base.startsWith(TREE)) {
        const relpath = decodeHref(
          base.startsWith(BLOB) ? base.slice(BLOB.length) : base.slice(TREE.length),
        ).replace(/\/$/, '')
        // The site SERVES docs/public/ from its root, so a page of the site
        // linking the github copy sends the reader away from a file this very
        // build shipped. It resolves, so it is not broken — it is misrouted,
        // and it means the public/ rule in config.ts stopped firing.
        if (relpath && publicSitePath(relpath)) misroute(base, page)
        else if (relpath && !repoHas(relpath)) flag(base, page)
        continue
      }
      if (NOT_A_PATH.test(base)) continue // another origin, or no location at all

      // Decoded before the climb check, so an escaped `..` is still a climb.
      const target = decodeHref(base)
      let sp
      if (target.startsWith(BASE)) sp = target.slice(BASE.length)
      // `/iterion` is the site root written without its trailing slash, which
      // GitHub Pages answers with a 301 to `/iterion/` — a redirect, not a 404.
      else if (target + '/' === BASE) sp = ''
      else if (target.startsWith('/')) {
        flag(base, page) // absolute without base → points at the domain root → 404
        continue
      } else {
        sp = posixpath.normalize(posixpath.join(pagedir, target))
        if (sp.startsWith('..')) {
          flag(base, page)
          continue
        }
      }
      if (!siteExists(distFiles, sp)) flag(base, page)
    }
  }
  return { broken, misrouted }
}

function main() {
  if (!existsSync(distDir)) {
    console.error('check-links: dist/ not found — run the build first.')
    process.exit(1)
  }
  const distFiles = collectDistFiles(distDir)
  const { broken, misrouted } = brokenLinks({ dist: distDir, distFiles, repoHas: repoResolver(repoRoot) })

  const report = (map, headline) => {
    console.error(`\n❌ ${map.size} ${headline}:`)
    for (const [href, pages] of [...map].sort()) {
      const list = [...pages].sort()
      console.error(`   ${href}  ←  ${list[0]}${list.length > 1 ? ` (+${list.length - 1})` : ''}`)
    }
  }
  if (broken.size) report(broken, 'broken link(s)')
  if (misrouted.size) {
    report(misrouted, `link(s) sent to github.com for a file the site serves from ${PUBLIC_ROOT}/`)
    console.error(`   → ${PUBLIC_ROOT}/ is served from the site root; docs/.vitepress/public-links.mjs maps it.`)
  }
  if (broken.size || misrouted.size) process.exit(1)
  console.log(`links: all internal + github targets resolve ✓ (${distFiles.size} files scanned)`)
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main()
