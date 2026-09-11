package bots

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The held-out REUSE check must run before the application is booted.
//
// It needs nothing the application provides: `spent_fingerprints` reads
// committed audit directories and `mutant_fingerprint` hashes a mutant
// directory, and `held_meta` is in hand a hundred lines earlier. Yet it used to
// be the last statement of the gate, after `app_up` and the entire corpus
// replay.
//
// Measured on a live campaign before the move: a lot run of 4 h 04 — of which
// 2 h 11 inside the replay — spent to be refused on a magazine that was already
// empty before a single request went out. Everything else in that report was
// green; this one term cost the run.
//
// This is a STRUCTURAL guard, and says so: it pins the ORDER of two statements
// inside `main`, read from the real file through Python's own parser rather
// than by grepping text. It does not prove the refusal is correct — the gate
// conjunction does that, via `length(outputs.oracle_run.holdout_reused) == 0`.
// It exists so that the ordering cannot be quietly undone.
func TestGoldenMasterHoldoutReuseIsCheckedBeforeBoot(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
import ast, sys

tree = ast.parse(open(sys.argv[1], encoding="utf-8").read())
main = next(n for n in ast.walk(tree)
            if isinstance(n, ast.FunctionDef) and n.name == "main")

reuse = boot = None
for node in ast.walk(main):
    # report["holdout_reused"] = ...
    if isinstance(node, ast.Assign):
        for tgt in node.targets:
            if (isinstance(tgt, ast.Subscript)
                    and isinstance(tgt.value, ast.Name) and tgt.value.id == "report"
                    and isinstance(tgt.slice, ast.Constant)
                    and tgt.slice.value == "holdout_reused"):
                reuse = node.lineno if reuse is None else min(reuse, node.lineno)
    # app_up(config, ws)
    if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
            and node.func.id == "app_up"):
        boot = node.lineno if boot is None else min(boot, node.lineno)

if reuse is None:
    sys.exit("no assignment to report['holdout_reused'] found inside main()")
if boot is None:
    sys.exit("no call to app_up() found inside main() — has the boot moved?")
print("%d %d" % (reuse, boot))
`
	out, err := exec.Command("python3", "-c", program, harness).CombinedOutput()
	if err != nil {
		t.Fatalf("parsing the harness failed: %v\n%s", err, out)
	}
	var reuse, boot int
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &reuse, &boot); err != nil {
		t.Fatalf("unexpected output %q: %v", out, err)
	}
	if reuse >= boot {
		t.Fatalf("the held-out reuse check is at line %d, the application boots at line %d — "+
			"a refusal that needs no application is paid for with a full boot and replay. "+
			"Measured cost when it last sat after the boot: one lot run of 4 h 04.", reuse, boot)
	}
}
