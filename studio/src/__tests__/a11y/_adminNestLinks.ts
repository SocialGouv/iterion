// The detector behind source-discipline's "no absolute path literal under
// views/admin" guard, as a function so its DOMAIN is testable: the guard runs
// it over the real tree, adminNestLinks.test.ts runs it over hand-written
// sources that spell the bypass. NOT a test file — imported by both.

/** The two route tables are the licensed homes of a console path. */
const TABLES = /\/views\/admin\/(?:adminRoutes\.ts|AdminConsoleRoutes\.tsx)$/;
/**
 * A quoted path whose FIRST segment is there: spelled out (`"/orgs…`) or
 * interpolated (`` `/${orgId}… ``, the shape a computed target takes). A bare
 * `"/"` is not one — that literal is the separator a relative path is joined
 * with, which is what every occurrence of it in the studio does today.
 */
const ABSOLUTE_LITERAL = /["'\x60]\/[A-Za-z0-9_-]|`\/\$\{/;

const BLANK = (m: string) => m.replace(/[^\n]/g, " ");

// String literals FIRST, comments second, in one pass: `//` and `/*` inside a
// string are string content, and a comment strip that does not know that
// blanks the rest of the line — href included. Quoted strings are bounded to
// their line (`[^\\\n]`) so an odd quote inside a regex literal cannot swallow
// the file; template literals keep their newlines. The failure direction is
// deliberate: a mis-tokenized line leaves a comment UNBLANKED, so the guard
// reports prose a human then rewords — never the reverse.
const STRING_OR_COMMENT =
  /(["'])(?:\\.|(?!\1)[^\\\n])*\1?|`(?:\\.|[^\\`])*`?|\/\*[\s\S]*?\*\/|\/\/[^\n]*/g;

/** Blank comments, so a quoted path in prose is not read as a target. Blanked
 *  rather than dropped so reported line numbers stay the file's own. */
function blankComments(src: string): string {
  return src.replace(STRING_OR_COMMENT, (m) => (/^["'\x60]/.test(m) ? m : BLANK(m)));
}

// studioRoute()'s own argument, and nothing else on the line: the escape is
// what leaves the nest, not a licence for every target sharing its line.
const ESCAPED_ARGUMENT = /\bstudioRoute\([^)\n]*\)?/g;

/**
 * absoluteTargetHits reports, as `path:line  excerpt`, every line under
 * views/admin/ that writes an absolute path literal — a target wouter resolves
 * against the console's nested base, so "/admin/users" becomes
 * /admin/admin/users and "/orgs/<id>" becomes /admin/orgs/<id>.
 *
 * It keys on the LITERAL, not on the attribute or call that consumes it. An
 * absolute target reaches wouter through many spellings — `href='/x'`,
 * `href={'/x' + id}`, `` href={`/${kind}/${id}`} ``, a const, a table of
 * `{ value: "/x" }` fed to navigate() (which is how the console once became
 * unreachable) — and a detector that enumerated them would always be one
 * spelling short.
 *
 * Two limits, each with a case in adminNestLinks.test.ts rather than a claim:
 * a target assembled across LINES is not seen, and neither is one whose
 * leading slash is its own literal (`"/" + kind + "/" + id`) — refusing that
 * spelling would refuse the join it is in every other use.
 */
export function absoluteTargetHits(files: Iterable<readonly [string, string]>): string[] {
  const hits: string[] = [];
  for (const [path, src] of files) {
    if (!path.includes("/views/admin/") || TABLES.test(path)) continue;
    blankComments(src)
      .split("\n")
      .forEach((line, i) => {
        if (!ABSOLUTE_LITERAL.test(line.replace(ESCAPED_ARGUMENT, BLANK))) return;
        hits.push(`${path}:${i + 1}  ${line.trim().slice(0, 100)}`);
      });
  }
  return hits;
}
