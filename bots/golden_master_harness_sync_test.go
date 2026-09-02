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

	inlined := inlinedHarnessBody(t, bot)
	standalone := standaloneHarnessBody(t, harness)

	if inlined != standalone {
		a, b := strings.Split(inlined, "\n"), strings.Split(standalone, "\n")
		for i := 0; i < len(a) || i < len(b); i++ {
			x, y := lineAt(a, i), lineAt(b, i)
			if x != y {
				t.Fatalf("the two copies of the harness have diverged at body line %d.\n"+
					"  main.bot (the copy that RUNS):        %q\n"+
					"  oracle-harness.py (the one REVIEWED): %q\n"+
					"Regenerate the inlined copy from the standalone one: same body, indented by four, "+
					"under the node preamble.", i+1, x, y)
			}
		}
		t.Fatalf("the two copies differ in length: %d lines inlined, %d standalone", len(a), len(b))
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

// TestGoldenMasterSkillMatchesObservationFields pins the collision key the
// SKILL documents against the one the harness computes.
//
// Same doctrine as the sync check above, applied to the other copy nobody
// diffs: the skill is the contract the acting agent reads before it decides
// how to distinguish two observations, and `_entry_observation_key` is what
// actually judges them. A review already caught this drifting once — the skill
// defined the tuple as "the entry minus its id", which the implementation
// explicitly rejects. The correction was written from the reviewer's field
// list, the allowlist then changed in the same branch, and the skill went back
// out of date naming three fields the key does not read (`params`, `query`,
// `body`) while omitting three it does.
//
// The cost is not cosmetic: an agent told `body` discriminates will add an
// entry that differs only there, and watch it refused as a collision with
// nothing explaining why.
func TestGoldenMasterSkillMatchesObservationFields(t *testing.T) {
	harness := readHarnessFile(t, "golden-master/oracle-harness.py")
	skill := readHarnessFile(t, "golden-master/skills/golden-master.md")

	decl := regexp.MustCompile(`(?s)OBSERVATION_FIELDS = \((.*?)\)`).FindStringSubmatch(harness)
	if decl == nil {
		t.Fatal("oracle-harness.py: OBSERVATION_FIELDS declaration not found — the constant was renamed, fix this test")
	}
	code := regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(decl[1], -1)

	// The skill states the tuple inline, right after naming the allowlist.
	doc := regexp.MustCompile(`(?s)OBSERVATION_FIELDS` + "` " + `allowlist\n?\((.*?)\), NOT`).FindStringSubmatch(skill)
	if doc == nil {
		t.Fatal("golden-master.md: the OBSERVATION_FIELDS allowlist sentence not found — the skill was reworded, fix this test")
	}
	documented := regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(doc[1], -1)

	inCode := map[string]bool{}
	for _, m := range code {
		inCode[m[1]] = true
	}
	inDoc := map[string]bool{}
	for _, m := range documented {
		inDoc[m[1]] = true
	}
	for f := range inCode {
		if !inDoc[f] {
			t.Errorf("the harness keys observations on %q but the skill does not list it: an agent is told that field cannot disambiguate two entries, when it does", f)
		}
	}
	for f := range inDoc {
		if !inCode[f] {
			t.Errorf("the skill lists %q in the collision tuple but the harness never reads it: an agent told to disambiguate with that field gets refused for a collision it thought it had resolved", f)
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
func inlinedHarnessBody(t *testing.T, bot string) string {
	t.Helper()
	const preambleEnd = "os.environ['GM_MODE'] = 'gate'"

	lines := strings.Split(bot, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, preambleEnd) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("main.bot: end of the oracle_run preamble (%q) not found — the node was restructured, fix this test", preambleEnd)
	}

	var out []string
	for _, l := range lines[start:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "    ") {
			break
		}
		out = append(out, strings.TrimPrefix(l, "    "))
	}
	if len(out) < 100 {
		t.Fatalf("main.bot: only %d lines of harness body found after the preamble — the block scalar was not read correctly", len(out))
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
