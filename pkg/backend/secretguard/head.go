package secretguard

import "unicode/utf8"

// RedactHead redacts what a display bound n keeps of s, and cuts it there.
// red is the redactor (a Guard's Redact, or nil for none); margin is how far
// past n it reads at least — the longest text red recognises as one secret
// (Guard.LongestLiteral) or more — so that a secret straddling the bound is
// recognised whole. The window's last margin is never kept: a secret the
// window holds only in part sits there, unrecognised, however far redaction
// before it shrank the text. cut reports whether s was cut; the head then
// ends on a rune boundary. Redacting a whole multi-megabyte value costs
// seconds; this reads n+margin bytes at most.
func RedactHead(s string, n, margin int, red func(string) string) (head string, cut bool) {
	if red == nil {
		red = func(v string) string { return v }
	}
	if len(s) <= n {
		return red(s), false
	}
	window := clipRunes(s, n+margin)
	r := red(window)
	if len(window) == len(s) {
		// The window holds s whole: nothing sits unrecognised at its end.
		if len(r) <= n {
			return r, false
		}
		return clipRunes(r, n), true
	}
	return clipRunes(r, max(0, min(n, len(r)-margin))), true
}

// clipRunes cuts s to at most n bytes, on a rune boundary.
func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
