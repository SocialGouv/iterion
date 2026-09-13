package delegate

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

func diagnosticShellPolicy(t *testing.T, deny []string) *permission.Policy {
	t.Helper()
	p, err := permission.NewPolicy(permission.ModeDeny, nil, []string{"diagnostic_shell"}, deny)
	if err != nil {
		t.Fatalf("new diagnostic-shell policy: %v", err)
	}
	return p
}

func TestClaudeDiagnosticShellApprovalIsExactAndSingleLine(t *testing.T) {
	p := diagnosticShellPolicy(t, nil)
	task := Task{DiagnosticShell: true, Permission: p}
	input := map[string]any{
		"command":     "python3 -m pytest -q tests/test_planner.py",
		"description": "Run the targeted planner test",
		"timeout":     120000,
	}

	tool := claudePermissionToolName(task, "Bash", input)
	if tool != "diagnostic_shell" {
		t.Fatalf("mapped tool = %q, want diagnostic_shell", tool)
	}
	if got, _ := p.Evaluate(tool, input); got != permission.Ask {
		t.Fatalf("initial diagnostic command = %v, want Ask", got)
	}

	rule, approved := permission.GrantFromAnswer("allow", tool, input)
	if !approved {
		t.Fatal("allow must produce a diagnostic-shell grant")
	}
	p.AddGrantRule(rule)

	retry := map[string]any{
		"command":     input["command"],
		"description": "Re-run after the operator approved it",
		"timeout":     300000,
	}
	if got, _ := p.Evaluate(claudePermissionToolName(task, "Bash", retry), retry); got != permission.Allow {
		t.Errorf("same command with changed metadata = %v, want Allow", got)
	}

	different := map[string]any{"command": "python3 -m pytest -q tests/test_other.py", "description": "Different test"}
	if got, _ := p.Evaluate(claudePermissionToolName(task, "Bash", different), different); got != permission.Ask {
		t.Errorf("different command = %v, want Ask rather than inherited approval", got)
	}

	multiline := map[string]any{"command": input["command"].(string) + "\ncat ~/.ssh/id_rsa"}
	if tool := claudePermissionToolName(task, "Bash", multiline); tool != "Bash" {
		t.Errorf("multiline native Bash mapped to %q, want raw Bash", tool)
	} else if got, _ := p.Evaluate(tool, multiline); got != permission.Deny {
		t.Errorf("multiline native Bash = %v, want Deny", got)
	}
}

func TestClaudeDiagnosticShellNeverActivatesWithoutItsDeclaredOptIn(t *testing.T) {
	input := map[string]any{"command": "python3 -m pytest -q tests/test_planner.py"}
	for _, tc := range []struct {
		name string
		task Task
	}{
		{
			name: "review task has no diagnostic opt-in",
			task: Task{Permission: diagnosticShellPolicy(t, nil), AllowedTools: []string{"read_file", "glob"}},
		},
		{
			name: "native Bash declaration alone is insufficient",
			task: Task{Permission: diagnosticShellPolicy(t, nil), AllowedTools: []string{"Bash"}},
		},
		{
			name: "explicit Bash deny wins over diagnostic opt-in",
			task: Task{DiagnosticShell: true, Permission: diagnosticShellPolicy(t, []string{"Bash"})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tool := claudePermissionToolName(tc.task, "Bash", input); tool != "Bash" {
				t.Fatalf("mapped tool = %q, want raw Bash", tool)
			}
			if got, _ := tc.task.Permission.Evaluate("Bash", input); got != permission.Deny {
				t.Errorf("raw Bash = %v, want Deny", got)
			}
		})
	}
}
