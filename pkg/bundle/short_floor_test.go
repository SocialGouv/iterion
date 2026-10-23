package bundle

import (
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The floor walk reads an executable source whole or names it (Short): a
// root entry whose fragments the read budget cuts short uses what nobody
// read — here an empty `tools: []`, which needs 3.190.0 — and a floor read
// off the rest (the `import`'s 3.145.0) would admit runners that cannot
// read it (r3a F4). Read whole, the same bundle asks for the later release.
func TestTheFloorWalkNamesASourceCutShort(t *testing.T) {
	main := "prompt p:\n  Do.\n\nagent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: p\n\nworkflow main:\n  entry: a\n  a -> done\n"
	child := "import \"lib/big.bot\"\nimport \"lib/k.bot\"\n\nworkflow c:\n  entry: helper\n  helper -> done\n"
	k := "prompt mission:\n  Do.\n\nagent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  tools: []\n"
	t.Setenv(parser.ReadBudgetEnv, "0")
	spent := func(name, src string) int {
		pr, err := parser.Parse(name, src)
		if err != nil {
			t.Fatal(err)
		}
		return pr.Spent
	}
	budget := 400
	big := "prompt big:\n" + strings.Repeat("  x\n", budget-spent("child.bot", child)-2)
	files := map[string]string{"main.bot": main, "child.bot": child, "lib/big.bot": big, "lib/k.bot": k}

	whole := MaxSyntaxRequirements(files)
	if len(whole.Short) != 0 || len(whole.EmptyToolsBy) != 1 {
		t.Fatalf("read whole: short %v, empty tools by %v", whole.Short, whole.EmptyToolsBy)
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	cut := MaxSyntaxRequirements(files)
	if len(cut.Short) != 1 || !strings.HasPrefix(cut.Short[0], "child.bot: ") || !strings.Contains(cut.Short[0], "past the read budget of "+strconv.Itoa(budget)) || !strings.Contains(cut.Short[0], parser.ReadBudgetEnv) {
		t.Errorf("cut short: Short = %v, want child.bot named with its refusal and the variable that moves the budget", cut.Short)
	}
	if len(cut.EmptyToolsBy) != 0 || len(cut.ImportedBy) != 0 {
		t.Errorf("cut short: the walk counted what it read of child.bot (empty tools by %v, imported by %v), a floor off a reading cut short", cut.EmptyToolsBy, cut.ImportedBy)
	}
}
