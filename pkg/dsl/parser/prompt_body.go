package parser

import (
	"fmt"
	"strings"
)

// CanonicalPromptBody is the body the profile-1 lexer settles on for a
// prompt written with body under its header — whatever the author, or a
// canvas textarea, put there. It is the one definition of what the v1
// syntax carries in a prompt body, read by the writer (which writes this
// form) and by the save guard (which compares against it), and pinned to
// the lexer itself by TestCanonicalPromptBodyIsWhereTheLexerSettles.
// CanonicalPromptBodyIn is the same definition for a given profile.
//
// The facts of the v1 lexer it restates:
//   - a CR before a newline is folded away with it (CRLF becomes LF before
//     anything is read), so a line loses its trailing CRs — every one of
//     them: the fold is one pass, and a second round-trip would take the
//     next, so the settled form has none;
//   - a line that is empty or holds only spaces is skipped wherever it
//     sits — before the first line, between two paragraphs, after the
//     last — so a paragraph break does not survive and a body never ends
//     with a newline (1 707 blank lines sit inside the shipped bots'
//     prompts; the model receives each as a single newline);
//   - the first kept line sets the body's indentation, and that many
//     leading spaces come off every line.
//
// Every other byte of a line — its own deeper indentation, trailing spaces,
// tabs, a CR inside the line — is kept.
//
// A body whose first line is indented deeper than a later one has no
// written form at all: the lexer ends the body at the shallower line.
// CanonicalPromptBody does not check for it — it would de-indent the body,
// a change of program — CheckPromptBody names it, and the guard refuses it.
func CanonicalPromptBody(body string) string {
	kept, base := promptBodyLines(body)
	lines := make([]string, len(kept))
	for i, k := range kept {
		lines[i] = stripSpaces(k.text, base)
	}
	return strings.Join(lines, "\n")
}

// CanonicalPromptBodyIn is CanonicalPromptBody for a syntax profile. The
// profiles differ in exactly one fact: from profile 2 a blank or
// space-only line BETWEEN two kept lines survives as an empty line — the
// paragraph break reaches the model — while leading and trailing blank
// lines are still dropped, a body still never ends with a newline, and the
// first kept line still sets the indentation. Pinned to the profile-2
// lexer by TestCanonicalPromptBodyInProfileTwoIsWhereTheLexerSettles.
func CanonicalPromptBodyIn(profile int, body string) string {
	if profile < 2 {
		return CanonicalPromptBody(body)
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.Trim(line, " ") == "" {
			line = ""
		}
		lines[i] = line
	}
	start, end := 0, len(lines)
	for start < end && lines[start] == "" {
		start++
	}
	for end > start && lines[end-1] == "" {
		end--
	}
	lines = lines[start:end]
	if len(lines) == 0 {
		return ""
	}
	base := leadingSpaces(lines[0])
	for i, line := range lines {
		if line != "" {
			lines[i] = stripSpaces(line, base)
		}
	}
	return strings.Join(lines, "\n")
}

// CheckPromptBody reports why body has no written form, or nil: a line
// indented less than the first kept line, whose indentation the lexer
// takes as the body's.
func CheckPromptBody(body string) error {
	kept, base := promptBodyLines(body)
	for _, k := range kept {
		if n := leadingSpaces(k.text); n < base {
			return fmt.Errorf("line %d is indented less than the body's first line (%d < %d spaces); the prompt syntax takes the first line's indentation as the body's — outdent the first line", k.line, n, base)
		}
	}
	return nil
}

// keptLine is a line of a prompt body that survives the lexer, with its
// 1-based line number in the body as written.
type keptLine struct {
	text string
	line int
}

// promptBodyLines is the kept lines of body — trailing CRs removed, blank
// and space-only lines dropped — and the first kept line's indentation.
func promptBodyLines(body string) (kept []keptLine, base int) {
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.Trim(line, " ") == "" {
			continue
		}
		if len(kept) == 0 {
			base = leadingSpaces(line)
		}
		kept = append(kept, keptLine{text: line, line: i + 1})
	}
	return kept, base
}

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// stripSpaces removes up to n leading spaces from line.
func stripSpaces(line string, n int) string {
	for i := 0; i < n && len(line) > 0 && line[0] == ' '; i++ {
		line = line[1:]
	}
	return line
}
