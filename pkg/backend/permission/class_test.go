package permission

import (
	"strings"
	"testing"
)

// The class:shell cells, through the real gate: Policy.Evaluate, the same
// evaluation a backend's enforcement seam runs on every tool call.

func TestDenyClassShellBoundsRepl(t *testing.T) {
	p, err := NewPolicy(ModeAsk, nil, nil, []string{"class:shell"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	for _, name := range []string{"repl", "bash", "Bash", "diagnostic_shell", "sh"} {
		decision, matched := p.Evaluate(name, map[string]any{"command": "ls"})
		if decision != Deny {
			t.Errorf("Evaluate(%q) = %v, want Deny", name, decision)
		}
		if matched != "class:shell" {
			t.Errorf("Evaluate(%q) matched %q, want the class row", name, matched)
		}
	}
	// Outside the class the row is inert, bounded by name deliberately:
	// workspace_grep is not grep, and the class is not a catch-all.
	if d, _ := p.Evaluate("workspace_grep", nil); d != Ask {
		t.Errorf("workspace_grep stays on the mode default, got %v", d)
	}
}

// TestDenyBashAloneStillLetsReplThrough pins the documented hole the class
// exists to close: a spelling row bounds its canonical key only. It stays
// green after the change — the class is additive — and would redden if a
// future edit aliased repl onto bash (allow widens, so that would be the
// #1579 shape again).
func TestDenyBashAloneStillLetsReplThrough(t *testing.T) {
	p, err := NewPolicy(ModeAsk, nil, nil, []string{"Bash"})
	if err != nil {
		t.Fatalf("the spelling row parses as before: %v", err)
	}
	if d, matched := p.Evaluate("repl", nil); d != Ask {
		t.Errorf("deny Bash + repl = %v (matched %q), want the mode default Ask — the documented hole", d, matched)
	}
	if d, _ := p.Evaluate("bash", nil); d != Deny {
		t.Errorf("the spelling row still bounds bash itself, got %v", d)
	}
}

// TestAllowClassShellGrantsReplAndBash: allow widens, deliberately. One row
// grants every member.
func TestAllowClassShellGrantsReplAndBash(t *testing.T) {
	p, err := NewPolicy(ModeDeny, []string{"class:shell"}, nil, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	for _, name := range []string{"repl", "bash", "diagnostic_shell"} {
		if d, _ := p.Evaluate(name, nil); d != Allow {
			t.Errorf("Evaluate(%q) = %v, want Allow — the class grant covers every member", name, d)
		}
	}
	if d, _ := p.Evaluate("read_file", nil); d != Deny {
		t.Errorf("read_file is not a member: mode default, got %v want Deny", d)
	}
}

// TestAskClassShellAsksEveryMember: an ask row over the class is an
// approval gate over every member, in every mode (an explicit ask precedes
// the mode default). A class ask is never the bare-exact shape a literal
// allow supersedes (matchBareExactTool skips class rules), so an authored
// literal allow cannot downgrade it.
func TestAskClassShellAsksEveryMember(t *testing.T) {
	p, err := NewPolicy(ModeDeny, nil, []string{"class:shell"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	for _, name := range []string{"repl", "bash", "diagnostic_shell"} {
		if d, matched := p.Evaluate(name, nil); d != Ask {
			t.Errorf("Evaluate(%q) = %v (matched %q), want Ask — the class ask gates every member", name, matched, d)
		}
	}
	p2, err := NewPolicy(ModeDeny, []string{"Bash"}, []string{"class:shell"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if d, _ := p2.Evaluate("repl", nil); d != Ask {
		t.Errorf("a literal allow for bash must not downgrade the class ask (repl = %v, want Ask)", d)
	}
	if d, _ := p2.Evaluate("bash", nil); d != Ask {
		t.Errorf("the class ask outranks the literal allow (bash = %v, want Ask)", d)
	}
}

// TestClassRowAndSpellingRowCoexist: rules are independent — a class row and
// a spelling row overlap without conflict, and each is matched on its own.
func TestClassRowAndSpellingRowCoexist(t *testing.T) {
	p, err := NewPolicy(ModeAsk, nil, nil, []string{"class:shell", "Bash(rm -rf:*)"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if d, matched := p.Evaluate("repl", nil); d != Deny || matched != "class:shell" {
		t.Errorf("the class row bounds repl: got %v matched %q", d, matched)
	}
	if d, _ := p.Evaluate("bash", map[string]any{"command": "ls"}); d != Deny {
		t.Errorf("the class row bounds bash too: got %v", d)
	}
	// The spelling row coexists for its own scoped contract; the class row
	// already answers bare bash first (deny rules match in declaration order).
	if d, matched := p.Evaluate("bash", map[string]any{"command": "rm -rf /"}); d != Deny || matched != "class:shell" {
		t.Errorf("bare rm -rf is denied by the class row: got %v matched %q", d, matched)
	}
}

// TestUnknownClassRefusedNamingTheKnownOnes: a typo'd class is refused at
// authoring (ValidateRule is the parser C154 runs) with the known classes in
// the message — never accepted as a rule that matches nothing.
func TestUnknownClassRefusedNamingTheKnownOnes(t *testing.T) {
	err := ValidateRule("class:shel")
	if err == nil {
		t.Fatal("class:shel must be refused — an unknown class is authoring-refused, not a silent no-op rule")
	}
	if !strings.Contains(err.Error(), "shell") {
		t.Errorf("the refusal must name the known classes, got: %v", err)
	}
	if err := ValidateRule("class:"); err == nil {
		t.Error("a bare class: prefix must be refused")
	}
}

// TestClassRowWithArgScopeComposes: a class row may carry an (arg) pattern,
// matched against each member's summary exactly as a scoped spelling row is.
func TestClassRowWithArgScopeComposes(t *testing.T) {
	p, err := NewPolicy(ModeDeny, nil, nil, []string{"class:shell(git push:*)"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if d, _ := p.Evaluate("bash", map[string]any{"command": "ls"}); d != Deny {
		t.Errorf("the scope must bound the match: bash ls = %v, want the mode default Deny", d)
	}
	if d, _ := p.Evaluate("bash", map[string]any{"command": "git push origin"}); d != Deny {
		t.Errorf("the scope must match its family: git push = %v, want Deny", d)
	}
	if d, _ := p.Evaluate("repl", map[string]any{"code": "git push"}); d != Deny {
		t.Errorf("repl is a member but its summary is not a command: %v, want the mode default Deny", d)
	}
}
