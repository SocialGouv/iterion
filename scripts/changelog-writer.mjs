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
//
// Matching the trailer FORM, not the bare keyword, is load-bearing: this test
// runs against every line of the chosen paragraph, so `^closes\b` also deleted
// hard-wrapped prose opening on the English word ("Closes the review→fix loop
// with no human in it — for repos that opt in, and"), decapitating 79 lines
// across this history. Hence the mandatory `:` or issue-ref anchor. The
// attribution line carries an emoji the old `^generated with` never reached,
// so its prefix is spelled out — restricted to non-letters so it cannot eat a
// sentence starting "Regenerated with [...]".
const TRAILER =
  /^(?:(?:co-authored-by|signed-off-by|updated-dependencies|release-as|refs|closes|fixes)\s*:|(?:refs|closes|fixes)\s+(?:#|https?:\/\/)|[^a-z0-9]{0,4}generated with \[)/i

// A paragraph that is a bullet list: the per-commit summary GitHub writes into
// a squashed PR body (32% of v3 entries). It restates the subjects we already
// render, so skip past it to the first real prose paragraph.
const BULLETS = /^\s*[-*]\s+\S/

// A paragraph carries an excerpt only if something READABLE survives the
// trailer filter. Without the alphanumeric test, the `---------` rule GitHub
// writes above the trailers of a squashed PR is neither a bullet nor a
// trailer, so it qualified as prose and rendered as a bare <hr> inside the
// <details>.
function isProse(paragraph) {
  const lines = paragraph.split('\n').map(l => l.trim()).filter(Boolean)
  if (!lines.length) return false
  if (BULLETS.test(lines[0])) return false
  return lines.some(l => !TRAILER.test(l) && /[a-z0-9]/i.test(l))
}

// Commit bodies use `<placeholder>` notation freely, and an excerpt is emitted
// as markdown. A BARE `<version>` parses as an HTML tag, so GitHub's sanitizer
// drops it and the word disappears from the rendered CHANGELOG.md — leaving
// "iterion-sandbox-slim:, a tag nobody pushed" and "~/.claude/projects//memory/"
// (14 excerpts at the time of writing).
//
// Backslash-escaping is CommonMark, so GitHub and VitePress both render the
// literal text. Only `<` is escaped: nothing can open a tag without it, so the
// closing `>` needs no help — and leaving it alone keeps `->` and `>=`, which
// are all over these bodies, out of the diff. Inside an inline-code span the
// character is already literal and a backslash would SHOW, hence the code-span
// alternative that returns its match untouched.
function escapeAngles(text) {
  return text.replace(/(`[^`]*`)|</g, (m, code) => code || '\\<')
}

function truncate(text) {
  if (text.length <= MAX_PROSE) return text
  const cut = text.slice(0, MAX_PROSE)
  const boundary = cut.lastIndexOf(' ')
  const kept = (boundary > MAX_PROSE * 0.6 ? cut.slice(0, boundary) : cut).trimEnd()
  // A hard cut can land between a backslash and the character it escapes; a
  // dangling `\` would render as a literal backslash.
  return `${kept.replace(/\\$/, '')}…`
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

  // Escape before truncating, so code-span detection sees the whole string
  // rather than a span the cut left unterminated.
  return text ? truncate(escapeAngles(text)) : ''
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

/**
 * writerOpts enabling the "why" excerpts. Omit for plain one-line entries.
 *
 * Both consumers merge this over the preset's own writer options — a SHALLOW
 * per-key spread (`{...preset.writer, ...ours}` in conventional-changelog's
 * ConventionalChangelog.js), applied after loadPreset. So `commitPartial`
 * replaces only itself and `groupBy` / `transform` / `headerPartial` /
 * `commitGroupsSort` all survive, which is what keeps the `### Features` /
 * `### Bug Fixes` headings on released sections.
 *
 * The shallowness is the trap: adding `transform` here would REPLACE the
 * preset's type→section mapping wholesale rather than extend it, and the
 * breakage would first appear in a real release.
 */
export const writerOpts = { commitPartial }
