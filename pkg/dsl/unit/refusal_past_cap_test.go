package unit

// A refusal past the report's cap is still the load's: a fragment there and
// not read (its permissions) imported after more missing fragments than a
// code may report (1001 E046, past the per-code cap of 1000) is STILL the
// refusal — the load is short (ShortError), its refusal names it, and a
// salvage of the files read marks the display. Reported through the capped
// list instead, the load would answer whole: a write from what it showed
// would replace the fragment nobody read.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestR7BRefusalPastTheCodeCap(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 1001; i++ {
		fmt.Fprintf(&b, "import \"lib/m%04d.bot\"\n", i)
	}
	b.WriteString("import \"lib/locked.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "lib", "locked.bot")
	if err := os.WriteFile(locked, []byte("prompt s:\n  x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	u, err := LoadDir(filepath.Join(dir, "main.bot"))
	var short *ShortError
	if !errors.As(err, &short) {
		n := 0
		if u != nil {
			n = len(u.Diagnostics)
		}
		t.Fatalf("R7BCAP the load is not short: err=%v unit_diags=%d — the refusal past the code cap was dropped", err, n)
	}
	d := short.ReadRefusal().Diagnostic
	t.Logf("R7BCAP refusal: %s (bound %v)", d.Error(), short.ReadRefusal().Bound)
	if !strings.Contains(d.Message, "locked.bot") || !strings.Contains(d.Message, "permission denied") {
		t.Errorf("R7BCAP the refusal is not the locked fragment's: %s", d.Error())
	}
	v := Salvage(u, err)
	n46 := 0
	for _, x := range v.Diagnostics() {
		if x.Code == parser.DiagImportUnreadable {
			n46++
		}
	}
	t.Logf("R7BCAP view diagnostics=%d (E046=%d) errors counted=%d", len(v.Diagnostics()), n46, v.ErrorCount())
}
