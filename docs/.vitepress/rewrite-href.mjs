// How a link written for github.com becomes a link the site serves.
//
// These docs are authored to be read on github.com: hundreds of links escape
// /docs (../pkg/*.go, ../README.md), point at raw in-repo source artifacts, or
// reach into docs/public/, which VitePress serves from the SITE ROOT. Each of
// those spellings is correct where it was written and wrong on the other
// reader, so this is where the two are reconciled — once, for every link the
// markdown carries.
//
// Written as .mjs next to github-slug.mjs and public-links.mjs, for the reason
// those are: `task docs:links:test` imports it and asserts the ROUTING itself.
// A rule only reachable through a VitePress build is a rule whose deletion
// nothing notices — measured: with the docs/public/ arm deleted, the site
// still published, because the one link it governs fell through to the
// RAW_SOURCE arm and became a github URL that resolves.
import { existsSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'

import { publicSitePath } from './public-links.mjs'

export const REPO = 'https://github.com/SocialGouv/iterion'
const BLOB = `${REPO}/blob/main`

// Extensions that are source artifacts, not site pages: even when referenced
// with an in-/docs relative path, link them to the file on GitHub.
const RAW_SOURCE = /\.(go|ebnf|ya?ml|sh|json|bot|botz|ts|tsx|mod|sum|toml|proto|csv)$/i

const DOCS_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')

// What a link to an in-docs directory can point at. VitePress builds index.md
// as the directory's own page; a nested README.md it builds as
// `<dir>/README.html`, never as that index (see the srcExclude note below), so
// a README-only directory has its page one name further down. A directory with
// neither has no page at all — those go to the GitHub tree. 'index' is also
// the neutral answer for a path that is missing, unreadable or not a
// directory: it leaves the link to VitePress, as before.
function dirTarget(resolved) {
  const abs = join(DOCS_ROOT, resolved.replace(/^docs\/?/, ''))
  if (!existsSync(abs)) return 'index'
  try {
    // Stat through the link: a symlinked page builds, a directory named
    // index.md does not, and neither does a broken link or a pipe.
    const files = readdirSync(abs).filter((name) => {
      // Per entry: throwIfNoEntry covers a dangling link, not a symlink loop
      // or a refused read, and one bad entry must not decide the directory.
      try {
        return statSync(join(abs, name)).isFile()
      } catch {
        return false
      }
    })
    // Case is kept end to end: VitePress globs `**.md` case-sensitively and
    // builds `Index.md` as `Index.html`, so only a lowercase `index.md` is
    // the directory's own page, and a README is linked under the exact name
    // it carries — `README.MD` builds nothing at all.
    if (files.includes('index.md')) return 'index'
    const readme = files.find((f) => f.endsWith('.md') && /^readme$/i.test(f.slice(0, -3)))
    return readme ? { readme: readme.slice(0, -3) } : 'none'
  } catch {
    return 'index'
  }
}

export function rewriteHref(href, relativePath) {
  // Skip absolute URLs, anchors, and protocol-relative links.
  if (/^([a-z]+:)?\/\//i.test(href) || href.startsWith('#') || href.startsWith('mailto:')) {
    return null
  }
  const [path, hash = ''] = href.split('#')
  const suffix = hash ? '#' + hash : ''
  if (!path) return null

  // The doc's directory relative to the repo root (docs are under docs/).
  const docDir = relativePath ? join('docs', dirname(relativePath)) : 'docs'
  const resolved = normalize(join(docDir, path)).replace(/\\/g, '/')

  // The top-level docs index is excluded from the site (index.md is the home);
  // point in-docs links to it at the site home instead.
  if (resolved === 'docs/README.md') {
    // Internal link: VitePress prepends `base` itself, so omit it here.
    return { href: '/' + suffix, external: false }
  }

  // docs/public/ is served from the site ROOT, so the path the reader gets is
  // not the path the source writes. This is decided before the rules below:
  // where the file is served from is a stronger fact than what its extension
  // is, and routing a `.csv` the site itself ships to github.com would make
  // the site link away from an asset it serves.
  const fromPublic = publicSitePath(resolved)
  if (fromPublic) {
    return { href: `${fromPublic}${suffix}`, external: false }
  }

  const escapesDocs = !resolved.startsWith('docs/') && resolved !== 'docs'
  const isRawInDocs = resolved.startsWith('docs/') && RAW_SOURCE.test(path)

  if (escapesDocs || isRawInDocs) {
    return { href: `${BLOB}/${resolved}${suffix}`, external: true }
  }
  // A trailing slash is what names a directory. Keep the reader on the site
  // where a page exists, and send the rest to the GitHub tree so the link
  // still resolves.
  if (/\/$/.test(path)) {
    const target = dirTarget(resolved)
    if (typeof target === 'object') {
      return { href: `${path}${target.readme}${suffix}`, external: false }
    }
    if (target === 'none') {
      return { href: `${REPO}/tree/main/${resolved}${suffix}`, external: true }
    }
  }
  return null
}
