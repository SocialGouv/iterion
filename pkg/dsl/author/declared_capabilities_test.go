package author

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"strings"
	"testing"
)

// A DECLARED empty capabilities list (`capabilities: []`, the author saying
// "this node holds no host capability", #1801) is not an absent one (the
// node inherits the workflow's list): the document writes it back as
// `capabilities: []` and reads it back declared, as the .bot does. Dropping
// the line would turn a closed grant back into an inheritance.
func TestADeclaredEmptyCapabilitiesListSurvivesTheDocument(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct {
		name, caps string
		declared   bool
	}{
		{"declared empty", "  capabilities: []", true},
		{"absent", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{"dsl: 2", "", "agent a:", "  model: m"}
			if tc.caps != "" {
				lines = append(lines, tc.caps)
			}
			lines = append(lines, "", "workflow w:", "  entry: a", "  a -> done", "")
			pr := parsertest.Parse("x.bot", strings.Join(lines, nl))
			if len(pr.Diagnostics) > 0 {
				t.Fatalf(".bot refused: %v", pr.Diagnostics)
			}
			if got := pr.File.Agents[0].Capabilities != nil; got != tc.declared {
				t.Fatalf("the .bot reads declared = %v, want %v", got, tc.declared)
			}
			out, err := Write(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			if written := strings.Contains(string(out), "capabilities: []"); written != tc.declared {
				t.Errorf("the document writes `capabilities: []` = %v, want %v:%s%s", written, tc.declared, nl, out)
			}
			res := Parse("x.yaml", out)
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, out)
			}
			if got := res.File.Agents[0].Capabilities != nil; got != tc.declared {
				t.Errorf("read back declared = %v, want %v", got, tc.declared)
			}
		})
	}
}
