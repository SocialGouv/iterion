package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// C142 reports a `\"` that the SHELL reads in an unquoted context. It reads
// the value, not where the value came from — and this is what makes that the
// only workable design, so it is guarded here rather than left as a comment.
//
// A provenance bit ("this string was written as `"…"`, so its escapes are
// legacy") would be correct exactly once. `b.str` rewrites any value holding a
// quote or a backslash into a BACKTICK raw string, so the first studio save
// flips the bit to "raw" — and the diagnostic would stop firing on the very
// value it exists to catch, silently, on a file nobody edited by hand.
//
// The value survives the rewrite unchanged, so the verdict does too. Both
// directions matter: the defect must still be reported after a save, and the
// legitimate nested quote must still be accepted.
func TestPostCreateEscapeVerdictSurvivesTheRoundTrip(t *testing.T) {
	const botShell = `
tool probe:
  command: ` + "`echo hi`" + `
  output: out

schema out:
  ok: string

workflow w:
  entry: probe

  sandbox:
    image: "example/image:tag"
    post_create: %s
`

	cases := []struct {
		name string
		// value is the post_create as WRITTEN in the .bot, delimiters included.
		value string
		fires bool
		why   string
	}{
		{
			name:  "legacy string carrying the defect",
			value: `"npm install -g --prefix \"$HOME/.npm-global\" pkg"`,
			fires: true,
			why:   "the save rewrites the delimiters, not the bytes — the shell still receives an unquoted \\\" and still glues a literal quote into the word",
		},
		{
			name:  "raw string carrying a legitimate nested quote",
			value: "`" + `printf '%s' "{\"a\":1}" > /tmp/x.json` + "`",
			fires: false,
			why:   "correct working shell before the save, and the save does not change the value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(botShell, "%s", tc.value, 1)

			before := parser.Parse("test.bot", src)
			if before.File == nil {
				t.Fatalf("parse produced no File for:\n%s", src)
			}
			if got := hasEscapedQuoteDiag(ir.Compile(before.File)); got != tc.fires {
				t.Fatalf("before the round-trip: C142 fired = %v, want %v — the fixture no longer exercises what it claims", got, tc.fires)
			}

			// What the studio writes back when the operator saves the file.
			text := Unparse(before.File)
			after := parser.Parse("test.bot", text)
			if after.File == nil {
				t.Fatalf("the rewritten text does not parse:\n%s", text)
			}
			if err := Verify(before.File, text); err != nil {
				t.Fatalf("the post_create does not round-trip: %v\n%s", err, text)
			}

			if got := hasEscapedQuoteDiag(ir.Compile(after.File)); got != tc.fires {
				t.Fatalf("after the round-trip: C142 fired = %v, want %v.\n%s\nRewritten as:\n%s",
					got, tc.fires, tc.why, text)
			}
		})
	}
}

func hasEscapedQuoteDiag(cr *ir.CompileResult) bool {
	for _, d := range cr.Diagnostics {
		if d.Code == ir.DiagEscapedQuoteInShellString {
			return true
		}
	}
	return false
}
