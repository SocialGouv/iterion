package kubernetes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateExecEnvironment gives real commands and fake kubectl scripts the same
// test-owned home and Git configuration. Call it before spawning a process,
// including through a production helper; these tests cannot run in parallel.
// Git fixture commands still use internal/gittest to disable auto-maintenance.
func isolateExecEnvironment(t *testing.T) {
	t.Helper()
	// Remove inherited command-scope config and repository redirections as
	// well as file config: either can redirect a shell's nested git command.
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, value)
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	home := t.TempDir()
	config := filepath.Join(home, "empty.gitconfig")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
}
