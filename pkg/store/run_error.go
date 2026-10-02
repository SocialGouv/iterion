package store

import (
	"strings"
	"unicode/utf8"
)

// RunErrorHintSeparator joins a failure's text to its remedy in one line: a
// run's Error keeps a single string, the remedy last (runtime's
// Remedy.Annotate), and a refusal's rendering reads the same.
const RunErrorHintSeparator = " — hint: "

// ClipRunError bounds a run's Error to limit runes, markers included, for a
// reader that shows it in a bounded space: the clip falls on the failure's
// text, marked "…", and the remedy after the last RunErrorHintSeparator is
// kept whole whenever it fits the limit at all — the failure keeps the
// rest. Only a remedy that cannot fit even alone is clipped in turn, and
// then the failure keeps half. A value without a remedy is clipped at its
// end.
func ClipRunError(s string, limit int) string {
	n := utf8.RuneCountInString(s)
	if n <= limit {
		return s
	}
	i := strings.LastIndex(s, RunErrorHintSeparator)
	if i < 0 {
		return clipRunes(s, limit)
	}
	head, remedy := s[:i], s[i:]
	r := utf8.RuneCountInString(remedy)
	keep := limit - r
	if r > limit {
		keep = limit / 2
	}
	return clipRunes(head, min(keep, n-r)) + clipRunes(remedy, limit-min(keep, n-r))
}

// clipRunes is s cut to at most n runes, its last one "…" when s is cut.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	end := 0
	for range n - 1 {
		_, w := utf8.DecodeRuneInString(s[end:])
		end += w
	}
	return s[:end] + "…"
}
