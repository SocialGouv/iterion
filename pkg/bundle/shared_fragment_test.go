package bundle

import (
	"testing"
)

// A fragment two workflows import is one file: C252's list names it once,
// and the profile it declares is counted once.
func TestSharedFragmentIsNamedOnce(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/n.bot\"\n\nworkflow w:\n  entry: a\n  a -> child\n  child -> done\n\nsubbot child:\n  source: \"child.bot\"\n",
		"child.bot": "import \"lib/n.bot\"\n\nworkflow c:\n  entry: a\n  a -> done\n",
		"lib/n.bot": "dsl: 2\nimport \"s.bot\"\n\nagent a:\n  model: \"m\"\n",
		"lib/s.bot": "schema s:\n  ok: bool\n",
	}
	req := MaxSyntaxRequirements(files)
	if got := req.Describe(); got != "dsl profile 2 (lib/n.bot) and `import` (child.bot, lib/n.bot, main.bot)" {
		t.Fatalf("Describe() = %q", got)
	}
	if len(req.DeclaredBy) != 1 || len(req.ImportedBy) != 3 {
		t.Fatalf("declaredBy %v importedBy %v", req.DeclaredBy, req.ImportedBy)
	}
}
