package delegate

import "unicode/utf8"

// truncate returns s unchanged if it fits within maxBytes, otherwise
// s[:n] + "..." where n ≤ maxBytes and n lands on a UTF-8 rune boundary.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
