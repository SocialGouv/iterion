package parser

import (
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// MaxProfile is the newest syntax profile this build reads. A file declaring
// a higher one was written for a newer engine: the parser refuses it (E040)
// and names `requires.iterion`, the manifest floor that keeps such a file off
// a build that cannot read it.
const MaxProfile = 2

// Preamble is what the head of a file says about how the rest of it is read,
// decided BEFORE tokenising: the escape mode of every quoted string depends
// on it, and the lexer cannot tokenise a string without knowing it.
//
// Two readers used to derive this each in their own way — the lexer's
// directive pre-scan and the unparser's mirror of it. ReadPreamble is the one
// definition both, and the migrator, now share.
type Preamble struct {
	// Profile is the value of a `dsl: N` header on the file's first
	// significant line, 0 when the file has none (read as profile 1) and
	// -1 when the header is present but its value is not a positive
	// integer (the parser reports it as E040; the lexer reads the file as
	// profile 1 meanwhile).
	Profile int
	// HeaderLine is the 1-based line of the `dsl:` header, 0 when absent.
	HeaderLine int
	// StrictEscape reports the profile-1 `## strict-escape: on` directive,
	// with profile 1's exact rule: among the first 32 lines, before the
	// first non-comment line. The rule is FROZEN — a directive on line 33
	// is not read, and never was — because widening the window would
	// change what a valid headerless file means.
	StrictEscape bool
}

// ReadPreamble reads the head of src: blank lines and comment lines (the
// frontmatter `## ---` block among them) are skipped, and the first
// significant line is examined for a `dsl: N` header. The directive is
// looked up by its own, narrower rule (see Preamble.StrictEscape).
//
// src is the text as the lexer sees it: BOM stripped, CRLF folded.
func ReadPreamble(src string) Preamble {
	pre := Preamble{StrictEscape: detectStrictEscape(src)}
	line := 0
	for rest := src; rest != ""; {
		line++
		var ln string
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			ln, rest = rest[:i], rest[i+1:]
		} else {
			ln, rest = rest, ""
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if _, ok := workflowfile.CommentText(ln); ok {
			continue
		}
		if value, ok := dslHeaderValue(ln); ok {
			pre.HeaderLine = line
			pre.Profile = parseProfile(value)
		}
		return pre
	}
	return pre
}

// dslHeaderValue reads `dsl: <value>` off a line — the header keyword at
// column 1, a colon, the value, then nothing but an optional comment — and
// returns the value text. A line that is not a header reports false.
func dslHeaderValue(line string) (string, bool) {
	if !strings.HasPrefix(line, "dsl") {
		return "", false
	}
	rest := strings.TrimLeft(line[len("dsl"):], " \t")
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	rest = strings.TrimSpace(rest[1:])
	if i := strings.Index(rest, "#"); i >= 0 {
		rest = strings.TrimSpace(rest[:i])
	}
	return rest, true
}

// parseProfile turns a header value into a profile number: a positive
// integer written in digits alone, or -1 for anything else (the parser
// names the defect). Digits alone, because the parser reads the value as
// one integer token: a sign or a space it would not accept must not be read
// here as a profile.
func parseProfile(value string) int {
	if value == "" {
		return -1
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return -1
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return -1
	}
	return n
}

// IsStrictEscapeDirective reports whether a comment's text is the profile-1
// directive that opts quoted strings into standard escapes, in any of the
// spellings the lexer accepts.
func IsStrictEscapeDirective(text string) bool {
	switch strings.TrimSpace(text) {
	case "strict-escape: on", "strict-escape:on", "strict-escape = on":
		return true
	}
	return false
}
