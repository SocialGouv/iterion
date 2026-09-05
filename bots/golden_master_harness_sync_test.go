package bots

import (
	"os"
	"regexp"
	"strings"
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
// This check compares the two BYTE FOR BYTE, and the strength of that is the
// whole point. An earlier version pinned a PROXY — the set of top-level
// function names and the report fields — on the stated grounds that verbatim
// comparison was impossible. It was not impossible, it was merely not done: the
// two copies then drifted by sixty-two lines while this test stayed green.
// A check that establishes something NEAR what it claims, and reports the
// resemblance, is the exact defect this bot exists to catch in others.
//
// Verbatim comparison is possible because the running copy is the reviewable
// one, indented by four and preceded by a node preamble that binds the graph's
// vars into the environment. Those two mechanical differences are undone here;
// nothing else is allowed to differ.
func TestGoldenMasterHarnessCopiesStayInSync(t *testing.T) {
	bot := readHarnessFile(t, "golden-master/main.bot")
	harness := readHarnessFile(t, "golden-master/oracle-harness.py")
	standalone := standaloneHarnessBody(t, harness)

	// Every inlined copy, with the line that ends its preamble: main.bot's
	// gate node, and sync-harness.bot's driver — the tool-only bot that
	// materialises this same body into a target tree. A third copy that
	// drifted would sync a judge nobody reviewed.
	for _, c := range []struct{ file, preambleEnd string }{
		{"golden-master/main.bot", "os.environ['GM_MODE'] = 'gate'"},
		{"golden-master/sync-harness.bot", "# ---- inlined harness below"},
	} {
		inlined := inlinedHarnessBody(t, readHarnessFile(t, c.file), c.preambleEnd)
		if inlined != standalone {
			a, b := strings.Split(inlined, "\n"), strings.Split(standalone, "\n")
			for i := 0; i < len(a) || i < len(b); i++ {
				x, y := lineAt(a, i), lineAt(b, i)
				if x != y {
					t.Fatalf("the copies of the harness have diverged at body line %d.\n"+
						"  %s (a copy that RUNS):            %q\n"+
						"  oracle-harness.py (the one REVIEWED): %q\n"+
						"Regenerate the inlined copies from the standalone one (bots/golden-master/sync-harness.py): "+
						"same body, indented by four, under each node's preamble.", i+1, c.file, x, y)
				}
			}
			t.Fatalf("%s and the standalone copy differ in length: %d lines inlined, %d standalone", c.file, len(a), len(b))
		}
	}

	// Orthogonal, and it survives the identity check above because it pins a
	// different contract: what the GRAPH reads must exist in the harness at all.
	// Identical copies can still both be missing a field the gate consumes.
	//
	// The check is on the skeleton declaration (`"field":`), not on the name
	// appearing somewhere: a first version tested mere presence and was proven
	// blind by a mutation — deleting the declaration left the name alive inside
	// a log message, and the test stayed green. Fitting, for the bot whose whole
	// point is that an oracle must be shown to see.
	for _, field := range gateFields(t, bot) {
		declared := regexp.MustCompile(`"` + regexp.QuoteMeta(field) + `"\s*:`)
		if !declared.MatchString(harness) {
			t.Errorf("the gate reads outputs.oracle_run.%s but the harness never declares it in its report: the gate would decide on a default", field)
		}
	}
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<absent>"
}

// inlinedHarnessBody returns the runnable copy, dedented, with the node
// preamble removed. The preamble is the part that CANNOT be shared: it binds
// {{vars.*}} into the environment, which only means something inside the graph.
func inlinedHarnessBody(t *testing.T, bot, preambleEnd string) string {
	t.Helper()
	lines := strings.Split(bot, "\n")
	start := -1
	for i, l := range lines {
		// The line that IS the end of the preamble — an assignment quoting
		// the marker in a string is not one.
		if strings.Contains(l, preambleEnd) && !strings.HasPrefix(strings.TrimSpace(l), "MARK") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("end of the harness preamble (%q) not found — the node was restructured, fix this test", preambleEnd)
	}

	var out []string
	for _, l := range lines[start:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "    ") {
			break
		}
		out = append(out, strings.TrimPrefix(l, "    "))
	}
	if len(out) < 100 {
		t.Fatalf("only %d lines of harness body found after the preamble — the block scalar was not read correctly", len(out))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// standaloneHarnessBody drops the standalone header — shebang and module
// docstring, which the block scalar has no use for — and returns the rest.
func standaloneHarnessBody(t *testing.T, harness string) string {
	t.Helper()
	const bodyStart = "import hashlib"

	lines := strings.Split(harness, "\n")
	for i, l := range lines {
		if l == bodyStart {
			return strings.TrimSpace(strings.Join(lines[i:], "\n"))
		}
	}
	t.Fatalf("oracle-harness.py: first body line (%q) not found — the imports were reordered, fix this test", bodyStart)
	return ""
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

func readHarnessFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s unreadable: %v", path, err)
	}
	return string(b)
}
