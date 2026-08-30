// Shared changelog rendering, imported by BOTH consumers so a regenerated
// section is byte-identical to one produced at release time:
//   - `.release-it.mjs`         → every release prepends one section
//   - `scripts/changelog-gen.mjs` → backfill / re-split of the whole history
//
// The preamble lives in HEADER, not in the file body: the release-it plugin
// strips the configured header with a literal String.replace and re-emits it
// on top, so anything else at the top of CHANGELOG.md would be pushed BELOW
// the newest release at the next run.
import { execFileSync } from 'node:child_process'
import createPreset from 'conventional-changelog-conventionalcommits'

// Absolute, because this header is read from three places with different link
// bases: the repo-root file, the /changelog page of the docs site, and the
// GitHub release notes (where relative links do not resolve at all).
export const HEADER = `# Changelog

Generated from Conventional Commits at each release. Older majors are archived
under [docs/changelog/](https://github.com/SocialGouv/iterion/tree/main/docs/changelog).`

// Longest "why" excerpt kept under a release entry, in characters.
const MAX_PROSE = 500

// Trailer lines the parser leaves in the body (it routes most of them to
// `footer`, but a squashed PR can carry them mid-body).
const TRAILER =
  /^(co-authored-by|signed-off-by|generated with|updated-dependencies|release-as|refs|closes|fixes)\b/i

// A paragraph that is a bullet list: the per-commit summary GitHub writes into
// a squashed PR body (32% of v3 entries). It restates the subjects we already
// render, so skip past it to the first real prose paragraph.
const BULLETS = /^\s*[-*]\s+\S/

function isProse(paragraph) {
  const lines = paragraph.split('\n').map(l => l.trim()).filter(Boolean)
  if (!lines.length) return false
  if (BULLETS.test(lines[0])) return false
  // The `---------` rule GitHub appends under a squashed PR's bullet list:
  // a paragraph carrying no letters or digits says nothing.
  if (!/[a-zA-Z0-9]/.test(paragraph)) return false
  return lines.some(l => !TRAILER.test(l))
}

function truncate(text) {
  if (text.length <= MAX_PROSE) return text
  const cut = text.slice(0, MAX_PROSE)
  const boundary = cut.lastIndexOf(' ')
  return `${(boundary > MAX_PROSE * 0.6 ? cut.slice(0, boundary) : cut).trimEnd()}…`
}

// `commit.body` is NOT the commit's body: conventional-commits-parser cuts
// body from footer at the first line matching `^(?:BREAKING CHANGE|[\w-]+):\s`
// and routes everything from there on into `footer`. That shape is hardcoded
// (getFooterTokenRegex, conventional-commits-parser@7 dist/regex.js) — no
// parser option narrows it. Bodies here are hard-wrapped at ~72 columns, so a
// continuation line opening `word: ` reads as a git trailer and truncates the
// paragraph mid-sentence.
//
// Rejoining body + footer cannot recover it: the blank line that would say
// whether the split was a real paragraph break is consumed either way. So read
// the message from git, keyed by hash — the exact source, once per process.
let bodies = null

function bodyOf(commit) {
  const hash = typeof commit?.hash === 'string' ? commit.hash : ''
  if (!hash) return ''

  if (!bodies) {
    // Unit separator between hash and body, record separator between commits:
    // neither occurs in a commit message.
    const log = execFileSync('git', ['log', '--format=%H%x1f%b%x1e'], {
      encoding: 'utf8',
      maxBuffer: 1 << 28
    })
    bodies = new Map()
    for (const record of log.split('\x1e')) {
      const sep = record.indexOf('\x1f')
      if (sep !== -1) bodies.set(record.slice(0, sep).trim(), record.slice(sep + 1))
    }
  }

  // A hash outside the log is a shallow clone, not a failure: fall back to the
  // parser's narrower view, which is what this used to read anyway.
  return bodies.get(hash) ?? (typeof commit.body === 'string' ? commit.body : '')
}

/**
 * First paragraph of real prose from a commit body — the "why" the author
 * wrote. Returns '' when the commit has nothing but a subject.
 */
export function firstProse(commit) {
  const body = bodyOf(commit)
  if (!body.trim()) return ''

  const paragraph = body.split(/\n\s*\n/).find(isProse)
  if (!paragraph) return ''

  const text = paragraph
    .split('\n')
    .map(l => l.trim())
    .filter(l => l && !TRAILER.test(l))
    .join(' ')
    .replace(/\s+/g, ' ')
    .trim()

  return text ? truncate(text) : ''
}

const preset = createPreset()
const basePartial = preset.writer.commitPartial

// The preset's own partial renders the canonical entry (bold scope, issue and
// commit links, closes/references). We call it rather than re-implement it,
// and append the collapsed excerpt. The blank line and 2-space indent keep the
// <details> inside the list item the writer wraps this in.
function commitPartial(context, commit) {
  const entry = basePartial(context, commit)
  const why = firstProse(commit)
  if (!why) return entry

  return `${entry}\n\n  <details><summary>why</summary>\n\n  ${why}\n\n  </details>\n`
}

/** writerOpts enabling the "why" excerpts. Omit for plain one-line entries. */
export const writerOpts = { commitPartial }
