package commands

import (
	"os"
	"os/exec"
	"testing"
)

func TestStashCommandNoGit(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("stash")
	if !ok {
		t.Fatal("stash command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error when git unavailable, got: %v", err)
	}
}

func TestBlameCommandNoFile(t *testing.T) {
	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("blame")
	if !ok {
		t.Fatal("blame command not registered")
	}

	// No args => usage message, no error
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error for empty args, got: %v", err)
	}
}

func TestLogCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(dir)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	os.WriteFile(dir+"/a.txt", []byte("a"), 0644)
	run("add", "a.txt")
	run("commit", "-m", "initial commit")

	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("log")
	if !ok {
		t.Fatal("log command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// With explicit count
	err = cmd.Handler("5", nil)
	if err != nil {
		t.Fatalf("expected no error with count, got: %v", err)
	}
}

func TestGitCommandNoArgs(t *testing.T) {
	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("git")
	if !ok {
		t.Fatal("git command not registered")
	}

	// No args => usage message
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error for no args, got: %v", err)
	}
}

func TestGitCommandNoGit(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("git")
	if !ok {
		t.Fatal("git command not registered")
	}

	err := cmd.Handler("status", nil)
	if err != nil {
		t.Fatalf("expected no error when git unavailable, got: %v", err)
	}
}

func TestProjectCommand(t *testing.T) {
	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("project")
	if !ok {
		t.Fatal("project command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestEnvCommand(t *testing.T) {
	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("env")
	if !ok {
		t.Fatal("env command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestSandboxCommandGit(t *testing.T) {
	// Sandbox command is registered by RegisterPluginCommands (not RegisterGitCommands).
	r := NewRegistry()
	RegisterPluginCommands(r)

	cmd, ok := r.Lookup("sandbox")
	if !ok {
		t.Fatal("sandbox command not registered")
	}

	// With nil loop, should fall through to environment detection
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestResetCommandNoLoop(t *testing.T) {
	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("reset")
	if !ok {
		t.Fatal("reset command not registered")
	}

	// With nil loop, should print "not available"
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestTerminalSetupCommand(t *testing.T) {
	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("terminal-setup")
	if !ok {
		t.Fatal("terminal-setup command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Test alias
	cmd2, ok := r.Lookup("term-setup")
	if !ok {
		t.Fatal("term-setup alias not registered")
	}
	if cmd2.Name != "terminal-setup" {
		t.Fatalf("expected name terminal-setup, got %s", cmd2.Name)
	}
}

func TestStashCommandInRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(dir)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	os.WriteFile(dir+"/a.txt", []byte("a"), 0644)
	run("add", "a.txt")
	run("commit", "-m", "initial")

	r := NewRegistry()
	RegisterGitCommands(r)

	cmd, ok := r.Lookup("stash")
	if !ok {
		t.Fatal("stash command not registered")
	}

	// List stash on clean repo
	err := cmd.Handler("list", nil)
	if err != nil {
		t.Fatalf("expected no error for stash list, got: %v", err)
	}

	// Unknown subcommand
	err = cmd.Handler("unknown", nil)
	if err != nil {
		t.Fatalf("expected no error for unknown subcommand, got: %v", err)
	}
}
