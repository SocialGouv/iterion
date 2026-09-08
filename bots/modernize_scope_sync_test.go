package bots

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The deterministic core exists once as scope.py — the copy a human reviews —
// and once per tool node in main.bot that runs it. "Keep the copies in sync" is
// a sentence, not a check, and a sentence drifts.
//
// The failure this prevents is quiet: a fix lands in the reviewable copy only,
// review passes, and the nodes keep running the old logic — because the running
// copies are exactly the ones nobody reads.
//
// The comparison is BYTE FOR BYTE, and that is the point. Pinning a proxy (the
// set of function names, the report's fields) would establish something NEAR
// what it claims; golden-master's copies drifted by sixty-two lines while
// exactly that kind of check stayed green.
//
// Verbatim comparison is possible because each running copy is the reviewable
// one indented by four, under a preamble that binds the node's vars. That one
// mechanical difference is undone here; nothing else may differ.
const (
	scopeBodyBegin = "# ---- shared body below (kept byte-identical with the inlined copy) ----"
	scopeBodyEnd   = "# ---- shared body above ----"
	inlineBelow    = "    # ---- inlined body below"
	inlineAbove    = "    # ---- inlined body above"
)

func readScopeFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func scopeSection(t *testing.T, text, start, end, what string) string {
	t.Helper()
	i := strings.Index(text, start)
	if i < 0 {
		t.Fatalf("%s: start marker %q not found — the markers are the contract, restore them", what, start)
	}
	rest := text[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("%s: end marker %q not found", what, end)
	}
	return strings.Trim(rest[:j], "\n")
}

func scopeDedent(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "    ")
	}
	return strings.Join(lines, "\n")
}

// inlinedBodies returns every inlined copy in the bot, in file order. Returning
// them all is the whole point: checking only the first would let a second node
// carry a stale body under a green test — the drift this file exists to stop,
// one node further along.
func scopeInlinedBodies(t *testing.T, bot string) []string {
	t.Helper()
	var out []string
	rest := bot
	for {
		i := strings.Index(rest, inlineBelow)
		if i < 0 {
			return out
		}
		rest = rest[i+len(inlineBelow):]
		j := strings.Index(rest, inlineAbove)
		if j < 0 {
			t.Fatalf("main.bot: an opening inline marker at copy %d has no closing marker", len(out)+1)
		}
		out = append(out, scopeDedent(strings.Trim(rest[:j], "\n")))
		rest = rest[j+len(inlineAbove):]
	}
}

func TestScopeCopiesStayInSync(t *testing.T) {
	standalone := scopeSection(t, readScopeFile(t, "modernize-scope/scope.py"),
		scopeBodyBegin, scopeBodyEnd, "scope.py")
	inlined := scopeInlinedBodies(t, readScopeFile(t, "modernize-scope/main.bot"))

	// A bot with no inlined copy would pass every comparison below by having
	// nothing to compare — the shape of green that proves nothing.
	if len(inlined) == 0 {
		t.Fatal("main.bot carries no inlined copy of the core: the markers are gone, and this test " +
			"would pass by vacuity from here on")
	}

	for n, body := range inlined {
		if body == standalone {
			continue
		}
		a, b := strings.Split(body, "\n"), strings.Split(standalone, "\n")
		for i := 0; i < len(a) || i < len(b); i++ {
			x, y := "", ""
			if i < len(a) {
				x = a[i]
			}
			if i < len(b) {
				y = b[i]
			}
			if x != y {
				t.Fatalf("inlined copy %d of %d has diverged from scope.py at body line %d.\n"+
					"  main.bot   (a copy that RUNS):       %q\n"+
					"  scope.py   (the copy REVIEWED):      %q\n"+
					"Regenerate every copy: python3 bots/modernize-scope/sync.py", n+1, len(inlined), i+1, x, y)
			}
		}
		t.Fatalf("inlined copy %d of %d differs in length: %d lines inlined, %d standalone",
			n+1, len(inlined), len(a), len(b))
	}
}

// A core that names a language is not stack-agnostic: the stack-specific half
// belongs to a lang-<id> skill and to the extractor it prescribes.
// catalog_universality greps the .bot's typed var defaults; it does not read a
// bundled script, so this covers the script the nodes actually run.
func TestScopeNamesNoStack(t *testing.T) {
	body := readScopeFile(t, "modernize-scope/scope.py")
	// Split so this test does not match itself.
	for _, tok := range []string{"spr" + "ing", "ma" + "ven", "gra" + "dle", "np" + "m", "ya" + "rn",
		"car" + "go", "pip" + "env", "go" + "mod", "src/main/" + "java", "package." + "json"} {
		if strings.Contains(strings.ToLower(body), tok) {
			t.Errorf("scope.py names %q — the core is stack-agnostic by construction; that knowledge "+
				"belongs in skills/lang-<id>.md and in the extractor it prescribes", tok)
		}
	}
}

// The selftest is the bot's own falsification bench, and --falsify is what makes
// its count mean something: it neutralises each `raise Refusal` in turn and
// demands the bench redden. Measured while writing it, ten guards were exercised
// and seventeen more refusal sites were not — and every run was green.
func TestScopeFalsifiesItsOwnGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("--falsify runs the selftest once per refusal site")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	out, err := exec.Command("python3", "modernize-scope/scope.py", "--falsify").CombinedOutput()
	if err != nil {
		t.Fatalf("scope.py --falsify: %v\n%s", err, out)
	}
}
