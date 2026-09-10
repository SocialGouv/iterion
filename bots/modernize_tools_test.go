package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireModernizeTools skips a modernize bot-level test when a tool it
// needs is missing on a developer's host — and FAILS instead under CI, where
// a skipped guard test is a green no-op: the exact class these tests exist
// to refuse (a verdict that passed because a check never ran).
func requireModernizeTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not on PATH under CI — the modernize guard tests would skip into a green no-op; install it in the workflow", tool)
			}
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// restrictedPATH builds a bin dir holding symlinks to the named tools only,
// so a script run with PATH=<dir> sees exactly those — the way to prove what
// a node does when `yq` is NOT on PATH without uninstalling anything.
func restrictedPATH(t *testing.T, tools ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not on PATH", tool)
		}
		if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
