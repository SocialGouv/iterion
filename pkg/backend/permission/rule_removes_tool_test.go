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
		{"*", true, "the tool glob that matches everything"},
		{"Write(docs/**)", false, "a scope bounding a subset"},
		{"Write(/work/docs/**)", false, "an absolute scope bounding a subset"},
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
