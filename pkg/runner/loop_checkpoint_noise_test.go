package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The checkpoint is pushed to the OPERATOR'S remote every ten minutes
// (workspace_checkpoint.go's incident): iterion's `.claude/` mirror and a
// drifted devbox.lock must never travel with it — the plan review's F2
// (#1464). The run's own files stage normally beside them.
func TestCheckpointScriptLeavesTheTreeNoiseOutOfThePreservedTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "lot@run")
	git("config", "user.name", "the lot")
	if err := os.WriteFile(filepath.Join(ws, "committed.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The lock is TRACKED — a drifted one is the #1459 shape.
	if err := os.WriteFile(filepath.Join(ws, "devbox.lock"), []byte("plugin_version: 0.0.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "baseline")

	// The noise: the mirror, untracked; the lock, drifted. Plus the run's
	// own work, which must survive.
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "skills", "mirrored.md"), []byte("iterion wrote this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "devbox.lock"), []byte("plugin_version: 0.0.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "uncommitted.txt"), []byte("the work that would be lost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sh := exec.Command("sh", "-c", checkpointScript)
	sh.Dir = ws
	sh.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := sh.Output()
	if err != nil {
		t.Fatalf("checkpoint script: %v (%s)", err, out)
	}
	_, sha := checkpointState(string(out))
	if sha == "" {
		t.Fatalf("a dirty tree must produce a checkpoint commit, got %q", string(out))
	}

	// The run's own work is in the preserved tree.
	if got := git("show", sha+":uncommitted.txt"); got != "the work that would be lost" {
		t.Fatalf("the checkpoint does not carry the run's work: %q", got)
	}
	// The mirror is not.
	if _, err := gittest.Try(ws, "show", sha+":.claude/skills/mirrored.md"); err == nil {
		t.Fatalf("the checkpoint carries iterion's .claude/ mirror — tree noise must never be pushed to the operator's remote")
	}
	// The drifted lock is not: the preserved tree holds the BASELINE lock,
	// because the drift was never staged into the temporary index.
	if got := strings.TrimSpace(git("show", sha+":devbox.lock")); got != "plugin_version: 0.0.4" {
		t.Fatalf("the checkpoint carries the drifted devbox.lock (%q) — the noise exclusion did not hold", got)
	}
}
