package bots

import (
	"os"
	"regexp"
	"testing"
)

// The golden-master harness exists twice: inlined in main.bot's oracle_run node
// (the copy that actually runs) and as oracle-harness.py (the reviewable copy a
// human reads). The README says "keep the two in sync" — which is a sentence,
// not a check, and this bot's own doctrine is that an obligation nobody
// verifies is an obligation that drifts.
//
// The failure this prevents is quiet: a fix lands in the reviewable copy only,
// review passes, and the gate keeps running the old logic. Nothing reports it,
// because the running copy is exactly the one nobody reads.
//
// Comparing the two verbatim is not possible — main.bot's copy is indented
// inside a block scalar and carries a node preamble the standalone file has no
// reason to have. So this pins what must not diverge: the set of top-level
// functions, and the report fields the gate's conjunction reads.
func TestGoldenMasterHarnessCopiesStayInSync(t *testing.T) {
	bot := readHarnessFile(t, "golden-master/main.bot")
	harness := readHarnessFile(t, "golden-master/oracle-harness.py")

	// 1. Same top-level functions, checked in both directions. In main.bot the
	//    harness sits in a block scalar indented by exactly four spaces, so
	//    four spaces is what "top level" means there; anything deeper is a
	//    method or a closure and has a counterpart nested in the other copy
	//    too, which would make the comparison compare different things.
	want := defNames(regexp.MustCompile(`(?m)^def (\w+)\(`), harness)
	got := defNames(regexp.MustCompile(`(?m)^ {4}def (\w+)\(`), bot)

	for name := range want {
		if !got[name] {
			t.Errorf("oracle-harness.py defines %s() but the inlined copy in main.bot does not: the copy that RUNS is missing it", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("main.bot's inlined copy defines %s() but oracle-harness.py does not: the copy that gets REVIEWED is missing it", name)
		}
	}

	// 2. Every field the gate's conjunction reads must be DECLARED in both
	//    copies' report skeleton. A field only one of them carries makes the
	//    gate read a default and call it a verdict.
	//
	//    The check is on the skeleton declaration (`"field":`), not on the
	//    name appearing somewhere: a first version tested mere presence and
	//    was proven blind by a mutation — deleting the declaration left the
	//    name alive inside a log message, and the test stayed green. Fitting,
	//    for the bot whose whole point is that an oracle must be shown to see.
	for _, field := range gateFields(t, bot) {
		declared := regexp.MustCompile(`"` + regexp.QuoteMeta(field) + `"\s*:`)
		if !declared.MatchString(bot) {
			t.Errorf("the gate reads outputs.oracle_run.%s but the inlined harness never declares it in its report: the gate would decide on a default", field)
		}
		if !declared.MatchString(harness) {
			t.Errorf("the gate reads outputs.oracle_run.%s but oracle-harness.py never declares it in its report: the reviewable copy has drifted", field)
		}
	}
}

// gateFields extracts the report fields the conjunction reads, so the test
// follows the gate instead of carrying a hardcoded list that would drift too.
func gateFields(t *testing.T, bot string) []string {
	t.Helper()
	re := regexp.MustCompile(`outputs\.oracle_run\.(\w+)`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(bot, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatal("no outputs.oracle_run.* reference found: the conjunction moved, fix this test")
	}
	return out
}

func defNames(re *regexp.Regexp, body string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

func readHarnessFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s unreadable: %v", path, err)
	}
	return string(b)
}
