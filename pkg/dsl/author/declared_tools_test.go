package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A DECLARED empty tools list (`tools: []`, the author saying "this node has
// no tools") is not an absent one (the backend's own default): the document
// writes it back as `tools: []` and reads it back declared, as the .bot
// does. Dropping the line would turn a closed node into an open one.
func TestADeclaredEmptyToolsListSurvivesTheDocument(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct {
		name, tools string
		declared    bool
	}{
		{"declared empty", "  tools: []", true},
		{"absent", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{"dsl: 2", "", "agent a:", "  model: m"}
			if tc.tools != "" {
				lines = append(lines, tc.tools)
			}
			lines = append(lines, "", "workflow w:", "  entry: a", "  a -> done", "")
			pr := parser.Parse("x.bot", strings.Join(lines, nl))
			if len(pr.Diagnostics) > 0 {
				t.Fatalf(".bot refused: %v", pr.Diagnostics)
			}
			if got := pr.File.Agents[0].Tools != nil; got != tc.declared {
				t.Fatalf("the .bot reads declared = %v, want %v", got, tc.declared)
			}
			out, err := Write(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			if written := strings.Contains(string(out), "tools: []"); written != tc.declared {
				t.Errorf("the document writes `tools: []` = %v, want %v:%s%s", written, tc.declared, nl, out)
			}
			res := Parse("x.yaml", out)
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, out)
			}
			if got := res.File.Agents[0].Tools != nil; got != tc.declared {
				t.Errorf("read back declared = %v, want %v", got, tc.declared)
			}
		})
	}
}
