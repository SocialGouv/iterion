// Fail the build if any link on the site would 404.
//
// VitePress's own dead-link check covers doc-to-doc markdown links, but NOT the
// links defined in config (sidebar/nav/home `link:`) nor the code-links this
// site rewrites to github.com. This audits the BUILT site deterministically:
//   - internal links resolve to a built page in dist/
//   - github blob/tree links to this repo resolve to a real path in the tree
// Run as the postbuild step so "no link 404s" stays enforced, not one-off.
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import posixpath from 'node:path/posix'

const docsRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
const repoRoot = join(docsRoot, '..')
const dist = join(docsRoot, '.vitepress', 'dist')

const BLOB = 'https://github.com/SocialGouv/iterion/blob/main/'
const TREE = 'https://github.com/SocialGouv/iterion/tree/main/'
const BASE = '/iterion/'

if (!existsSync(dist)) {
  console.error('check-links: dist/ not found — run the build first.')
  process.exit(1)
}

// Set of dist-relative file paths that exist (the built site).
const distFiles = new Set()
;(function walk(d) {
  for (const e of readdirSync(d)) {
    const p = join(d, e)
    statSync(p).isDirectory() ? walk(p) : distFiles.add(p.slice(dist.length + 1).replace(/\\/g, '/'))
  }
})(dist)

const siteExists = (sitePath) => {
  const p = sitePath
  if (p === '' || p.endsWith('/')) return distFiles.has(p + 'index.html')
  if (/\.[a-z0-9]+$/i.test(p)) return distFiles.has(p) // asset with extension (css/js/png/ico…)
  return distFiles.has(p + '.html') || distFiles.has(p + '/index.html')
}

const hrefRe = /href="([^"]+)"/g
const broken = new Map() // href -> Set(pages)
const flag = (href, page) => {
  if (!broken.has(href)) broken.set(href, new Set())
  broken.get(href).add(page)
}

for (const rel of distFiles) {
  if (!rel.endsWith('.html')) continue
  const page = rel
  const pagedir = posixpath.dirname(page)
  const html = readFileSync(join(dist, rel), 'utf8')
  let m
  while ((m = hrefRe.exec(html))) {
    const raw = m[1]
    const base = raw.split('#')[0]
    if (base === '' || raw.startsWith('#') || raw.startsWith('mailto:')) continue

    if (base.startsWith(BLOB) || base.startsWith(TREE)) {
      const relpath = (base.startsWith(BLOB) ? base.slice(BLOB.length) : base.slice(TREE.length)).replace(/\/$/, '')
      if (relpath && !existsSync(join(repoRoot, relpath))) flag(base, page)
      continue
    }
    if (base.startsWith('http://') || base.startsWith('https://')) continue // other external

    let sp
    if (base.startsWith(BASE)) sp = base.slice(BASE.length)
    else if (base.startsWith('/')) {
      flag(base, page) // absolute without base → points at the domain root → 404
      continue
    } else {
      sp = posixpath.normalize(posixpath.join(pagedir, base))
      if (sp.startsWith('..')) {
        flag(base, page)
        continue
      }
    }
    if (!siteExists(sp)) flag(base, page)
  }
}

if (broken.size) {
  console.error(`\n❌ ${broken.size} broken link(s):`)
  for (const [href, pages] of [...broken].sort()) {
    const list = [...pages].sort()
    console.error(`   ${href}  ←  ${list[0]}${list.length > 1 ? ` (+${list.length - 1})` : ''}`)
  }
  process.exit(1)
}
console.log(`links: all internal + github targets resolve ✓ (${distFiles.size} files scanned)`)
