// VitePress serves `docs/public/` from the SITE ROOT: `docs/public/x` is
// `/x`. So the two readers of these docs disagree about how to write a link
// into that tree, and no single spelling used to serve both:
//
//   ](../public/comparatifs/index.html)  correct on github.com, dead on the site
//   ](../comparatifs/index.html)         correct on the site, dead on github.com
//
// The convention is decided here, once: pages are authored to be read on
// github.com, so the SOURCE carries the github-correct form, and the site is
// made correct by construction — this maps the resolved repository path to
// the path the site really serves, and `rewriteHref` emits that instead.
// `docs/scripts/check-links.mjs` then verifies the built result.
//
// Written as .mjs, next to github-slug.mjs and for the same reason: the rule
// is the kind of thing that must be exercised on its own, and this way
// `task docs:links:test` can import it without a TypeScript build.

// The tree VitePress copies to the site root, as a repository path.
export const PUBLIC_ROOT = 'docs/public'

// publicSitePath maps a repository path under docs/public/ to the absolute
// site path that serves it, and returns null for anything else.
//
// The site path is returned WITHOUT `base`: VitePress prepends it to an
// absolute internal link, and a second copy would render `/iterion/iterion/…`.
export function publicSitePath(resolved) {
  if (resolved === PUBLIC_ROOT) return '/'
  // The separator is what makes this a path prefix and not a string prefix:
  // `docs/publications/x` is not inside `docs/public/`.
  if (!resolved.startsWith(PUBLIC_ROOT + '/')) return null
  return '/' + resolved.slice(PUBLIC_ROOT.length + 1)
}
