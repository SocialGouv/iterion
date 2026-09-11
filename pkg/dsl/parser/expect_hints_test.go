package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// An "expected X, got Y" must arrive with the remedy for the shape the
// parser wanted at that token — the generic E002 line ("quote string
// values") told the author of `system: "…"` to do the opposite of the fix.
func TestExpectedTokenHintsNameTheWantedShape(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"quoted string where a prompt name belongs", "agent a:\n  system: \"Review the diff\"\n", "bare name"},
		{"bare word where a string belongs", "agent a:\n  backend: claw\n", "Quote this value"},
		{"header without its block", "agent a:\nworkflow w:\n  entry: a\n", "indented block"},
		{"bare word where a list belongs", "agent a:\n  tools: bash\n", "inline list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := parser.Parse("h.bot", tc.src)
			var found bool
			for _, d := range pr.Diagnostics {
				if d.Code == parser.DiagExpectedToken && strings.Contains(d.Hint, tc.want) {
					found = true
				}
			}
			if !found {
				var got []string
				for _, d := range pr.Diagnostics {
					got = append(got, d.Error()+" | fix: "+d.Hint)
				}
				t.Errorf("no E002 hint containing %q; diagnostics:\n%s", tc.want, strings.Join(got, "\n"))
			}
		})
	}
}
