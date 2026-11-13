package commands

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCommitCommandNoGit(t *testing.T) {
	// Save and restore PATH so we can simulate git being absent.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	if gitAvailable() {
		t.Fatal("expected gitAvailable() to return false with empty PATH")
	}

	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("commit")
	if !ok {
		t.Fatal("commit command not registered")
	}

	// Handler should not error — just print a message.
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error when git unavailable, got: %v", err)
	}
}

func TestCommitCommandNoStagedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create a temporary git repo with nothing staged.
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

	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("commit")
	if !ok {
		t.Fatal("commit command not registered")
	}

	// No staged changes — handler should succeed without error.
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected nil error for no staged changes, got: %v", err)
	}
}

func TestReviewCommandOutput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create a repo with uncommitted changes so diff is non-empty.
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
	os.WriteFile(dir+"/hello.txt", []byte("hello"), 0644)
	run("add", "hello.txt")
	run("commit", "-m", "init")
	os.WriteFile(dir+"/hello.txt", []byte("hello world"), 0644)

	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("review")
	if !ok {
		t.Fatal("review command not registered")
	}

	// Should not error — the diff is non-empty.
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestTestCommandProjectDetection(t *testing.T) {
	tests := []struct {
		file    string
		wantCmd string
		wantArg string // checked as first arg
	}{
		{"go.mod", "go", "test"},
		{"package.json", "npm", "test"},
		{"Cargo.toml", "cargo", "test"},
		{"Makefile", "make", "test"},
	}

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			dir := t.TempDir()
			os.Chdir(dir)
			os.WriteFile(dir+"/"+tc.file, []byte(""), 0644)

			cmd, args := detectTestCommand()
			if cmd != tc.wantCmd {
				t.Errorf("expected command %q, got %q", tc.wantCmd, cmd)
			}
			if len(args) == 0 || args[0] != tc.wantArg {
				t.Errorf("expected first arg %q, got %v", tc.wantArg, args)
			}
		})
	}
}

func TestReleaseNotesCommand(t *testing.T) {
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
	run("commit", "-m", "feat: initial")

	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("release-notes")
	if !ok {
		t.Fatal("release-notes command not registered")
	}

	// Should run without error (no tags, falls back to recent log).
	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

// mockPromptInjector implements the promptInjector interface for testing.
type mockPromptInjector struct {
	injected string
}

func (m *mockPromptInjector) InjectPrompt(prompt string) error {
	m.injected = prompt
	return nil
}

func TestExplainCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("explain")
	if !ok {
		t.Fatal("explain command not registered")
	}

	t.Run("no args no loop", func(t *testing.T) {
		err := cmd.Handler("", nil)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("with target", func(t *testing.T) {
		pi := &mockPromptInjector{}
		err := cmd.Handler("main.go", pi)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if !strings.Contains(pi.injected, "main.go") {
			t.Fatalf("expected prompt to contain target, got: %s", pi.injected)
		}
	})
}

func TestRefactorCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("refactor")
	if !ok {
		t.Fatal("refactor command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	pi := &mockPromptInjector{}
	err = cmd.Handler("utils.go", pi)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !strings.Contains(pi.injected, "utils.go") {
		t.Fatalf("expected prompt to contain target, got: %s", pi.injected)
	}
}

func TestDocsCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("docs")
	if !ok {
		t.Fatal("docs command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestFixCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("fix")
	if !ok {
		t.Fatal("fix command not registered")
	}

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestPerfCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("perf")
	if !ok {
		t.Fatal("perf command not registered")
	}

	pi := &mockPromptInjector{}
	err := cmd.Handler("server.go", pi)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !strings.Contains(pi.injected, "server.go") {
		t.Fatalf("expected prompt to contain target, got: %s", pi.injected)
	}
}

func TestLintCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("lint")
	if !ok {
		t.Fatal("lint command not registered")
	}

	// In a temp dir with no project markers, should print "no linter found"
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(dir)

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestBuildCommand(t *testing.T) {
	r := NewRegistry()
	RegisterCodeCommands(r)

	cmd, ok := r.Lookup("build")
	if !ok {
		t.Fatal("build command not registered")
	}

	// In a temp dir with no project markers, should print "no build system found"
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(dir)

	err := cmd.Handler("", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestRunGitSafeArgs(t *testing.T) {
	// Verify that runGit uses exec.Command with separate args (no shell).
	// We test by passing a string that would be dangerous in a shell context
	// but is safe as a literal git argument.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// This would cause issues with shell interpolation: `; rm -rf /`
	// With exec.Command it is passed as a literal argument to git.
	_, err := runGit("log", "--oneline", "-1", "--grep=; rm -rf /")
	// It should not error catastrophically — git just finds no matches.
	// The key assertion is that no shell injection occurred.
	if err != nil {
		// Accept errors from git itself (e.g., not in a repo), but make
		// sure it is a git error, not a shell execution error.
		if !strings.Contains(err.Error(), "git log") {
			t.Fatalf("unexpected error type: %v", err)
		}
	}
}
