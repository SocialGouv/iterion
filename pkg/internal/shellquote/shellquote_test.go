package shellquote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Empty string.
		{"empty", "", "''"},

		// Safe strings returned bare.
		{"bare word", "hello", "hello"},
		{"upper and digits", "Abc123", "Abc123"},
		{"path", "/usr/local/bin/iterion", "/usr/local/bin/iterion"},
		{"relative path with dots", "./a.b/c-d_e", "./a.b/c-d_e"},
		{"colon", "key:value", "key:value"},
		{"at sign", "user@host", "user@host"},
		{"comma", "a,b,c", "a,b,c"},
		{"plus", "a+b", "a+b"},
		{"equals", "KEY=value", "KEY=value"},
		{"leading dash flag", "--flag", "--flag"},

		// Unsafe: wrapped in single quotes.
		{"space", "hello world", "'hello world'"},
		{"tab", "a\tb", "'a\tb'"},
		{"newline", "a\nb", "'a\nb'"},
		{"double quote", `say "hi"`, `'say "hi"'`},
		{"dollar", "$HOME", "'$HOME'"},
		{"backtick", "`cmd`", "'`cmd`'"},
		{"backslash", `a\b`, `'a\b'`},
		{"semicolon", "a;b", "'a;b'"},
		{"pipe", "a|b", "'a|b'"},
		{"ampersand", "a&b", "'a&b'"},
		{"redirect", "a>b", "'a>b'"},
		{"glob star", "*.go", "'*.go'"},
		{"question mark", "a?", "'a?'"},
		{"tilde", "~/dir", "'~/dir'"},
		{"parens", "(x)", "'(x)'"},
		{"hash", "#comment", "'#comment'"},
		{"percent", "100%", "'100%'"},
		{"unicode multibyte", "héllo", "'héllo'"},

		// Embedded single quotes use the canonical '\'' sequence.
		{"single quote alone", "'", `''\'''`},
		{"apostrophe word", "it's", `'it'\''s'`},
		{"two single quotes", "''", `''\'''\'''`},
		{"quote at start", "'x", `''\''x'`},
		{"quote at end", "x'", `'x'\'''`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quote(tt.in); got != tt.want {
				t.Errorf("Quote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestQuoteShellRoundTrip proves the quoted form survives a real
// /bin/sh evaluation byte-for-byte: `sh -c "printf %s <quoted>"`
// must print the original string.
func TestQuoteShellRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	inputs := []string{
		"",
		"plain",
		"hello world",
		"it's a 'test'",
		`$HOME and "quotes" and ` + "`backticks`",
		"semi;colon|pipe&amp",
		"new\nline\ttab",
		"star* question? ~tilde",
		`back\slash`,
		"héllo wörld",
	}
	for _, in := range inputs {
		in := in
		t.Run(strings.ReplaceAll(in, "\n", "\\n"), func(t *testing.T) {
			out, err := exec.Command("sh", "-c", "printf %s "+Quote(in)).Output()
			if err != nil {
				t.Fatalf("sh -c failed: %v", err)
			}
			if string(out) != in {
				t.Errorf("round trip: got %q, want %q", out, in)
			}
		})
	}
}
