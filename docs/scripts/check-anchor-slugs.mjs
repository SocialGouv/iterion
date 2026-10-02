// Hold the site's heading slugifier to the anchors GitHub generates.
//
// internal/docsguard/testdata/github-anchors/*.tsv are `raw heading<TAB>anchor`
// rows copied from pages GitHub rendered (see the Go test for the
// provenance). The Go checker is held to them by `go test
// ./internal/docsguard`; this script holds docs/.vitepress/github-slug.mjs —
// the copy VitePress runs — to the same rows, including the `-1`, `-2`
// numbering markdown-it-anchor applies to a repeated heading. Dependency-free
// on purpose: it runs wherever `node` does, without the docs' node_modules.
import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { githubSlug } from '../.vitepress/github-slug.mjs'

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..', '..')
const fixtures = join(repoRoot, 'internal', 'docsguard', 'testdata', 'github-anchors')

// The text markdown-it hands the slugifier: the heading's text and code
// tokens — a code span's content, a link's text, no image, no HTML tag,
// entities decoded, emphasis unwrapped.
const headingText = (raw) =>
  raw
    .replace(/^\s*(?:(?:>|[-*+]|\d+[.)])\s+)*#{1,6}\s+/, '')
    .replace(/\s+#+\s*$/, '')
    .replace(/!\[[^\]]*\]\([^)]*\)/g, '')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\[[^\]]*\]/g, '$1')
    .replace(/`+([^`]*)`+/g, (_, code) => code.replace(/</g, '&lt;').replace(/>/g, '&gt;'))
    .replace(/<[^>]+>/g, '')
    .replace(/&amp;/g, '&')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/(^|[\s(])_{1,2}([^_\s][^_]*?[^_\s]|[^_\s])_{1,2}(?=$|[\s).,;:!?])/g, '$1$2')
    .trim()

// markdown-it-anchor's uniqueSlug: `slug`, then `slug-1`, `slug-2`, …
const numbered = () => {
  const seen = new Map()
  return (slug) => {
    const n = seen.get(slug) ?? 0
    seen.set(slug, n + 1)
    return n === 0 ? slug : `${slug}-${n}`
  }
}

let rows = 0
let failures = 0
for (const name of readdirSync(fixtures).filter((f) => f.endsWith('.tsv')).sort()) {
  const unique = numbered()
  for (const line of readFileSync(join(fixtures, name), 'utf8').split('\n')) {
    if (!line) continue
    const tab = line.indexOf('\t')
    const raw = line.slice(0, tab)
    const want = line.slice(tab + 1)
    const got = unique(githubSlug(headingText(raw)))
    rows++
    if (got !== want) {
      failures++
      console.error(`${name}: ${JSON.stringify(raw)}\n  want ${want}\n  got  ${got}`)
    }
  }
}
if (rows === 0) {
  console.error('check-anchor-slugs: no fixture row read — the check would pass on anything')
  process.exit(1)
}
if (failures) {
  console.error(`check-anchor-slugs: ${failures} of ${rows} GitHub anchors not reproduced by docs/.vitepress/github-slug.mjs`)
  process.exit(1)
}
console.log(`check-anchor-slugs: the site's slugifier reproduces all ${rows} GitHub anchors ✓`)
