package cli_test

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// A finding with no position is usually the CONSEQUENCE of a positioned one:
// a tab on the `model:` line drops the agent declaration, so the entry node
// no longer exists (a global C008). Listed first, the consequence is what the
// reader — an agent in a validate loop — acts on; it must come last.
func TestValidate_GlobalFindingsSortAfterPositionedOnes(t *testing.T) {
	dir := t.TempDir()
	src := "schema out:\n  ok: bool\n\nagent a:\n\tmodel: \"m\"\n  output: out\n\nworkflow w:\n  entry: a\n  a -> done\n"
	path := writeFixture(t, dir, "tab.bot", src)
	p, buf := newTestPrinter(cli.OutputJSON)
	_ = cli.RunValidate(path, p)
	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", err, buf.String())
	}
	positioned, global := 0, 0
	seenGlobal := false
	for _, d := range result.Diagnostics {
		if d.Line == 0 {
			global++
			seenGlobal = true
			continue
		}
		positioned++
		if seenGlobal {
			t.Fatalf("a positioned finding follows a global one:\n%+v", result.Diagnostics)
		}
	}
	if positioned == 0 || global == 0 {
		t.Fatalf("fixture must yield both a positioned and a global finding, got %d/%d:\n%+v", positioned, global, result.Diagnostics)
	}
}
