package detect

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The claude CLI detection runs never receives an empty CLAUDE_CONFIG_DIR —
// the CLI takes "" for a relative directory and writes its config backups
// into the working directory, a package source dir under go test — while a
// set value reaches it untouched. Exercised at the call site: a stub claude
// records the environment it is started with.
func TestClaudeAuthStatus_NeverHandsTheCLIAnEmptyConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub CLI is a POSIX shell script")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "env")
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nenv > \"$DETECT_TEST_ENV_RECORD\"\necho '{\"loggedIn\":false}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DETECT_TEST_ENV_RECORD", record)

	recorded := func() string {
		t.Helper()
		b, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the stub claude did not run: %v", err)
		}
		return string(b)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	claudeAuthStatusFn(context.Background(), stub)
	for _, line := range strings.Split(recorded(), "\n") {
		if strings.HasPrefix(line, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("an empty CLAUDE_CONFIG_DIR reached the claude CLI: %q", line)
		}
	}

	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude-config"))
	claudeAuthStatusFn(context.Background(), stub)
	if want := "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude-config"); !strings.Contains(recorded(), want) {
		t.Fatalf("a set CLAUDE_CONFIG_DIR did not reach the claude CLI (want %q)", want)
	}
}
