// Package workflowfile is the single source of truth for which file
// extensions iterion recognises as workflow source files. Plain workflow
// sources use `.bot`; packaged workflows use `.botz` through the bundle
// loader.
package workflowfile

import "strings"

// Extensions lists the accepted workflow file suffixes, including the
// leading dot. Order is informational only.
var Extensions = []string{".bot"}

// IsWorkflowFile reports whether path ends with one of the accepted
// workflow file extensions.
func IsWorkflowFile(path string) bool {
	for _, ext := range Extensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// AuthorExtension is the suffix of an author document — the YAML twin a
// `.bot` can be written as (pkg/dsl/author) — a way of WRITING a workflow
// file, never one. It is deliberately not an entry of Extensions:
// IsWorkflowFile stays false for it, so no surface that launches, lists,
// packs or stores a workflow takes a draft for the truth; the readers that
// accept one (validate, fmt, diagram, the MCP validate tool) ask
// IsAuthorDocument by name, and every launcher refuses it by name.
const AuthorExtension = ".bot.yaml"

// IsAuthorDocument reports whether path names an author document. The
// suffix is matched case-folded, as bundle.Detect folds `.BOT` and
// `.BOTZ`: every door asks this one predicate on the path as written,
// so a `DRAFT.BOT.YAML` is refused at the first door rather than read as
// a parse error at the last.
func IsAuthorDocument(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), AuthorExtension)
}

// CommentText reports whether line is a comment line of a workflow source —
// a `#` or `##` after optional indentation — and returns its text with the
// hashes and one following space removed (`## note` and `# note` both yield
// "note"; `##   - item` keeps its inner indentation as "  - item").
//
// It is the ONE definition of what a comment line looks like, shared by the
// lexer's directive pre-scan, the bundle frontmatter reader and the
// catalogue's description reader: a hash count the parser accepts must be
// accepted by every reader of the same file, or a valid `.bot` silently loses
// its strict-escape mode or its catalogue identity to a comment written the
// other way.
func CommentText(line string) (text string, ok bool) {
	// Only spaces may precede the hash: the lexer refuses a tab as
	// indentation, and a reader that accepted one would give an
	// unparseable file a catalogue identity.
	t := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(t, "#") {
		return "", false
	}
	t = strings.TrimPrefix(t, "#")
	t = strings.TrimPrefix(t, "#")
	return CommentBody(t), true
}

// CommentBody is a comment's text once its hashes are off: one following
// space removed — the one the writer puts back — and nothing else, so the
// indentation an author wrote INSIDE the comment (`##   - item`, a wrapped
// line aligned under a bullet) is part of the text and survives a rewrite.
// The lexer reads the same rule off this function, which is what keeps the
// two from drifting.
func CommentBody(afterHashes string) string {
	return strings.TrimRight(strings.TrimPrefix(afterHashes, " "), " \t\r")
}

// SkipWalkDir reports a directory a walk over a source tree does not
// descend into: hidden trees hold other checkouts (`.claude/worktrees`,
// `.works`, `.repos`), the run store and the VCS; `vendor` and
// `node_modules` hold someone else's sources. A `.bot` under one of them is
// reached only by naming it as a path.
//
// It is the ONE definition of that rule, shared by every walk: `iterion
// fmt`'s collector and the guards that check the same tree have to
// enumerate the same files, or a `.bot` one sees and the other does not
// puts them in a disagreement no list can settle.
func SkipWalkDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules"
}

// FrontmatterFence is the text of the comment line that opens and closes a
// bot's frontmatter block (`## ---` or `# ---`).
const FrontmatterFence = "---"
