package permission

import (
	"strings"
	"testing"
)

func TestParseAnswer(t *testing.T) {
	cases := []struct {
		in            string
		allow, always bool
	}{
		{"allow", true, false},
		{"Allow", true, false},
		{"allow once", true, false},
		{"yes", true, false},
		{"once", true, false},
		{"allow always", true, true},
		{"always", true, true},
		{"ALLOW ALWAYS", true, true},
		{"deny", false, false},
		{"no", false, false},
		{"", false, false},
		{"gibberish", false, false}, // fail-safe: unrecognized = deny
	}
	for _, c := range cases {
		allow, always := ParseAnswer(c.in)
		if allow != c.allow || always != c.always {
			t.Errorf("ParseAnswer(%q) = (%v,%v), want (%v,%v)", c.in, allow, always, c.allow, c.always)
		}
	}
}

func TestGrantRuleFor(t *testing.T) {
	// always → bare tool name (whole-tool grant)
	if got := GrantRuleFor("Bash", map[string]any{"command": "go build"}, true); got != "Bash" {
		t.Errorf("always grant = %q, want Bash", got)
	}
	// once → scoped to the argument so only the identical retry passes
	got := GrantRuleFor("Bash", map[string]any{"command": "go build ./..."}, false)
	if got != "Bash(go build ./...)" {
		t.Errorf("once grant = %q, want Bash(go build ./...)", got)
	}
	// A once-grant must actually authorize the same call when re-evaluated.
	p := mustPolicy(t, ModeAsk, []string{got}, nil, nil)
	if dec, _ := p.Evaluate("Bash", map[string]any{"command": "go build ./..."}); dec != Allow {
		t.Errorf("granted call = %v, want Allow", dec)
	}
}

func TestGrantRuleFor_LongGenericArgumentAuthorizesExactRetry(t *testing.T) {
	command := "git diff -- " + strings.Repeat("bots/shared-planner/path-", 12)
	if len(command) <= 200 {
		t.Fatalf("test command length = %d, want > 200", len(command))
	}
	input := map[string]any{"command": command}

	rule := GrantRuleFor("diagnostic_shell", input, false)
	if strings.Contains(rule, "…") {
		t.Fatalf("once-grant rule was truncated: %q", rule)
	}
	p := mustPolicy(t, ModeAsk, nil, []string{"diagnostic_shell"}, nil)
	p.AddGrantRule(rule)
	if got, _ := p.Evaluate("diagnostic_shell", input); got != Allow {
		t.Errorf("identical long retry = %v, want Allow", got)
	}
	if got, _ := p.Evaluate("diagnostic_shell", map[string]any{"command": command + " --cached"}); got != Ask {
		t.Errorf("different long retry = %v, want Ask", got)
	}
}

func TestDiagnosticShellGrantUsesOnlyTheExactCommand(t *testing.T) {
	command := "iterion validate bots/shared-planner/workflows/hierarchy-epic-author.bot"
	input := map[string]any{
		"command":     command,
		"description": "Validate the saved workflow",
		"timeout":     300000,
	}
	rule := GrantRuleFor("diagnostic_shell", input, false)
	if rule != "diagnostic_shell("+command+")" {
		t.Fatalf("diagnostic once-grant = %q, want command-only scope", rule)
	}
	p := mustPolicy(t, ModeDeny, nil, []string{"diagnostic_shell"}, nil)
	p.AddGrantRule(rule)
	if got, _ := p.Evaluate("diagnostic_shell", map[string]any{"command": command, "description": "Retry"}); got != Allow {
		t.Errorf("same diagnostic command = %v, want Allow despite changed metadata", got)
	}
	if got, _ := p.Evaluate("diagnostic_shell", map[string]any{"command": command + " --strict"}); got != Ask {
		t.Errorf("different diagnostic command = %v, want Ask", got)
	}
}

func TestDenyMessageMentionsTool(t *testing.T) {
	msg := DenyMessage("Bash", map[string]any{"command": "rm -rf /"}, "Bash(rm -rf:*)")
	for _, want := range []string{"Bash", "denied", "rm -rf"} {
		if !contains(msg, want) {
			t.Errorf("DenyMessage missing %q: %s", want, msg)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
