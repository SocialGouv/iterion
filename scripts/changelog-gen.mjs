#!/usr/bin/env node
// Regenerate CHANGELOG.md (current major, with "why" excerpts) and the
// per-major archives under docs/changelog/, from git history alone.
//
// Idempotent and re-runnable: this is both the one-time backfill AND the
// re-split to run after a major bump, or when the size warning below fires.
// Between those, release-it maintains CHANGELOG.md on its own.
import { mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { ConventionalChangelog } from 'conventional-changelog'
import { HEADER, writerOpts } from './changelog-writer.mjs'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const ARCHIVE_DIR = join(ROOT, 'docs', 'changelog')
const PRESET = 'conventionalcommits'

// GitHub stops rendering markdown past 512 KiB. Warn with enough margin that
// there is time to bump the major (or hand-split) before a release trips it.
const SIZE_WARN = 450 * 1024

// Every release heading the preset emits is `## [x.y.z](compare-url) (date)`
// — verified across the full history, no `#`/`###` variants — so this split
// is unambiguous.
const SECTION = /(?=^## \[)/m

const RELEASES_URL = 'https://github.com/SocialGouv/iterion/releases'

async function generate(opts) {
  const gen = new ConventionalChangelog(ROOT)
  gen.loadPreset(PRESET)
  gen.readRepository()
  gen.options({ releaseCount: 0 })
  if (opts?.writer) gen.writer(opts.writer)

  let out = ''
  for await (const chunk of gen.write()) out += chunk
  return out
}

// Split into release sections, dropping the phantom leading section the
// generator emits when HEAD is past the newest tag (the usual case: the
// `chore(brew)` commit that follows every release).
function sections(changelog) {
  return changelog
    .split(SECTION)
    .map(s => s.trim())
    .filter(s => s.startsWith('## [') && !s.startsWith('## [undefined]'))
}

function versionOf(section) {
  const m = section.match(/^## \[(\d+\.\d+\.\d+[^\]]*)\]/)
  if (!m) throw new Error(`section without a parsable version: ${section.slice(0, 80)}`)
  return m[1]
}

function majorOf(section) {
  return Number(versionOf(section).split('.')[0])
}

function groupByMajor(list) {
  const byMajor = new Map()
  for (const section of list) {
    const major = majorOf(section)
    if (!byMajor.has(major)) byMajor.set(major, [])
    byMajor.get(major).push(section)
  }
  return byMajor
}

function archiveDoc(major, list) {
  // Sections run newest-first, so the last one is the oldest release this
  // generator can reach. On v0 that is NOT v0.1.0: the tags before it point at
  // a history that was rewritten away, and saying so beats leaving a gap the
  // reader has to explain to themselves.
  const oldest = versionOf(list[list.length - 1])
  const gap =
    major === 0
      ? `
Releases before \`v${oldest}\` are not listed: their commits are no longer
reachable from \`main\` (the history was rewritten). They remain on the
[GitHub releases page](${RELEASES_URL}).
`
      : ''

  return `# Changelog — v${major}.x

Archived releases. The current major is in [the changelog](../changelog).
${gap}
${list.join('\n\n')}
`
}

async function main() {
  const pkg = JSON.parse(await readFile(join(ROOT, 'package.json'), 'utf8'))
  const currentMajor = Number(pkg.version.split('.')[0])

  // Two passes: the current major carries the "why" excerpts, the archives
  // stay one-liners (an enriched v0 would blow past GitHub's render ceiling).
  const [enriched, plain] = await Promise.all([
    generate({ writer: writerOpts }),
    generate()
  ])

  const enrichedByMajor = groupByMajor(sections(enriched))
  const plainByMajor = groupByMajor(sections(plain))

  const current = enrichedByMajor.get(currentMajor)
  if (!current?.length) throw new Error(`no release section found for major v${currentMajor}`)

  const changelog = `${HEADER}\n\n${current.join('\n\n')}\n`
  await writeFile(join(ROOT, 'CHANGELOG.md'), changelog)

  // Clear only the archives this script owns, so a major that stops existing
  // (re-split) leaves no stale page behind — and anything else filed here by
  // hand survives.
  await mkdir(ARCHIVE_DIR, { recursive: true })
  for (const entry of await readdir(ARCHIVE_DIR)) {
    if (/^v\d+\.md$/.test(entry)) await rm(join(ARCHIVE_DIR, entry))
  }

  // Everything that is not the current major is archived — `!==`, not `<`, so
  // a checkout whose package.json TRAILS the newest tag (a branch forked
  // before a release, a maintenance branch, a stale worktree) still writes
  // those sections somewhere instead of dropping them on the floor with a
  // zero exit code and a `total` below that contradicts it.
  const archived = [...plainByMajor.keys()].filter(m => m !== currentMajor).sort((a, b) => b - a)
  for (const major of archived) {
    await writeFile(join(ARCHIVE_DIR, `v${major}.md`), archiveDoc(major, plainByMajor.get(major)))
  }

  const size = Buffer.byteLength(changelog)
  const total = [...plainByMajor.values()].reduce((n, l) => n + l.length, 0)
  console.log(
    `CHANGELOG.md: v${currentMajor}.x, ${current.length} releases, ${(size / 1024).toFixed(0)} KB`
  )
  for (const major of archived) {
    console.log(`docs/changelog/v${major}.md: ${plainByMajor.get(major).length} releases`)
  }
  console.log(`total: ${total} releases`)

  if (size > SIZE_WARN) {
    console.warn(
      `\nWARNING: CHANGELOG.md is ${(size / 1024).toFixed(0)} KB, past the ${SIZE_WARN / 1024} KB` +
        ` mark. GitHub stops rendering markdown at 512 KB — bump the major and re-run this script.`
    )
  }
}

await main()
