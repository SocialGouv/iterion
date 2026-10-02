package cli_test

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// A finding of a document's unit — a fragment its imports name that cannot
// be read (E046) — points at the document the author wrote, at the line of
// its `imports:`, never at the .bot it stands for, which may not exist.
func TestValidatePositionsADocumentsUnitFindingsOnTheDocument(t *testing.T) {
	dir := t.TempDir()
	doc := writeFixture(t, dir, "x.bot.yaml", "dsl: 2\nimports: [lib/missing.bot]\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: a\n    model: m\n    system: ask\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n")
	res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	for _, d := range res.Diagnostics {
		if d.Code != "E046" {
			continue
		}
		if d.File != doc || d.Line != 2 {
			t.Fatalf("E046 at %s:%d, want the document %s at line 2 (not %s)\n%s", d.File, d.Line, doc, filepath.Join(dir, "x.bot"), out)
		}
		return
	}
	t.Fatalf("the unreadable fragment drew no E046:\n%s", out)
}
