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
	t = strings.TrimPrefix(t, " ")
	return strings.TrimRight(t, " \t\r"), true
}

// FrontmatterFence is the text of the comment line that opens and closes a
// bot's frontmatter block (`## ---` or `# ---`).
const FrontmatterFence = "---"
