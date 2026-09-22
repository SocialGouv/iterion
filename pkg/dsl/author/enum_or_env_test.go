package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// An enum|env property takes its words bare or quoted and ANY other value
// quoted, kept as written for a run-time substitution — as the .bot does.
// A .bot carrying such a value writes as a document whose reader gives the
// value back: the document says quoted with a quoted scalar. A plain word
// outside the list is refused, as the .bot refuses it bare.
func TestAnyQuotedRuntimeEffortRoundTrips(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct {
		name, value string
		quoted      bool
	}{
		{"a word of the list", "high", false},
		{"an environment variable", "$EFFORT", true},
		{"an environment form", "${EFFORT:-high}", true},
		{"a template", "{{ .vars.effort }}", true},
		{"any text", "whatever", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Join([]string{"dsl: 2", "", "agent a:", "  model: m", `  reasoning_effort: "` + tc.value + `"`, "", "workflow w:", "  entry: a", "  a -> done", ""}, nl)
			pr := parser.Parse("x.bot", src)
			if len(pr.Diagnostics) > 0 {
				t.Fatalf("the .bot refuses the value: %v", pr.Diagnostics)
			}
			out, err := Write(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			var line string
			for _, l := range strings.Split(string(out), nl) {
				if strings.Contains(l, "reasoning_effort") {
					line = l
				}
			}
			if quoted := strings.Contains(line, `"`); quoted != tc.quoted {
				t.Errorf("the document writes %q; quoted = %v wanted", line, tc.quoted)
			}
			res := Parse("x.yaml", out)
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, out)
			}
			if got := res.File.Agents[0].ReasoningEffort; got != tc.value {
				t.Errorf("read back %q, want %q", got, tc.value)
			}
		})
	}
	t.Run("a plain word outside the list is refused", func(t *testing.T) {
		doc := strings.Join([]string{"dsl: 2", "nodes:", "  - agent: a", "    model: m", "    reasoning_effort: whatever", "workflow:", "  name: w", "  entry: a", ""}, nl)
		res := Parse("p.yaml", []byte(doc))
		for _, d := range res.Diagnostics {
			if d.Code == parser.DiagAuthorValue && strings.Contains(d.Message, "in quotes") {
				return
			}
		}
		t.Fatalf("no E051 naming the quoted form among %v", res.Diagnostics)
	})
}
