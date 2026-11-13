package commands

import (
	"fmt"
	"runtime"
	"testing"
)

// mockContextLoop implements contextLoop for testing.
type mockContextLoop struct {
	files     []string
	breakdown map[string]int
	cleared   bool
}

func (m *mockContextLoop) ListContextFiles() []string            { return m.files }
func (m *mockContextLoop) ContextTokenBreakdown() map[string]int { return m.breakdown }
func (m *mockContextLoop) ClearContext()                         { m.cleared = true }

// mockRewindLoop implements rewindLoop for testing.
type mockRewindLoop struct {
	rewound int
}

func (m *mockRewindLoop) RewindSteps(n int) error { m.rewound = n; return nil }

// mockClipboardLoop implements clipboardLoop for testing.
type mockClipboardLoop struct {
	last string
	full string
}

func (m *mockClipboardLoop) GetLastOutput() string       { return m.last }
func (m *mockClipboardLoop) GetFullConversation() string { return m.full }

func TestFilesCommand(t *testing.T) {
	r := NewRegistry()
	RegisterContextCommands(r)

	ml := &mockContextLoop{
		files: []string{"main.go", "lib.go"},
	}

	handled, err := r.Execute("/files", ml)
	if !handled {
		t.Fatal("expected /files to be handled")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Empty files
	ml.files = nil
	handled, err = r.Execute("/files", ml)
	if !handled || err != nil {
		t.Fatalf("expected handled with no error, got handled=%v err=%v", handled, err)
	}
}

func TestRewindCommand(t *testing.T) {
	r := NewRegistry()
	RegisterContextCommands(r)

	ml := &mockRewindLoop{}

	t.Run("default step", func(t *testing.T) {
		_, err := r.Execute("/rewind", ml)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ml.rewound != 1 {
			t.Fatalf("expected 1 step, got %d", ml.rewound)
		}
	})

	t.Run("explicit steps", func(t *testing.T) {
		_, err := r.Execute("/rewind 3", ml)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ml.rewound != 3 {
			t.Fatalf("expected 3 steps, got %d", ml.rewound)
		}
	})

	t.Run("invalid steps", func(t *testing.T) {
		_, err := r.Execute("/rewind abc", ml)
		if err == nil {
			t.Fatal("expected error for invalid step count")
		}
	})

	t.Run("zero steps", func(t *testing.T) {
		_, err := r.Execute("/rewind 0", ml)
		if err == nil {
			t.Fatal("expected error for zero steps")
		}
	})
}

func TestBranchValidation(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"main", true},
		{"feature/my-branch", true},
		{"fix_123", true},
		{"release-1.0", true},
		{"", false},
		{"-leading-dash", false},
		{".leading-dot", false},
		{"has spaces", false},
		{"has..double-dots", false},
		{"name.lock", false},
		{"special@char", false},
		{"special!char", false},
		{"under_score.ok", true},
		{"UPPERCASE", true},
		{"a/b/c", true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.name), func(t *testing.T) {
			got := isValidBranchName(tt.name)
			if got != tt.valid {
				t.Errorf("isValidBranchName(%q) = %v, want %v", tt.name, got, tt.valid)
			}
		})
	}
}

func TestCopyCommand(t *testing.T) {
	r := NewRegistry()
	RegisterContextCommands(r)

	ml := &mockClipboardLoop{last: "hello", full: "full conversation"}

	// Test that /copy with invalid arg gives usage
	handled, err := r.Execute("/copy invalid", ml)
	if !handled {
		t.Fatal("expected /copy to be handled")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Test empty content
	ml.last = ""
	handled, err = r.Execute("/copy last", ml)
	if !handled || err != nil {
		t.Fatalf("expected handled with no error, got handled=%v err=%v", handled, err)
	}
}

func TestClipboardCmd(t *testing.T) {
	bin, args, err := clipboardCmd()

	switch runtime.GOOS {
	case "darwin":
		if err != nil {
			t.Fatalf("unexpected error on darwin: %v", err)
		}
		if bin != "pbcopy" {
			t.Fatalf("expected pbcopy, got %q", bin)
		}
		if args != nil {
			t.Fatalf("expected nil args, got %v", args)
		}
	case "linux":
		// On linux, either xclip/xsel is found or we get an error.
		// All outcomes are valid depending on the environment.
		if err != nil {
			// No clipboard tool installed — acceptable in CI.
			t.Logf("no clipboard tool: %v", err)
			return
		}
		if bin != "xclip" && bin != "xsel" {
			t.Fatalf("expected xclip or xsel, got %q", bin)
		}
	case "windows":
		if err != nil {
			t.Fatalf("unexpected error on windows: %v", err)
		}
		if bin != "clip.exe" {
			t.Fatalf("expected clip.exe, got %q", bin)
		}
	default:
		if err == nil {
			t.Fatal("expected error on unsupported platform")
		}
	}
}
