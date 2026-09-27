package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// An AST edit rewrites a Literal's value and leaves Raw behind — the normal
// way a program changes a parsed number. The writer reused that Raw whenever
// it looked like a .bot number, so the document restored the value the parse
// had seen, not the one the AST now holds. Raw rides only while it still
// names FloatVal (#1821).
func TestTheWriterWritesTheEditedFloatNotAStaleRawSpelling(t *testing.T) {
	nl := string(rune(10))
	bot := strings.Join([]string{
		"dsl: 2",
		"",
		"vars:",
		"  threshold: float = 1.0",
		"",
		"presets:",
		"  dev:",
		"    threshold: 2.5",
		"",
		"tool t:",
		"  command: \"echo hi\"",
		"",
		"workflow w:",
		"  vars:",
		"    local: float = 3.5",
		"  entry: t",
		"  t -> done",
		"",
	}, nl)
	pr := parser.Parse("x.bot", bot)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf(".bot refused: %v", pr.Diagnostics)
	}

	// The edit: change the value, leave Raw — three sites, one writer.
	pr.File.Vars.Fields[0].Default.FloatVal = 8
	pr.File.Presets.Entries[0].Values[0].Value.FloatVal = 9
	pr.File.Workflows[0].Vars.Fields[0].Default.FloatVal = 7

	out, err := Write(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("x.yaml", out)
	if res.HasErrors() {
		t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, out)
	}
	if got := res.File.Vars.Fields[0].Default.FloatVal; got != 8 {
		t.Errorf("the edited var default came back as %v, want 8%s--- written:%s%s", got, nl, nl, out)
	}
	if got := res.File.Presets.Entries[0].Values[0].Value.FloatVal; got != 9 {
		t.Errorf("the edited preset value came back as %v, want 9%s--- written:%s%s", got, nl, nl, out)
	}
	if got := res.File.Workflows[0].Vars.Fields[0].Default.FloatVal; got != 7 {
		t.Errorf("the edited workflow var default came back as %v, want 7%s--- written:%s%s", got, nl, nl, out)
	}
}

// A raw spelling that still names the value keeps its shape — the trailing
// zeros an author wrote are a choice, not noise.
func TestAFloatRawStillNamingTheValueKeepsItsSpelling(t *testing.T) {
	nl := string(rune(10))
	bot := strings.Join([]string{
		"dsl: 2",
		"",
		"vars:",
		"  threshold: float = 8.00",
		"",
		"tool t:",
		"  command: \"echo hi\"",
		"",
		"workflow w:",
		"  entry: t",
		"  t -> done",
		"",
	}, nl)
	pr := parser.Parse("x.bot", bot)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf(".bot refused: %v", pr.Diagnostics)
	}
	out, err := Write(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "default: 8.00") {
		t.Errorf("the author's spelling was rewritten:%s%s", nl, out)
	}
}
