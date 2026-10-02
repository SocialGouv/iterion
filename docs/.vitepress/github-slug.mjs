// The anchor GitHub generates for a heading (html-pipeline's
// TableOfContentsFilter): lowercase, drop every character that is not a
// letter, a mark, a decimal digit, connector punctuation, a space or a
// hyphen, then one hyphen per space. Runs of spaces are kept, so
// `Backend parity — claw ↔ claude_code` slugs to
// `backend-parity--claw--claude_code`, and a leading emoji leaves a leading
// hyphen.
//
// The site uses it in place of VitePress's default slugifier (which
// collapses hyphens, turns `_` into `-` and keeps emoji) so a `#fragment`
// that resolves on github.com resolves on the published page too — one
// dialect for the docs, checked by `task docs:links`. The reference
// implementation and the GitHub-verified fixtures are in internal/docsguard;
// docs/scripts/check-anchor-slugs.mjs holds this copy to the same rows.
export function githubSlug(text) {
  return text
    .toLowerCase()
    .replace(/[^\p{L}\p{M}\p{Nd}\p{Pc} -]/gu, '')
    .replace(/ /g, '-')
}
