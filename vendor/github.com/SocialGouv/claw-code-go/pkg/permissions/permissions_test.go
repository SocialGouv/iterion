package permissions_test

import (
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/permissions"
)

func TestTypeAliasesCompile(t *testing.T) {
	// Verify every re-exported type can be instantiated.
	var _ permissions.PermissionMode
	var _ permissions.Decision
	var _ permissions.Scope
	var _ permissions.Rule
	var _ permissions.Ruleset
	var _ *permissions.Manager
	var _ *permissions.PermissionPolicy
	var _ permissions.PermissionOverride
	var _ permissions.PermissionContext
	var _ permissions.PermissionRequest
	var _ permissions.PermissionPromptDecision
	var _ permissions.PermissionOutcome
	var _ permissions.PermissionPrompter
	var _ *permissions.HookPermissionOverride
}

func TestNewManagerAndCheck(t *testing.T) {
	m := permissions.NewManager(permissions.ModeBypassPermissions, nil)
	if d := m.Check("anything", ""); d != permissions.DecisionAllow {
		t.Fatalf("bypass mode should allow, got %v", d)
	}

	m2 := permissions.NewManager(permissions.ModePlan, nil)
	if d := m2.Check("bash", "rm -rf /"); d == permissions.DecisionAllow {
		t.Fatal("plan mode should deny execution, got Allow")
	}
}

func TestParsePermissionModeRoundTrip(t *testing.T) {
	cases := []struct {
		input string
		want  permissions.PermissionMode
	}{
		{"default", permissions.ModePrompt},
		{"prompt", permissions.ModePrompt},
		{"accept-edits", permissions.ModeWorkspaceWrite},
		{"workspace-write", permissions.ModeWorkspaceWrite},
		{"bypass", permissions.ModeAllow},
		{"allow", permissions.ModeAllow},
		{"plan", permissions.ModeReadOnly},
		{"read-only", permissions.ModeReadOnly},
		{"danger-full-access", permissions.ModeDangerFullAccess},
	}
	for _, tc := range cases {
		got, err := permissions.ParsePermissionMode(tc.input)
		if err != nil {
			t.Errorf("ParsePermissionMode(%q) error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParsePermissionMode(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestModeConstants(t *testing.T) {
	if permissions.ModeDefault != permissions.ModePrompt {
		t.Error("ModeDefault != ModePrompt")
	}
	if permissions.ModeAcceptEdits != permissions.ModeWorkspaceWrite {
		t.Error("ModeAcceptEdits != ModeWorkspaceWrite")
	}
	if permissions.ModeBypassPermissions != permissions.ModeAllow {
		t.Error("ModeBypassPermissions != ModeAllow")
	}
	if permissions.ModePlan != permissions.ModeReadOnly {
		t.Error("ModePlan != ModeReadOnly")
	}
}

func TestRulesetFromLists(t *testing.T) {
	rs := permissions.RulesetFromLists([]string{"read_file"}, []string{"bash"})
	d, ok := rs.Match("read_file", "")
	if !ok || d != permissions.DecisionAllow {
		t.Error("expected read_file to be allowed")
	}
	d, ok = rs.Match("bash", "")
	if !ok || d != permissions.DecisionDeny {
		t.Error("expected bash to be denied")
	}
}

func TestNewPermissionPolicy(t *testing.T) {
	p := permissions.NewPermissionPolicy(permissions.ModeAllow)
	if p.ActiveMode() != permissions.ModeAllow {
		t.Errorf("ActiveMode = %v, want %v", p.ActiveMode(), permissions.ModeAllow)
	}
	outcome := p.Authorize("anything", "", nil)
	if !outcome.Allowed {
		t.Error("ModeAllow policy should authorize any tool")
	}
}
