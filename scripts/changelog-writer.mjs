// Shared changelog rendering, imported by BOTH consumers so a regenerated
// section is byte-identical to one produced at release time:
//   - `.release-it.mjs`         → every release prepends one section
//   - `scripts/changelog-gen.mjs` → backfill / re-split of the whole history
//
// The preamble lives in HEADER, not in the file body: the release-it plugin
// strips the configured header with a literal String.replace and re-emits it
// on top, so anything else at the top of CHANGELOG.md would be pushed BELOW
// the newest release at the next run.
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
  return lines.some(l => !TRAILER.test(l))
}

function truncate(text) {
  if (text.length <= MAX_PROSE) return text
  const cut = text.slice(0, MAX_PROSE)
  const boundary = cut.lastIndexOf(' ')
  return `${(boundary > MAX_PROSE * 0.6 ? cut.slice(0, boundary) : cut).trimEnd()}…`
}

/**
 * First paragraph of real prose from a commit body — the "why" the author
 * wrote. Returns '' when the commit has nothing but a subject.
 */
export function firstProse(commit) {
  const body = typeof commit?.body === 'string' ? commit.body : ''
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
