package commands

import (
	"testing"
)

func TestRegistryListDeduplicates(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{
		Name:        "test",
		Aliases:     []string{"t", "tst"},
		Description: "test command",
		Handler:     func(args string, loop interface{}) error { return nil },
	})

	cmds := r.List()
	count := 0
	for _, c := range cmds {
		if c.Name == "test" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 'test' command in list, got %d (total commands: %d)", count, len(cmds))
	}
}

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{
		Name:        "test",
		Aliases:     []string{"t"},
		Description: "test command",
		Handler:     func(args string, loop interface{}) error { return nil },
	})

	// Lookup by primary name
	cmd, ok := r.Lookup("test")
	if !ok {
		t.Fatal("expected to find 'test'")
	}
	if cmd.Name != "test" {
		t.Errorf("expected Name='test', got %q", cmd.Name)
	}

	// Lookup by alias
	cmd, ok = r.Lookup("t")
	if !ok {
		t.Fatal("expected to find 't' alias")
	}
	if cmd.Name != "test" {
		t.Errorf("expected Name='test' from alias, got %q", cmd.Name)
	}

	// Lookup with / prefix
	cmd, ok = r.Lookup("/test")
	if !ok {
		t.Fatal("expected to find '/test'")
	}

	// Lookup missing
	_, ok = r.Lookup("nonexistent")
	if ok {
		t.Error("expected not to find 'nonexistent'")
	}
}

func TestRegistryCount(t *testing.T) {
	r := NewRegistry()
	// Builtins: help, exit, quit, clear, session-list = 5
	baseline := r.Count()
	r.Register(Command{
		Name:    "custom",
		Aliases: []string{"c"},
		Handler: func(args string, loop interface{}) error { return nil },
	})
	if got := r.Count(); got != baseline+1 {
		t.Errorf("expected %d commands after adding 1, got %d", baseline+1, got)
	}
}

func TestRegistrySuggestCommands(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{
		Name:    "session",
		Handler: func(args string, loop interface{}) error { return nil },
	})
	r.Register(Command{
		Name:    "status",
		Handler: func(args string, loop interface{}) error { return nil },
	})
	r.Register(Command{
		Name:    "compact",
		Handler: func(args string, loop interface{}) error { return nil },
	})

	suggestions := r.SuggestCommands("s", 10)
	if len(suggestions) < 2 {
		t.Errorf("expected at least 2 suggestions for 's', got %d", len(suggestions))
	}
	for _, s := range suggestions {
		if s[0] != '/' {
			t.Errorf("suggestion %q should start with '/'", s)
		}
	}

	// Limit
	limited := r.SuggestCommands("", 2)
	if len(limited) > 2 {
		t.Errorf("expected at most 2 suggestions, got %d", len(limited))
	}
}

func TestRegistryResumeSupportedCommands(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{
		Name:            "resumable",
		ResumeSupported: true,
		Handler:         func(args string, loop interface{}) error { return nil },
	})
	r.Register(Command{
		Name:    "not-resumable",
		Handler: func(args string, loop interface{}) error { return nil },
	})

	resumable := r.ResumeSupportedCommands()
	for _, cmd := range resumable {
		if !cmd.ResumeSupported {
			t.Errorf("command %q in resume list but ResumeSupported=false", cmd.Name)
		}
	}
	// The built-in /help is resume-supported
	found := false
	for _, cmd := range resumable {
		if cmd.Name == "help" || cmd.Name == "resumable" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one resume-supported command")
	}
}

func TestExecuteExit(t *testing.T) {
	r := NewRegistry()

	handled, err := r.Execute("/exit", nil)
	if !handled {
		t.Error("expected /exit to be handled")
	}
	if err != ErrExit {
		t.Errorf("expected ErrExit, got %v", err)
	}
}

func TestExecuteQuit(t *testing.T) {
	r := NewRegistry()

	handled, err := r.Execute("/quit", nil)
	if !handled {
		t.Error("expected /quit to be handled")
	}
	if err != ErrExit {
		t.Errorf("expected ErrExit, got %v", err)
	}
}

func TestExecuteUnknown(t *testing.T) {
	r := NewRegistry()

	handled, _ := r.Execute("/nonexistent", nil)
	if !handled {
		t.Error("expected unknown command to still be handled (with error message)")
	}
}

func TestExecuteNonCommand(t *testing.T) {
	r := NewRegistry()

	handled, _ := r.Execute("just a message", nil)
	if handled {
		t.Error("expected non-command input to return handled=false")
	}
}

func TestCommandsByCategory(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)
	RegisterConfigCommands(r)
	RegisterDiagnosticCommands(r)

	statusCmds := r.CommandsByCategory(CategoryStatus)
	if len(statusCmds) == 0 {
		t.Error("expected at least one status command")
	}
	for _, cmd := range statusCmds {
		if cmd.Category != CategoryStatus {
			t.Errorf("command %q has category %q, want %q", cmd.Name, cmd.Category, CategoryStatus)
		}
	}

	configCmds := r.CommandsByCategory(CategoryConfig)
	if len(configCmds) == 0 {
		t.Error("expected at least one config command")
	}

	diagCmds := r.CommandsByCategory(CategoryDiagnostics)
	if len(diagCmds) == 0 {
		t.Error("expected at least one diagnostic command")
	}
}

func TestCategories(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)
	RegisterConfigCommands(r)

	cats := r.Categories()
	if len(cats) < 2 {
		t.Errorf("expected at least 2 categories, got %d", len(cats))
	}
	foundStatus := false
	foundConfig := false
	for _, c := range cats {
		if c == CategoryStatus {
			foundStatus = true
		}
		if c == CategoryConfig {
			foundConfig = true
		}
	}
	if !foundStatus {
		t.Error("expected CategoryStatus in categories")
	}
	if !foundConfig {
		t.Error("expected CategoryConfig in categories")
	}
}

func TestFullRegistryRustParityCommands(t *testing.T) {
	r := NewFullRegistry()

	// Every Rust command name from SLASH_COMMAND_SPECS should exist in Go.
	rustCommands := []string{
		"add-dir", "advisor", "agent", "agents", "alias", "allowed-tools", "api-key",
		"approve", "autofix", "benchmark", "blame", "bookmarks", "branch", "brief",
		"budget", "bughunter", "build", "cache", "changelog", "chat", "clear", "color",
		"commit", "compact", "config", "context", "copy", "cost", "cron",
		"debug-tool-call", "definition", "deny", "desktop", "diagnostics", "diff",
		"docs", "doctor", "effort", "env", "exit", "explain", "export", "fast",
		"feedback", "files", "fix", "focus", "format", "git", "help", "history",
		"hooks", "hover", "ide", "image", "init", "insights", "issue", "keybindings",
		"language", "lint", "listen", "log", "login", "logout", "macro", "map",
		"max-tokens", "mcp", "memory", "metrics", "migrate", "model", "multi",
		"notifications", "output-style", "parallel", "paste", "perf", "permissions",
		"pin", "plan", "plugin", "pr", "privacy-settings", "profile", "project",
		"providers", "rate-limit", "reasoning", "refactor", "references",
		"release-notes", "rename", "reset", "resume", "retry", "review", "rewind",
		"run", "sandbox", "screenshot", "search", "security-review", "session",
		"share", "skills", "speak", "stash", "stats", "status", "stickers", "stop",
		"subagent", "summary", "symbols", "system-prompt", "tag", "tasks", "team",
		"telemetry", "teleport", "temperature", "templates", "terminal-setup", "test",
		"theme", "thinkback", "tokens", "tool-details", "ultraplan", "undo", "unfocus",
		"unpin", "upgrade", "usage", "version", "vim", "voice", "web", "workspace",
	}

	var missing []string
	for _, name := range rustCommands {
		if _, ok := r.Lookup(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing %d Rust-parity commands: %v", len(missing), missing)
	}
}

func TestFullRegistryNoDuplicatePrimaryNames(t *testing.T) {
	r := NewFullRegistry()
	cmds := r.List()
	seen := make(map[string]int)
	for _, cmd := range cmds {
		name := cmd.Name
		seen[name]++
		if seen[name] > 1 {
			t.Errorf("duplicate primary command name %q (seen %d times)", name, seen[name])
		}
	}
}

func TestFullRegistryAliasResolution(t *testing.T) {
	r := NewFullRegistry()

	// Test known aliases resolve to their primary command.
	aliases := map[string]string{
		"plugins":     "plugin",
		"marketplace": "plugin",
		"skill":       "skills",
		"ws":          "workspace",
		"temp":        "temperature",
		"sysprompt":   "system-prompt",
		"lang":        "language",
		"term-setup":  "terminal-setup",
		"yes":         "approve",
		"y":           "approve",
		"no":          "deny",
		"n":           "deny",
	}

	for alias, primaryName := range aliases {
		cmd, ok := r.Lookup(alias)
		if !ok {
			t.Errorf("alias %q not found in registry", alias)
			continue
		}
		if cmd.Name != primaryName {
			t.Errorf("alias %q resolved to %q, want %q", alias, cmd.Name, primaryName)
		}
	}
}

func TestResumeSupportedCommandsContainsExpected(t *testing.T) {
	r := NewFullRegistry()
	resumable := r.ResumeSupportedCommands()

	expectedResumable := []string{"help", "history", "workspace", "status", "cost", "usage", "version", "stats", "tokens", "cache"}
	resumeMap := make(map[string]bool)
	for _, cmd := range resumable {
		name := cmd.Name
		if name[0] == '/' {
			name = name[1:]
		}
		resumeMap[name] = true
	}

	for _, name := range expectedResumable {
		if !resumeMap[name] {
			t.Errorf("expected %q to be resume-supported, but it's not", name)
		}
	}
}

func TestHelpWithArgShowsDetail(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{
		Name:         "test-cmd",
		Aliases:      []string{"tc"},
		Description:  "A test command",
		ArgumentHint: "<arg>",
		Handler:      func(args string, loop interface{}) error { return nil },
	})

	// Should not error
	handled, err := r.Execute("/help test-cmd", nil)
	if !handled || err != nil {
		t.Errorf("expected help detail: handled=%v, err=%v", handled, err)
	}

	// Unknown command help
	handled, err = r.Execute("/help nonexistent", nil)
	if !handled || err != nil {
		t.Errorf("expected help for unknown: handled=%v, err=%v", handled, err)
	}
}
