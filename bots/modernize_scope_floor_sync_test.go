package bots

import (
	"os"
	"strings"
	"testing"
)

// The floor exists twice: inlined in main.bot's inventory_floor node (the copy
// that actually RUNS) and as floor.py (the copy a human reviews). "Keep the two
// in sync" is a sentence, not a check, and a sentence drifts.
//
// The failure this prevents is quiet: a fix lands in the reviewable copy only,
// review passes, and the gate keeps running the old logic — because the running
// copy is exactly the one nobody reads.
//
// The comparison is BYTE FOR BYTE, and that is the point. Pinning a proxy (the
// set of function names, the report's fields) would establish something NEAR
// what it claims; golden-master's copies drifted by sixty-two lines while
// exactly that kind of check stayed green.
//
// Verbatim comparison is possible because the running copy is the reviewable
// one indented by four, under a preamble that binds the graph's vars. That one
// mechanical difference is undone here; nothing else may differ.
const (
	floorBegin       = "# ---- shared body below (kept byte-identical with the inlined copy) ----"
	floorEnd         = "# ---- shared body above ----"
	floorPreambleEnd = "    # ---- inlined floor below"
	floorEpilogue    = "    # ---- inlined floor above"
)

func readScopeFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func between(t *testing.T, text, start, end, what string) string {
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

func dedent(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "    ")
	}
	return strings.Join(lines, "\n")
}

func TestScopeFloorCopiesStayInSync(t *testing.T) {
	standalone := between(t, readScopeFile(t, "modernize-scope/floor.py"),
		floorBegin, floorEnd, "floor.py")
	inlined := dedent(between(t, readScopeFile(t, "modernize-scope/main.bot"),
		floorPreambleEnd, floorEpilogue, "main.bot"))

	if inlined == standalone {
		return
	}
	a, b := strings.Split(inlined, "\n"), strings.Split(standalone, "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := "", ""
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			t.Fatalf("the two copies of the floor have diverged at body line %d.\n"+
				"  main.bot   (the copy that RUNS):     %q\n"+
				"  floor.py   (the copy REVIEWED):      %q\n"+
				"Regenerate the inlined copy: python3 bots/modernize-scope/sync-floor.py", i+1, x, y)
		}
	}
	t.Fatalf("the copies differ in length: %d lines inlined, %d standalone", len(a), len(b))
}

// A floor that names a language is not a floor: the stack-specific half belongs
// to a lang-<id> skill and to the extractor it prescribes. catalog_universality
// greps the .bot's typed var defaults; it does not read a bundled script, so
// this covers the script the node actually runs.
func TestScopeFloorNamesNoStack(t *testing.T) {
	body := readScopeFile(t, "modernize-scope/floor.py")
	// Split so this test does not match itself.
	for _, tok := range []string{"spr" + "ing", "ma" + "ven", "gra" + "dle", "np" + "m", "ya" + "rn",
		"car" + "go", "pip" + "env", "go" + "mod", "src/main/" + "java", "package." + "json"} {
		if strings.Contains(strings.ToLower(body), tok) {
			t.Errorf("floor.py names %q — the floor is stack-agnostic by construction; that knowledge "+
				"belongs in skills/lang-<id>.md and in the extractor it prescribes", tok)
		}
	}
}
