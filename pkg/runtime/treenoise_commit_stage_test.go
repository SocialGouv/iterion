package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The behavioral witness plan review F2 asked for (verdict-3 fix): the
// commit-and-finalize argv, executed against a real repository whose dirt
// is a dependency bot's deliverable (devbox.json AND devbox.lock modified,
// the mirror untracked), stages BOTH halves of the deliverable and none of
// the mirror. The call site (CommitUncommittedAndFinalize) is merge-
// destined: dropping the lock here would merge devbox.json without its
// resolution and destroy the bump with the worktree.
func TestCommitStageArgsStageTheDependencyWork(t *testing.T) {
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "lot@run")
	git("config", "user.name", "the lot")
	if err := os.WriteFile(filepath.Join(ws, "devbox.json"), []byte("{\"packages\": []}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "devbox.lock"), []byte("plugin_version: 0.0.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "baseline with a tracked lock")

	// The dependency bot's deliverable, dirty: both halves.
	if err := os.WriteFile(filepath.Join(ws, "devbox.json"), []byte("{\"packages\": [\"go@1.26\"]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "devbox.lock"), []byte("plugin_version: 0.0.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And the engine's mirror, untracked beside it.
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "skills", "mirrored.md"), []byte("iterion wrote this\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	git(commitStageArgs()...)

	staged := strings.Split(strings.TrimSpace(git("diff-index", "--name-only", "HEAD")), "\n")
	if !slices.Contains(staged, "devbox.json") || !slices.Contains(staged, "devbox.lock") {
		t.Fatalf("the dependency work is not fully staged: %q", staged)
	}
	for _, p := range staged {
		if strings.HasPrefix(p, ".claude") {
			t.Fatalf("the mirror is staged: %q", staged)
		}
	}
}

// R138690 (verdict 5): the probe must agree with THIS gesture — a
// lock-only bump is merge-destined work here, never "tree noise only",
// and the mirror beside it is still set aside.
func TestCommitWorkPathsTreatALockOnlyBumpAsWork(t *testing.T) {
	porcelain := strings.Join([]string{" M devbox.lock"}, "\n")
	got := commitWorkPaths(porcelain)
	if len(got) != 1 || got[0] != "devbox.lock" {
		t.Fatalf("commitWorkPaths = %q, want [devbox.lock] — a lock-only bump is merge-destined work on this path", got)
	}
	porcelain = strings.Join([]string{"?? .claude/settings.json", " M devbox.lock"}, "\n")
	got = commitWorkPaths(porcelain)
	if len(got) != 1 || got[0] != "devbox.lock" {
		t.Fatalf("mirror beside the lock = %q, want only the lock", got)
	}
}
