package bots

import (
	"encoding/json"
	"fmt"
	"os"
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

// A REUSED held-out set must refuse, and the refused report must not read green
// on the vacuous term.
//
// Bailing in preflight leaves the held-out figures at their 0/0 defaults, and
// the gate converges in part on `holdout_detected == holdout_total` — which
// 0 == 0 satisfies. The bail therefore stamps them DELIBERATELY unequal, the
// idiom the harness already uses for selfcheck and for the lost-baseline arm.
//
// That stamping is what this exercises, end to end, on the real harness: the
// sibling tests cover the fingerprint RULE (selftest) and the PLACEMENT (the
// AST guard), and neither would notice the two lines going away.
func TestGoldenMasterReusedHoldoutRefusesWithoutReadingGreen(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	gm := filepath.Join(ws, ".golden-master")

	// One mutant body, written into BOTH the held-out set and the audit pile:
	// identical content is identical fingerprint, which is the whole rule.
	const body = "#!/bin/sh\ntrue\n"
	for _, dir := range []string{
		filepath.Join(gm, "mutants", "holdout", "z1-reused"),
		filepath.Join(gm, "mutants", "audit", "01a0-previous-cycle", "z1-reused"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "apply.sh"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		// load_mutants keeps only directories carrying a meta.json, so without
		// one the held-out set reads as ABSENT and the reuse check has nothing
		// to compare — a different refusal entirely.
		meta := `{"surface": "write", "archetype": "create_lost", "targets": ["001"]}`
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Visible mutants covering the archetypes the corpus's surfaces require:
	// that bail runs before this one, and a hole there would mask what is
	// under test.
	for name, archetype := range map[string]string{
		"v1-roundtrip": "roundtrip_corruption",
		"v2-create":    "create_lost",
	} {
		d := filepath.Join(gm, "mutants", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "apply.sh"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{"surface": "write", "archetype": "` + archetype + `", "targets": ["001"]}`
		if err := os.WriteFile(filepath.Join(d, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(gm, "canon"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"config.json":    `{"up": "true", "base_url": "http://127.0.0.1:1", "standard": 3, "personas": [{"name": "alice", "login": {"fields": {"user": "alice", "pass": "x"}}}, {"name": "alice-upper", "case_variant_of": "alice", "login": {"fields": {"user": "ALICE", "pass": "x"}}}]}`,
		"corpus.json":    `{"entries": [{"id": "001", "surface": "write", "method": "POST", "path": "/items", "probes": ["write_create"]}, {"id": "002", "surface": "write", "method": "POST", "path": "/items/edit", "steps": [{"path": "/items/edit", "fields": {"name": ""}}, {"path": "/items/edit", "fields": {"name": "ok"}}], "probes": ["error_then_corrected"]}, {"id": "003", "surface": "read", "path": "/Items", "probes": ["case_pair"]}, {"id": "004", "surface": "read", "path": "/items", "probes": ["case_pair"]}, {"id": "005", "surface": "read", "path": "/items?sort=name", "probes": ["text_sort"]}, {"id": "006", "surface": "read", "path": "/profile", "persona": "alice-upper"}]}`,
		"standard-mark":  "3\n",
		"canon/rules.py": "def canonicalize(entry, status, headers, body):\n    return status, headers, body\n",
	} {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("python3", harness)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "GM_WORKSPACE="+ws)
	out, runErr := cmd.CombinedOutput()

	var report struct {
		HoldoutReused   []string `json:"holdout_reused"`
		HoldoutDetected int      `json:"holdout_detected"`
		HoldoutTotal    int      `json:"holdout_total"`
	}
	line := lastHarnessReport(string(out))
	if line == "" {
		t.Fatalf("the harness printed no JSON report (err=%v):\n%s", runErr, out)
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("report is not JSON (err=%v): %v\n%s", runErr, err, line)
	}

	if len(report.HoldoutReused) == 0 {
		t.Fatalf("a held-out set whose fingerprint is already published under mutants/audit/ "+
			"was not reported as reused — a spent set is evidence, not a test:\n%s", line)
	}
	// THE assertion the commit message claimed and nothing pinned.
	if report.HoldoutDetected == report.HoldoutTotal {
		t.Errorf("the refused report carries holdout_detected == holdout_total (%d == %d), "+
			"which the gate converges on: a refusal that reads green on one of the gate's own "+
			"terms is how a bail sails through for a reader that checks only that pair",
			report.HoldoutDetected, report.HoldoutTotal)
	}
}

// lastHarnessReport returns the final line that parses as a JSON object — the report
// the harness prints last, past any log noise.
func lastHarnessReport(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && json.Valid([]byte(l)) {
			return l
		}
	}
	return ""
}
