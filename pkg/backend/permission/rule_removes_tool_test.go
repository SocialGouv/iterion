package permission

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// writeProbes are calls of `Write` a reader asking "is this tool gone" must
// have answered for: an absolute path, one under a scoped rule's root, and a
// relative one. A rule that removes the tool denies all of them.
var writeProbes = []map[string]any{
	{"file_path": "/work/notes.md", "content": "x"},
	{"file_path": "/work/docs/notes.md", "content": "x"},
	{"file_path": "relative.md", "content": "x"},
}

// TestRuleRemovesToolAgreesWithTheGate holds RuleRemovesTool to what Evaluate
// decides, which is the only reading that matters: a reader credits a
// capability it believes survives the gate, so a predicate that disagreed
// with the gate would credit a tool no call can use (or refuse one every call
// can).
//
// The agreement is measured in `ask` mode, where nothing but a rule denies:
// a rule RuleRemovesTool accepts must deny every probe; one it refuses must
// leave at least one call through.
func TestRuleRemovesToolAgreesWithTheGate(t *testing.T) {
	cases := []struct {
		rule  string
		gone  bool
		about string
	}{
		{"Write", true, "a bare tool rule bounds every call"},
		{"write_file", true, "a native spelling folds onto the same tool"},
		{"Write(**)", true, "a scope that leaves no path out"},
		{"Write(*)", true, "the single-star catch-all"},
		{"Write(:*)", true, "the prefix idiom with an EMPTY prefix, which compiles to `^`"},
		{"*", true, "the tool glob that matches everything"},
		{"Write*", true, "a tool glob matching the name the gate sees"},
		{"Write(docs/**)", false, "a scope bounding a subset"},
		{"Write(/work/docs/**)", false, "an absolute scope bounding a subset"},
		{"Write()", false, "an empty scope bounds the empty argument alone"},
		{"write_*", false, "a tool glob matching neither the name nor the canonical key"},
		{"Edit", false, "another tool entirely"},
		{"Write(", false, "an unparseable rule bounds nothing"},
	}
	for _, c := range cases {
		got := RuleRemovesTool(c.rule, "Write")
		if got != c.gone {
			t.Errorf("RuleRemovesTool(%q, Write) = %v, want %v (%s)", c.rule, got, c.gone, c.about)
			continue
		}
		if c.rule == "Write(" {
			// NewPolicy refuses the malformed rule, so there is no gate to
			// compare against — the predicate's "bounds nothing" is the
			// whole claim.
			continue
		}
		p := mustPolicy(t, ModeAsk, nil, nil, []string{c.rule})
		denied := 0
		for _, in := range writeProbes {
			if d, _ := p.Evaluate("Write", in); d == Deny {
				denied++
			}
		}
		if c.gone && denied != len(writeProbes) {
			t.Errorf("%q: RuleRemovesTool says the tool is gone, but the gate denied only %d of %d calls (%s)", c.rule, denied, len(writeProbes), c.about)
		}
		if !c.gone && denied == len(writeProbes) {
			t.Errorf("%q: RuleRemovesTool says the tool is left, but the gate denied every call (%s)", c.rule, c.about)
		}
	}
}

// TestAClassRowRemovesEveryMember reads the one class the registry ships
// (toolcatalog.ClassShell): a bare class row bounds every member it covers,
// present and future, so a reader asking "is the shell gone" must see it.
func TestAClassRowRemovesEveryMember(t *testing.T) {
	rule := "class:" + toolcatalog.ClassShell
	if !RuleRemovesTool(rule, "Bash") {
		t.Fatalf("RuleRemovesTool(%q, Bash) = false — a bare class row bounds every member", rule)
	}
	p := mustPolicy(t, ModeAsk, nil, nil, []string{rule})
	if d, _ := p.Evaluate("Bash", map[string]any{"command": "ls"}); d != Deny {
		t.Errorf("the gate did not deny a shell call under %q: got %v", rule, d)
	}
	// A scope narrows it back to a subset, and the predicate must follow.
	if RuleRemovesTool(rule+"(git diff:*)", "Bash") {
		t.Error("a SCOPED class row was read as removing the tool outright")
	}
}

// TestLeavesToolAgreesWithTheGateOverEveryModeAndSpelling holds the
// existential question to the gate that answers the per-call one: whatever
// LeavesTool says about a tool concept, Evaluate must agree over the calls of
// the spellings offered — one non-Deny verdict where the concept is left,
// none where it is gone.
//
// The spelling set is the half Evaluate cannot infer: a declaration reaches
// the gate under the roster's name, so a GLOB row written in one spelling
// bounds the concept on one backend and not on the other. Both are read here,
// and the concept survives when any spelling of it does — the same reading
// evaluateForwardedCall performs for a call it knows under several names.
func TestLeavesToolAgreesWithTheGateOverEveryModeAndSpelling(t *testing.T) {
	cases := []struct {
		what             string
		mode             Mode
		allow, ask, deny []string
		spellings        []string
		left             bool
	}{
		{"ask + a glob matching the native spelling offered", ModeAsk, nil, nil, []string{"Write*"}, []string{"Write"}, false},
		{"ask + a glob matching no spelling offered", ModeAsk, nil, nil, []string{"Write*"}, []string{"write_file"}, true},
		{"ask + a glob matching ONE of two spellings", ModeAsk, nil, nil, []string{"Edit*"}, []string{"Edit", "MultiEdit"}, true},
		{"ask + a bare row, which folds every spelling onto the key", ModeAsk, nil, nil, []string{"Write"}, []string{"Write", "write_file"}, false},
		{"ask + a deny scoped to a subset, which leaves the other calls", ModeAsk, nil, nil, []string{"Write(/work/docs/**)"}, []string{"Write"}, true},
		{"ask + a deny scoped to every call", ModeAsk, nil, nil, []string{"Write(**)"}, []string{"Write"}, false},
		{"deny + a glob covering the native spelling offered", ModeDeny, []string{"Write*"}, nil, nil, []string{"Write"}, true},
		{"deny + a glob covering no spelling offered", ModeDeny, []string{"Write*"}, nil, nil, []string{"write_file"}, false},
		{"deny + an ask row covering the spelling", ModeDeny, nil, []string{"Write"}, nil, []string{"Write"}, true},
		{"deny + a scoped allow, which leaves some call", ModeDeny, []string{"Write(/work/docs/**)"}, nil, nil, []string{"Write"}, true},
		{"deny + no row at all", ModeDeny, nil, nil, nil, []string{"Write"}, false},
	}
	for _, c := range cases {
		p := mustPolicy(t, c.mode, c.allow, c.ask, c.deny)
		if got := p.LeavesTool(c.spellings...); got != c.left {
			t.Errorf("%s: LeavesTool(%v) = %v, want %v", c.what, c.spellings, got, c.left)
			continue
		}
		passed := 0
		for _, name := range c.spellings {
			for _, in := range writeProbes {
				if d, _ := p.Evaluate(name, in); d != Deny {
					passed++
				}
			}
		}
		if c.left && passed == 0 {
			t.Errorf("%s: LeavesTool said the concept is left, but the gate denied every call of every spelling %v", c.what, c.spellings)
		}
		if !c.left && passed != 0 {
			t.Errorf("%s: LeavesTool said the concept is gone, but the gate let %d call(s) of %v through", c.what, passed, c.spellings)
		}
	}
	// No spelling is no capability: an empty call must not read as "left", or
	// a concept the backend has no tool for would be credited.
	if mustPolicy(t, ModeDeny, []string{"*"}, nil, nil).LeavesTool() {
		t.Error("LeavesTool() with no spelling was read as a capability")
	}
	// A disabled gate leaves everything, spellings or not.
	if !(&Policy{}).LeavesTool("Write") {
		t.Error("a disabled policy (mode off) bounded a tool")
	}
	if !(*Policy)(nil).LeavesTool("Write") {
		t.Error("a nil policy bounded a tool")
	}
}

// TestRuleCoversIsTheWiderQuestion keeps the two predicates apart: coverage
// asks "does any row bound this tool" (what a `deny` mode's allow list is
// read for — a scoped grant still lets some call through), removal asks "is
// every call bound".
func TestRuleCoversIsTheWiderQuestion(t *testing.T) {
	const scoped = "Write(docs/**)"
	if !RuleCovers(scoped, "Write") {
		t.Errorf("RuleCovers(%q, Write) = false — a scoped row does name the tool", scoped)
	}
	if RuleRemovesTool(scoped, "Write") {
		t.Errorf("RuleRemovesTool(%q, Write) = true — a scoped row bounds a subset", scoped)
	}
}
