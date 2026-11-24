package commands

import (
	"testing"
)

func TestDoctorCommand(t *testing.T) {
	r := NewRegistry()
	RegisterDiagnosticCommands(r)

	handled, err := r.Execute("/doctor", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDiffCommand(t *testing.T) {
	r := NewRegistry()
	RegisterDiagnosticCommands(r)

	// /diff should work if git is available (no error expected in CI)
	handled, err := r.Execute("/diff", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	// err may be non-nil if not in a git repo — that's acceptable
	_ = err
}

func TestDoctorWithSandboxChecker(t *testing.T) {
	r := NewRegistry()
	RegisterDiagnosticCommands(r)

	type sandboxMock struct {
		enabled bool
	}

	// Pass a struct that doesn't implement SandboxEnabled — should still work
	handled, err := r.Execute("/doctor", &sandboxMock{enabled: true})
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
