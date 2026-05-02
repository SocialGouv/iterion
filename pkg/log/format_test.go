package log

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateMiddle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short string below max", "hello", 10, "hello"},
		{"empty string", "", 5, ""},
		{"exactly max runes", "hello", 5, "hello"},
		{"long ascii even budget", "abcdefghij", 5, "ab…ij"},
		{"long ascii odd budget", "abcdefghij", 6, "abc…ij"},
		{"max zero", "hello", 0, ""},
		{"max negative", "hello", -3, ""},
		{"max one", "hello", 1, "…"},
		{"max two", "hello", 2, "h…"},
		{"multibyte below limit", "héllo", 10, "héllo"},
		{"multibyte exactly max runes", "héllo", 5, "héllo"},
		{"multibyte truncated", "αβγδεζηθικ", 5, "αβ…ικ"},
		{"emoji truncated odd budget", "🙂🙂🙂🙂🙂🙂", 4, "🙂🙂…🙂"},
		{"emoji truncated even budget", "🙂🙂🙂🙂🙂🙂", 5, "🙂🙂…🙂🙂"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateMiddle(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("TruncateMiddle(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateMiddle(%q, %d) returned invalid UTF-8: %q", tc.in, tc.max, got)
			}
			if tc.max > 0 {
				if rc := utf8.RuneCountInString(got); rc > tc.max {
					t.Errorf("TruncateMiddle(%q, %d) returned %d runes, exceeds max", tc.in, tc.max, rc)
				}
			}
		})
	}
}
