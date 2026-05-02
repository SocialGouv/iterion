package log

import "unicode/utf8"

// ellipsis is the single-rune marker inserted in the middle of a
// TruncateMiddle result.
const ellipsis = "…"

// TruncateMiddle returns s shortened to at most max runes by replacing the
// middle with an ellipsis (…), keeping the beginning and the end. If s
// already fits in max runes it is returned unchanged. If max <= 0 an empty
// string is returned. The ellipsis itself counts as one rune toward the
// limit; when the remaining budget is odd the head receives the extra rune.
//
// Unlike Truncate, which counts bytes, TruncateMiddle counts runes and
// always slices on rune boundaries — safe for arbitrary UTF-8 input such
// as IDs, hashes, paths, or URLs.
func TruncateMiddle(s string, max int) string {
	if max <= 0 {
		return ""
	}
	n := utf8.RuneCountInString(s)
	if n <= max {
		return s
	}
	if max == 1 {
		return ellipsis
	}
	keep := max - 1
	head := (keep + 1) / 2
	tail := keep / 2

	runes := []rune(s)
	return string(runes[:head]) + ellipsis + string(runes[len(runes)-tail:])
}
