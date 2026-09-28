package author

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The document-level notice of #1663. yaml.v3's scanner ends a line at CR,
// NEL (U+0085), LS (U+2028) and PS (U+2029), not only at LF. Inside a block
// scalar the converter reads an LS/PS as the newline the scanner meant
// (E053, spell.go), but a trailing line made only of such a character is a
// trailing blank line to the scanner, and clip or strip chomping removes it
// before the converter sees the value — no diagnostic, no trace. An author
// told to "remove the invisible character" at line N may leave a second one
// on a trailing line and never hear of it again.
//
// So the RAW source is scanned once, before any reading of it, and one
// warning per document lists the physical line of every NEL, LS and PS it
// holds — no need to know any scalar's extent. A line is counted where the
// scanner ends one (yamlBreaks), so the line named is the one every other
// diagnostic of the document counts by.
func separatorNotice(name string, src []byte) *parser.Diagnostic {
	// The BOM is the scanner's to skip, and it is not a separator.
	s := strings.TrimPrefix(string(src), "\ufeff")
	var lines []int
	line := 1
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\r':
			if i+size < len(s) && s[i+size] == '\n' {
				i += size // CRLF is one break
			}
			line++
		case '\n':
			line++
		case 0x85, 0x2028, 0x2029:
			lines = append(lines, line)
			line++
		}
		i += size
	}
	if len(lines) == 0 {
		return nil
	}
	sep := "an invisible line separator"
	where := "line " + strconv.Itoa(lines[0])
	if len(lines) > 1 {
		sep = strconv.Itoa(len(lines)) + " invisible line separators"
		parts := make([]string, len(lines))
		for i, l := range lines {
			parts[i] = strconv.Itoa(l)
		}
		where = "lines " + strings.Join(parts, ", ")
	}
	d := docDiag(name, lines[0], 1, "the document holds "+sep+" (U+0085 NEL, U+2028 LS, U+2029 PS — "+where+"): yaml.v3 ends a line at each; inside a block body it is read as a newline, on a trailing line it is chomped like a blank line")
	d.Code = parser.DiagAuthorSeparators
	d.Severity = parser.SeverityWarning
	d.Hint = parser.HintFor(parser.DiagAuthorSeparators)
	return &d
}
