// Validate every ```mermaid block in the docs parses cleanly.
//
// VitePress renders mermaid on the client, so a diagram with broken syntax
// (a common LLM failure mode — missing quotes, stray punctuation) builds fine
// and only fails in the reader's browser. This runs mermaid's own parser
// headlessly (jsdom provides the DOM mermaid's DOMPurify needs) so a broken
// diagram fails the build instead. Wired as the `build` prebuild step.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { JSDOM } from 'jsdom'

const dom = new JSDOM('<!DOCTYPE html><body></body>', { pretendToBeVisual: true })
globalThis.window = dom.window
globalThis.document = dom.window.document
globalThis.Element = dom.window.Element
globalThis.SVGElement = dom.window.SVGElement
const { default: mermaid } = await import('mermaid')

const docsRoot = join(dirname(fileURLToPath(import.meta.url)), '..')

const files = []
;(function walk(d) {
  for (const e of readdirSync(d)) {
    const p = join(d, e)
    const s = statSync(p)
    if (s.isDirectory()) {
      if (!/node_modules|\.vitepress\/(dist|cache)|\/scripts$/.test(p)) walk(p)
    } else if (p.endsWith('.md')) {
      files.push(p)
    }
  }
})(docsRoot)

const fence = /```mermaid\n([\s\S]*?)```/g
let total = 0
const broken = []
for (const f of files.sort()) {
  const src = readFileSync(f, 'utf8')
  let m
  let idx = 0
  while ((m = fence.exec(src))) {
    idx++
    total++
    const line = src.slice(0, m.index).split('\n').length
    const rel = f.slice(docsRoot.length + 1)
    try {
      await mermaid.parse(m[1])
    } catch (e) {
      broken.push({ rel, line, idx, msg: String(e?.message || e) })
    }
  }
}

for (const b of broken) {
  console.error(`\n❌ ${b.rel}:${b.line} (mermaid #${b.idx})`)
  console.error('   ' + b.msg.split('\n').slice(0, 4).join('\n   '))
}
console.log(
  `\nmermaid: ${total} diagram(s) checked, ${broken.length} broken${broken.length ? '' : ' ✓'}`,
)
process.exit(broken.length ? 1 : 0)
