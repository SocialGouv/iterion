package permissions

import (
	"errors"
	"testing"
)

func TestPermissionModeOrdering(t *testing.T) {
	// Verify the 5-mode ordering: ReadOnly < WorkspaceWrite < DangerFullAccess < Prompt < Allow
	if ModeReadOnly >= ModeWorkspaceWrite {
		t.Error("ReadOnly should be < WorkspaceWrite")
	}
	if ModeWorkspaceWrite >= ModeDangerFullAccess {
		t.Error("WorkspaceWrite should be < DangerFullAccess")
	}
	if ModeDangerFullAccess >= ModePrompt {
		t.Error("DangerFullAccess should be < Prompt")
	}
	if ModePrompt >= ModeAllow {
		t.Error("Prompt should be < Allow")
	}
}

func TestPermissionModeExplicitValues(t *testing.T) {
	// Verify explicit int assignments (not iota) by checking values directly.
	if ModeReadOnly != 0 {
		t.Errorf("ModeReadOnly = %d, want 0", ModeReadOnly)
	}
	if ModeWorkspaceWrite != 1 {
		t.Errorf("ModeWorkspaceWrite = %d, want 1", ModeWorkspaceWrite)
	}
	if ModeDangerFullAccess != 2 {
		t.Errorf("ModeDangerFullAccess = %d, want 2", ModeDangerFullAccess)
	}
	if ModePrompt != 3 {
		t.Errorf("ModePrompt = %d, want 3", ModePrompt)
	}
	if ModeAllow != 4 {
		t.Errorf("ModeAllow = %d, want 4", ModeAllow)
	}
}

func TestPermissionModeLegacyAliases(t *testing.T) {
	// Verify CLI aliases map to the correct Rust-style modes.
	if ModeDefault != ModePrompt {
		t.Error("ModeDefault should equal ModePrompt")
	}
	if ModeAcceptEdits != ModeWorkspaceWrite {
		t.Error("ModeAcceptEdits should equal ModeWorkspaceWrite")
	}
	if ModeBypassPermissions != ModeAllow {
		t.Error("ModeBypassPermissions should equal ModeAllow")
	}
	if ModePlan != ModeReadOnly {
		t.Error("ModePlan should equal ModeReadOnly")
	}
}

func TestPermissionModeString(t *testing.T) {
	tests := []struct {
		mode PermissionMode
		want string
	}{
		{ModeReadOnly, "read-only"},
		{ModeWorkspaceWrite, "workspace-write"},
		{ModeDangerFullAccess, "danger-full-access"},
		{ModePrompt, "prompt"},
		{ModeAllow, "allow"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestParsePermissionModeRoundTrip(t *testing.T) {
	// Legacy CLI names
	tests := []struct {
		input string
		mode  PermissionMode
	}{
		{"default", ModePrompt},
		{"accept-edits", ModeWorkspaceWrite},
		{"bypass", ModeAllow},
		{"plan", ModeReadOnly},
		{"", ModePrompt},
		// Rust-style names
		{"read-only", ModeReadOnly},
		{"workspace-write", ModeWorkspaceWrite},
		{"danger-full-access", ModeDangerFullAccess},
		{"prompt", ModePrompt},
		{"allow", ModeAllow},
	}
	for _, tt := range tests {
		got, err := ParsePermissionMode(tt.input)
		if err != nil {
			t.Errorf("ParsePermissionMode(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.mode {
			t.Errorf("ParsePermissionMode(%q) = %v, want %v", tt.input, got, tt.mode)
		}
	}
}

func TestParsePermissionModeInvalid(t *testing.T) {
	_, err := ParsePermissionMode("bogus")
	if err == nil {
		t.Error("expected error for invalid permission mode")
	}
}

func TestCheckWithHookOverrideAllow(t *testing.T) {
	m := NewManager(ModeDefault, nil)
	override := &HookPermissionOverride{Decision: DecisionAllow, Reason: "hook says ok"}
	d := m.CheckWithHookOverride("bash", "rm -rf /", override)
	if d != DecisionAllow {
		t.Errorf("expected Allow with hook override, got %v", d)
	}
}

func TestCheckWithHookOverrideDeny(t *testing.T) {
	m := NewManager(ModeBypassPermissions, nil)
	override := &HookPermissionOverride{Decision: DecisionDeny, Reason: "hook blocks"}
	d := m.CheckWithHookOverride("bash", "ls", override)
	if d != DecisionDeny {
		t.Errorf("expected Deny with hook override even in bypass mode, got %v", d)
	}
}

func TestCheckWithHookOverrideNilFallsThrough(t *testing.T) {
	m := NewManager(ModeBypassPermissions, nil)
	d := m.CheckWithHookOverride("bash", "ls", nil)
	if d != DecisionAllow {
		t.Errorf("expected Allow in bypass mode with nil override, got %v", d)
	}
}

func TestCheckWithHookOverridePlanMode(t *testing.T) {
	m := NewManager(ModePlan, nil)
	// No override → Plan mode denies.
	d := m.CheckWithHookOverride("bash", "ls", nil)
	if d != DecisionDeny {
		t.Errorf("expected Deny in plan mode, got %v", d)
	}

	// Override Allow takes precedence over Plan mode.
	override := &HookPermissionOverride{Decision: DecisionAllow}
	d = m.CheckWithHookOverride("bash", "ls", override)
	if d != DecisionAllow {
		t.Errorf("expected Allow with hook override in plan mode, got %v", d)
	}
}

// mockPrompter implements PermissionPrompter for testing.
type mockPrompter struct {
	called bool
}

func (m *mockPrompter) Decide(req *PermissionRequest) PermissionPromptDecision {
	m.called = true
	return PermissionPromptDecision{Allowed: true}
}

func TestPermissionPrompterMockable(t *testing.T) {
	var p PermissionPrompter = &mockPrompter{}
	result := p.Decide(&PermissionRequest{
		ToolName: "bash",
		Input:    "echo hello",
	})
	if !result.Allowed {
		t.Error("mock prompter should allow")
	}
	if !p.(*mockPrompter).called {
		t.Error("mock prompter should have been called")
	}
}

func TestSetPrompter(t *testing.T) {
	m := NewManager(ModeDefault, nil)
	mp := &mockPrompter{}
	m.SetPrompter(mp)
	if m.prompter != mp {
		t.Error("prompter should be set")
	}
}

// --- QUALITY-1: MatchesAskRule tests ---

func TestMatchesAskRule_WithPolicy(t *testing.T) {
	m := NewManager(ModeDefault, nil)
	policy := NewPermissionPolicy(ModeReadOnly).
		WithToolRequirement("bash", ModeDangerFullAccess).
		WithPermissionRules(nil, nil, []string{"bash(git:*)"})
	m.SetPolicy(policy)

	// Should match: bash tool with git-related input.
	if !m.MatchesAskRule("bash", `{"command":"git status"}`) {
		t.Error("MatchesAskRule should match bash tool with git input against ask rule bash(git:*)")
	}

	// Should not match: bash tool with non-git input.
	if m.MatchesAskRule("bash", `{"command":"echo hi"}`) {
		t.Error("MatchesAskRule should NOT match bash tool with non-git input")
	}

	// Should not match: different tool.
	if m.MatchesAskRule("read_file", `{"path":"/tmp/x"}`) {
		t.Error("MatchesAskRule should NOT match non-bash tool")
	}
}

func TestMatchesAskRule_WithLegacyRuleset(t *testing.T) {
	rules := &Ruleset{
		Rules: []Rule{
			{Tool: "bash", Pattern: "", Decision: DecisionAsk, RawDecision: "ask"},
		},
	}
	m := NewManager(ModeDefault, rules)

	if !m.MatchesAskRule("bash", "echo hi") {
		t.Error("MatchesAskRule should match via legacy ruleset")
	}
	if m.MatchesAskRule("read_file", "/tmp/x") {
		t.Error("MatchesAskRule should NOT match unrelated tool via legacy ruleset")
	}
}

func TestMatchesAskRule_NoRules(t *testing.T) {
	m := NewManager(ModeDefault, nil)
	if m.MatchesAskRule("bash", "echo hi") {
		t.Error("MatchesAskRule should return false with no rules")
	}
}

func TestSetPolicyDelegation(t *testing.T) {
	m := NewManager(ModeDefault, nil)

	// Without policy, bash should Ask in default mode.
	d := m.Check("bash", "{}")
	if d != DecisionAsk {
		t.Errorf("expected Ask without policy, got %v", d)
	}

	// Set a policy with Allow mode; now bash should be allowed.
	policy := NewPermissionPolicy(ModeAllow)
	m.SetPolicy(policy)

	d = m.Check("bash", "{}")
	if d != DecisionAllow {
		t.Errorf("expected Allow with Allow-mode policy, got %v", d)
	}

	// Clear policy; should fall back to legacy logic.
	m.SetPolicy(nil)
	d = m.Check("bash", "{}")
	if d != DecisionAsk {
		t.Errorf("expected Ask after clearing policy, got %v", d)
	}
}

// stubClassifier is reused from dontask_auto_test.go — same package.

func TestSetClassifierAllowShortCircuitsLegacy(t *testing.T) {
	m := NewManager(ModeDefault, nil)
	if d := m.Check("bash", "{}"); d != DecisionAsk {
		t.Fatalf("expected Ask before classifier, got %v", d)
	}

	stub := &stubClassifier{decision: DecisionAllow}
	m.SetClassifier(stub)

	if d := m.Check("bash", `{"command":"echo hi"}`); d != DecisionAllow {
		t.Errorf("expected Allow from classifier, got %v", d)
	}
	if stub.calls != 1 {
		t.Errorf("expected classifier invoked once, got %d", stub.calls)
	}

	// Clearing returns to legacy behavior.
	m.SetClassifier(nil)
	if d := m.Check("bash", "{}"); d != DecisionAsk {
		t.Errorf("expected Ask after clearing classifier, got %v", d)
	}
}

func TestSetClassifierDenyShortCircuitsLegacy(t *testing.T) {
	rules := &Ruleset{
		Rules: []Rule{{Tool: "bash", Decision: DecisionAllow}},
	}
	m := NewManager(ModeDefault, rules)

	// Legacy would Allow because of the rule.
	if d := m.Check("bash", "{}"); d != DecisionAllow {
		t.Fatalf("setup: legacy path should Allow via ruleset, got %v", d)
	}

	stub := &stubClassifier{decision: DecisionDeny}
	m.SetClassifier(stub)

	if d := m.Check("bash", `{"command":"rm -rf /"}`); d != DecisionDeny {
		t.Errorf("expected Deny from classifier, got %v", d)
	}
}

func TestSetClassifierAskFallsThroughToLegacy(t *testing.T) {
	rules := &Ruleset{Rules: []Rule{{Tool: "bash", Decision: DecisionAllow}}}
	m := NewManager(ModeDefault, rules)

	stub := &stubClassifier{decision: DecisionAsk}
	m.SetClassifier(stub)

	// Classifier returns Ask → legacy ruleset says Allow.
	if d := m.Check("bash", "{}"); d != DecisionAllow {
		t.Errorf("expected Allow from legacy after classifier Ask, got %v", d)
	}
	if stub.calls != 1 {
		t.Errorf("expected classifier invoked once, got %d", stub.calls)
	}
}

func TestSetClassifierErrorFallsThroughToLegacy(t *testing.T) {
	m := NewManager(ModeBypassPermissions, nil)
	stub := &stubClassifier{decision: DecisionDeny, err: errors.New("model unavailable")}
	m.SetClassifier(stub)

	// Classifier error must NOT short-circuit; legacy ModeBypassPermissions Allows.
	if d := m.Check("bash", "{}"); d != DecisionAllow {
		t.Errorf("expected Allow from legacy after classifier error, got %v", d)
	}
}

func TestPolicyTakesPrecedenceOverClassifier(t *testing.T) {
	m := NewManager(ModeDefault, nil)

	// Policy Allows everything in ModeAllow.
	m.SetPolicy(NewPermissionPolicy(ModeAllow))
	// Classifier would Deny.
	m.SetClassifier(&stubClassifier{decision: DecisionDeny})

	if d := m.Check("bash", "{}"); d != DecisionAllow {
		t.Errorf("expected Allow from policy (precedence), got %v", d)
	}
}

func TestCheckWithHookOverrideClassifier(t *testing.T) {
	m := NewManager(ModeDefault, nil)
	stub := &stubClassifier{decision: DecisionAllow}
	m.SetClassifier(stub)

	// No override: classifier Allows.
	if d := m.CheckWithHookOverride("bash", "{}", nil); d != DecisionAllow {
		t.Errorf("expected Allow from classifier without override, got %v", d)
	}

	// Hook override Deny wins over classifier Allow.
	override := &HookPermissionOverride{Decision: DecisionDeny, Reason: "test"}
	if d := m.CheckWithHookOverride("bash", "{}", override); d != DecisionDeny {
		t.Errorf("expected Deny from hook override, got %v", d)
	}
}
