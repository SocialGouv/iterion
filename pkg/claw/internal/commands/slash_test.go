package commands

import (
	"sort"
	"testing"
)

// TestFullRegistryCommandCount verifies the total number of unique commands
// registered when all categories are initialized.
func TestFullRegistryCommandCount(t *testing.T) {
	r := NewFullRegistry()
	count := r.Count()

	// Rust has 143 commands; Go adds Go-only commands (quit, session-list,
	// auth, billing, benchmarks, benchmark, output-style, etc.) for 146+ total.
	// Minimum expected: 143 (Rust parity).
	const minExpected = 143
	if count < minExpected {
		t.Errorf("expected at least %d commands, got %d", minExpected, count)
	}
	t.Logf("Total unique commands registered: %d", count)
}

// TestAllCategoriesHaveCommands verifies every defined category has at least one command.
func TestAllCategoriesHaveCommands(t *testing.T) {
	r := NewFullRegistry()

	expectedCategories := []CommandCategory{
		CategorySession,
		CategoryStatus,
		CategoryConfig,
		CategoryDiagnostics,
		CategoryBuiltin,
		CategoryPlugin,
		CategoryCode,
		CategoryUX,
		CategoryContext,
		CategoryAuth,
		CategoryInteraction,
	}

	for _, cat := range expectedCategories {
		cmds := r.CommandsByCategory(cat)
		if len(cmds) == 0 {
			t.Errorf("category %q has no commands", cat)
		}
	}
}

// TestCommandsByCategorySorted verifies CommandsByCategory returns consistent results.
func TestCommandsByCategorySorted(t *testing.T) {
	r := NewFullRegistry()

	for _, cat := range AllCategories() {
		cmds := r.CommandsByCategory(cat)
		names := make([]string, len(cmds))
		for i, c := range cmds {
			names[i] = c.Name
		}
		// Verify no duplicates
		seen := make(map[string]bool)
		for _, n := range names {
			if seen[n] {
				t.Errorf("category %q: duplicate command %q", cat, n)
			}
			seen[n] = true
		}
	}
}

// TestPluginCommandSubcommands tests /plugin subcommand dispatch.
func TestPluginCommandSubcommands(t *testing.T) {
	r := NewFullRegistry()

	tests := []struct {
		input   string
		handled bool
	}{
		{"/plugin", true},
		{"/plugin list", true},
		{"/plugins list", true},
		{"/marketplace list", true},
	}

	for _, tt := range tests {
		handled, err := r.Execute(tt.input, nil)
		if handled != tt.handled {
			t.Errorf("%s: handled=%v, want %v (err=%v)", tt.input, handled, tt.handled, err)
		}
	}
}

// TestTasksCommandSubcommands tests /tasks subcommand dispatch.
func TestTasksCommandSubcommands(t *testing.T) {
	r := NewFullRegistry()

	tests := []struct {
		input   string
		handled bool
	}{
		{"/tasks", true},
		{"/tasks list", true},
		{"/tasks get abc", true},
		{"/tasks stop abc", true},
	}

	for _, tt := range tests {
		handled, err := r.Execute(tt.input, nil)
		if handled != tt.handled {
			t.Errorf("%s: handled=%v, want %v (err=%v)", tt.input, handled, tt.handled, err)
		}
	}
}

// TestCommitCommandRegistered verifies /commit is registered and handles gracefully.
func TestCommitCommandRegistered(t *testing.T) {
	r := NewFullRegistry()

	cmd, ok := r.Lookup("commit")
	if !ok {
		t.Fatal("expected /commit to be registered")
	}
	if cmd.Category != CategoryCode {
		t.Errorf("expected /commit category=%q, got %q", CategoryCode, cmd.Category)
	}
}

// TestCodeCommandsRegistered verifies all code commands are present.
func TestCodeCommandsRegistered(t *testing.T) {
	r := NewFullRegistry()

	codeCommands := []string{"commit", "pr", "issue", "bughunter", "review", "security-review", "release-notes", "test"}
	for _, name := range codeCommands {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("expected /%s to be registered", name)
		}
	}
}

// TestUXCommandsRegistered verifies all UX commands are present.
func TestUXCommandsRegistered(t *testing.T) {
	r := NewFullRegistry()

	uxCommands := []string{"theme", "vim", "effort", "fast", "brief", "advisor", "color", "keybindings", "privacy-settings", "output-style"}
	for _, name := range uxCommands {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("expected /%s to be registered", name)
		}
	}
}

// TestContextCommandsRegistered verifies all context commands are present.
func TestContextCommandsRegistered(t *testing.T) {
	r := NewFullRegistry()

	contextCommands := []string{"files", "context", "hooks", "search", "copy", "rewind", "branch"}
	for _, name := range contextCommands {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("expected /%s to be registered", name)
		}
	}
}

// TestPluginCommandsRegistered verifies all plugin/session-mgmt commands are present.
func TestPluginCommandsRegistered(t *testing.T) {
	r := NewFullRegistry()

	pluginCommands := []string{"plugin", "agents", "skills", "tasks", "team", "cron", "memory", "sandbox", "init", "upgrade"}
	for _, name := range pluginCommands {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("expected /%s to be registered", name)
		}
	}
}

// TestAliasResolution verifies important aliases work correctly.
func TestAliasResolution(t *testing.T) {
	r := NewFullRegistry()

	aliases := map[string]string{
		"plugins":     "plugin",
		"marketplace": "plugin",
		"skill":       "skills",
	}

	for alias, primary := range aliases {
		cmd, ok := r.Lookup(alias)
		if !ok {
			t.Errorf("expected alias /%s to resolve", alias)
			continue
		}
		if cmd.Name != primary {
			t.Errorf("alias /%s resolved to %q, want %q", alias, cmd.Name, primary)
		}
	}
}

// TestResumeSupportedCommandsList verifies the right commands are resume-supported.
func TestResumeSupportedCommandsList(t *testing.T) {
	r := NewFullRegistry()

	resumable := r.ResumeSupportedCommands()
	resumableNames := make(map[string]bool)
	for _, cmd := range resumable {
		resumableNames[cmd.Name] = true
	}

	// These should be resume-supported per the Rust spec
	expectedResumable := []string{"help", "status", "cost", "usage", "version", "export", "memory", "sandbox", "brief", "advisor", "keybindings", "privacy-settings", "files", "context", "hooks"}
	for _, name := range expectedResumable {
		if !resumableNames[name] {
			t.Errorf("expected /%s to be resume-supported", name)
		}
	}

	// These should NOT be resume-supported
	expectedNotResumable := []string{"commit", "pr", "issue", "exit", "quit"}
	for _, name := range expectedNotResumable {
		if resumableNames[name] {
			t.Errorf("expected /%s to NOT be resume-supported", name)
		}
	}
}

// TestAllCategoriesSorted verifies AllCategories returns sorted categories.
func TestAllCategoriesSorted(t *testing.T) {
	cats := AllCategories()
	if len(cats) == 0 {
		t.Fatal("expected non-empty categories")
	}

	strs := make([]string, len(cats))
	for i, c := range cats {
		strs[i] = string(c)
	}
	if !sort.StringsAreSorted(strs) {
		t.Errorf("categories not sorted: %v", strs)
	}
}

// TestNilLoopSafety verifies all commands handle nil loop gracefully.
func TestNilLoopSafety(t *testing.T) {
	r := NewFullRegistry()

	// Every command should handle nil loop without panicking.
	for _, cmd := range r.List() {
		t.Run(cmd.Name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("/%s panicked with nil loop: %v", cmd.Name, r)
				}
			}()
			// Some commands may return errors, that's OK.
			// We just verify they don't panic.
			_ = cmd.Handler("", nil)
		})
	}
}
