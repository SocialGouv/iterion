package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The desktop loader asks the same round-8 questions the CLI and studio
// lanes ask: the execution admissions (git's editor chain and pager, the
// vendored AWS SDK's trust root, the python module path, the opencode
// config home, the claude CLI's socket) are refused by name, and a
// $-reference this lane plants literal is warned about — this lane plants
// no expansion, the studio lane expands the same file.
func TestTheDesktopEnvFileLeavesNoRound8ExecutionNameInTheEnvironment(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(envtrust.DotenvEndpointsOptIn, "")

	names := []string{
		"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER",
		"AWS_CA_BUNDLE",
		"PYTHONPATH", "CLOUDSDK_PYTHON",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR",
		"OPENCODE_CONFIG_CONTENT", "OPENCODE_PERMISSION",
		"ANTHROPIC_UNIX_SOCKET",
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	lines := []string{"MY_TOOL_EDITOR=$HOME/bin/gedit", "MY_TOOL_HOME='$HOME/literal'"}
	for _, name := range names {
		os.Unsetenv(name)
		t.Cleanup(func() { os.Unsetenv(name) })
		lines = append(lines, name+"=planted-by-the-file")
	}
	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("ITERION_ENV_FILE", envPath)
	dotenvMu.Lock()
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()

	said := captureStderr(t, loadIterionEnvFile)

	for _, name := range names {
		if got, present := os.LookupEnv(name); present {
			t.Errorf("%s = %q: the file chose what iterion's children execute, load or trust", name, got)
		}
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the refusal does not name %s and the file — it said: %s", name, said)
		}
	}
	// The unexpanded reference on this lane warns and plants literal; the
	// single-quoted value plants the same kind of bytes with no warning.
	if got := os.Getenv("MY_TOOL_EDITOR"); got != "$HOME/bin/gedit" {
		t.Errorf("MY_TOOL_EDITOR = %q: this lane plants no expansion, the bytes the file spelled", got)
	}
	for _, want := range []string{"MY_TOOL_EDITOR", envPath, "plants no expansion"} {
		if !strings.Contains(said, want) {
			t.Errorf("the unexpanded-reference warning does not say %q — it said: %s", want, said)
		}
	}
	if got := os.Getenv("MY_TOOL_HOME"); got != "$HOME/literal" {
		t.Errorf("MY_TOOL_HOME = %q: the single-quoted bytes are what the file spelled", got)
	}
	if strings.Contains(said, "MY_TOOL_HOME comes from") {
		t.Errorf("the single-quoted value is warned about: it is literal on every lane — it said: %s", said)
	}
}
