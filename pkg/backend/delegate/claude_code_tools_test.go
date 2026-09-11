package delegate

import (
	"slices"
	"testing"
)

func TestClaudeNativeToolRestriction_ReadGlobReviewer(t *testing.T) {
	disallowed := claudeNativeDisallowedTools([]string{"read_file", "glob"}, false)

	for _, want := range []string{
		"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "Task", "WebFetch", "WebSearch",
	} {
		if !slices.Contains(disallowed, want) {
			t.Errorf("restricted Read/Glob reviewer leaves native %q available; disallowed = %v", want, disallowed)
		}
	}
	for _, want := range []string{"Read", "Glob"} {
		if slices.Contains(disallowed, want) {
			t.Errorf("restricted Read/Glob reviewer disables required native %q; disallowed = %v", want, disallowed)
		}
	}
}

func TestClaudeNativeToolRestriction_DiagnosticShellOptIn(t *testing.T) {
	withoutOptIn := claudeNativeDisallowedTools([]string{"diagnostic_shell"}, false)
	if !slices.Contains(withoutOptIn, "Bash") {
		t.Fatal("diagnostic_shell without the task opt-in leaves Bash available")
	}

	withOptIn := claudeNativeDisallowedTools([]string{"diagnostic_shell"}, true)
	if slices.Contains(withOptIn, "Bash") {
		t.Fatal("diagnostic-shell task opt-in disables the native Bash bridge")
	}
}
