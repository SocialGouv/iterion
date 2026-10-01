package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClipRunError: a run's Error bounded for a reader keeps its remedy —
// the clip falls on the failure's text — and never exceeds the bound.
func TestClipRunError(t *testing.T) {
	const limit = 100
	remedy := RunErrorHintSeparator + "resume (--accept-scratch-loss)"
	for _, tc := range []struct {
		name, in, want string
	}{
		{"within the bound", "short — hint: do this", "short — hint: do this"},
		{"a long failure keeps its remedy whole", strings.Repeat("f", 300) + remedy,
			strings.Repeat("f", limit-utf8.RuneCountInString(remedy)-1) + "…" + remedy},
		{"a remedy past half the bound leaves the failure half", strings.Repeat("f", 300) + RunErrorHintSeparator + strings.Repeat("r", 300),
			strings.Repeat("f", limit/2-1) + "…" + RunErrorHintSeparator + strings.Repeat("r", limit/2-utf8.RuneCountInString(RunErrorHintSeparator)-1) + "…"},
		{"a short failure leaves its room to the remedy", "boom" + RunErrorHintSeparator + strings.Repeat("r", 300),
			"boom" + RunErrorHintSeparator + strings.Repeat("r", limit-4-utf8.RuneCountInString(RunErrorHintSeparator)-1) + "…"},
		{"the remedy is the last one", strings.Repeat("f", 50) + RunErrorHintSeparator + "inner" + strings.Repeat("g", 200) + RunErrorHintSeparator + "do this",
			strings.Repeat("f", 50) + RunErrorHintSeparator + "inner" + strings.Repeat("g", limit-50-2*utf8.RuneCountInString(RunErrorHintSeparator)-5-7-1) + "…" + RunErrorHintSeparator + "do this"},
		{"no remedy: the end is clipped", strings.Repeat("é", 300), strings.Repeat("é", limit-1) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClipRunError(tc.in, limit)
			if got != tc.want {
				t.Fatalf("ClipRunError =\n %q\nwant\n %q", got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > limit || !utf8.ValidString(got) {
				t.Fatalf("ClipRunError gives %d runes (valid UTF-8: %v), want at most %d", n, utf8.ValidString(got), limit)
			}
		})
	}
}
