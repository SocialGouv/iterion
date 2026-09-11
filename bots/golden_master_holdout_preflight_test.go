package bots

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Three properties of WHERE the held-out reuse check sits, pinned together
// because each one fails differently and silently.
//
// The check needs nothing the application provides: `spent_fingerprints` reads
// committed audit directories, `mutant_fingerprint` hashes a mutant directory,
// and `held_meta` is in hand a hundred lines earlier. It was nonetheless the
// LAST statement of the gate — after `app_up` and the entire corpus replay.
//
// Measured on a live campaign before the move: a lot run of 4 h 04 — of which
// 2 h 11 inside the replay — spent to be refused on a magazine that was already
// empty before a single request went out. Every other term of that report was
// green; this one cost the run.
//
// This is a STRUCTURAL guard and says so: it pins the ORDER and the COUNT of
// statements inside `main`, read through Python's own parser rather than by
// grepping text. It does not prove the refusal correct — the gate conjunction
// does that, via `length(outputs.oracle_run.holdout_reused) == 0`. It exists so
// the placement cannot be quietly undone.
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

def calls(name):
    return [n.lineno for n in ast.walk(main)
            if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)
            and n.func.id == name]

reuse = [n.lineno for n in ast.walk(main) if isinstance(n, ast.Assign)
         for tgt in n.targets
         if isinstance(tgt, ast.Subscript)
         and isinstance(tgt.value, ast.Name) and tgt.value.id == "report"
         and isinstance(tgt.slice, ast.Constant) and tgt.slice.value == "holdout_reused"]
boot = calls("app_up")
seal = calls("seal_holdout")

if not reuse:
    sys.exit("no assignment to report['holdout_reused'] inside main()")
if not boot:
    sys.exit("no call to app_up() inside main() — has the boot moved?")
if not seal:
    sys.exit("no call to seal_holdout() inside main() — has the seal moved?")
print("%d %d %d %d" % (len(reuse), min(reuse), min(boot), min(seal)))
`
	out, err := exec.Command("python3", "-c", program, harness).CombinedOutput()
	if err != nil {
		t.Fatalf("parsing the harness failed: %v\n%s", err, out)
	}
	var count, reuse, boot, seal int
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &count, &reuse, &boot, &seal); err != nil {
		t.Fatalf("unexpected output %q: %v", out, err)
	}

	// The cost this move was made for.
	if reuse >= boot {
		t.Errorf("the held-out reuse check is at line %d, the application boots at line %d — "+
			"a refusal that needs no application is paid for with a full boot and replay. "+
			"Measured cost when it last sat after the boot: one lot run of 4 h 04.", reuse, boot)
	}

	// Exactly ONE, because the late block was deleted rather than kept as a
	// second guard — two copies of one check is how they drift apart. A
	// min-based ordering pin alone would accept a late copy re-added ALONGSIDE
	// the preflight one, which is the drift it is meant to prevent.
	if count != 1 {
		t.Errorf("found %d assignments to report[\"holdout_reused\"] in main(), want exactly 1 — "+
			"a second copy is how the two fall out of step, which is why the late block was deleted", count)
	}

	// The check reads fingerprints off the SEALED set, and mutant_fingerprint
	// tolerates a missing file rather than raising. So a reordering that put the
	// seal after this point would not fail loudly: reuse detection would simply
	// stop seeing, and the report would read clean. Pinned here because nothing
	// else says it.
	if seal >= reuse {
		t.Errorf("seal_holdout is at line %d but the reuse check reads fingerprints at line %d — "+
			"unsealed, mutant_fingerprint tolerates missing files instead of raising, so "+
			"detection degrades SILENTLY and a reused set reports clean", seal, reuse)
	}
}
