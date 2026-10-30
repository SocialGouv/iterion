package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// round8WitnessNames is one name of every family the H-10 round-8 review
// admitted to the execution class: the editor chain and pager git runs, the
// vendored AWS SDK's trust root, the python interpreter's module path, the
// opencode config home, the claude CLI's own socket. Through the real
// producer — the project `.env` the CLI auto-loads — each is refused by
// name, unset, and unmarked, where the defect planted every one of them
// into the process (the reviewer's pE.log measured plant-refused=false on
// all of them).
func TestAProjectDotenvLeavesNoRound8ExecutionNameInTheEnvironment(t *testing.T) {
	isolateRoutingEnv(t)

	names := []string{
		"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER",
		"AWS_CA_BUNDLE",
		"PYTHONPATH", "CLOUDSDK_PYTHON",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR",
		"OPENCODE_CONFIG_CONTENT", "OPENCODE_PERMISSION",
		"ANTHROPIC_UNIX_SOCKET",
	}
	for _, name := range names {
		clearEnv(t, name)
	}

	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, name+"=planted-by-the-repo-under-review")
	}
	repo := writeDotEnv(t, lines...)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	envPath := filepath.Join(repo, ".env")
	for _, name := range names {
		if got, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q: the repository under review chose what iterion's children execute, load or trust", name, got)
		}
		if envtrust.Planted(name) {
			t.Errorf("%s reads as planted: a refused name does not exist, it is not planted-but-distrusted", name)
		}
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the refusal does not name %s and the file — it said: %s", name, said)
		}
	}
	// The refusal reads the execution reason, not the routing one: the
	// class's answer is what a child runs or trusts.
	if !strings.Contains(said, "what iterion's children execute or trust") {
		t.Errorf("the round-8 names do not read as the execution class — it said: %s", said)
	}
}

// A value carrying a $-reference is planted LITERAL on this lane — the CLI
// plants no expansion, the studio lane expands the same file — so the
// divergence is warned about by name, and a single-quoted value plants the
// same bytes with no warning. On the defect, `GIT_EDITOR=$HOME/bin/gedit`
// planted the literal bytes and the child execed a path named `$HOME/...`
// with not a word on stderr (the reviewer's pC.log).
func TestAProjectDotenvWarnsWhenItPlantsAReferenceUnexpanded(t *testing.T) {
	isolateRoutingEnv(t)
	clearEnv(t, "MY_TOOL_EDITOR")
	clearEnv(t, "MY_TOOL_HOME")

	repo := writeDotEnv(t,
		"MY_TOOL_EDITOR=$HOME/bin/gedit",
		"MY_TOOL_HOME='$HOME/literal'",
	)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	// The unquoted reference plants its LITERAL bytes, and the warning says
	// so: the lane, the name, the file, the two ways out.
	if got := os.Getenv("MY_TOOL_EDITOR"); got != "$HOME/bin/gedit" {
		t.Errorf("MY_TOOL_EDITOR = %q: this lane plants no expansion, the bytes the file spelled", got)
	}
	for _, want := range []string{"MY_TOOL_EDITOR", filepath.Join(repo, ".env"), "plants no expansion", "single-quote"} {
		if !strings.Contains(said, want) {
			t.Errorf("the unexpanded-reference warning does not say %q — it said: %s", want, said)
		}
	}
	// The single-quoted value plants the same kind of bytes with no warning:
	// it is literal on every lane, there is no divergence to make loud.
	if got := os.Getenv("MY_TOOL_HOME"); got != "$HOME/literal" {
		t.Errorf("MY_TOOL_HOME = %q: the single-quoted bytes are what the file spelled", got)
	}
	if strings.Contains(said, "MY_TOOL_HOME comes from") {
		t.Errorf("the single-quoted value is warned about: it is literal on every lane — it said: %s", said)
	}
}
