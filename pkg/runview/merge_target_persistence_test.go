package runview

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The "current" vocabulary reads FOLDED everywhere merge_into is read —
// the compile's validation (pkg/dsl/ir/compile.go), the engine's own
// finalize (runtime resolveMergeTarget), the opt-out (store.MergeIntoOptOut).
// The persistence resolver is a reader of the same field: a merge_into
// spelled "Current"/"CURRENT" — accepted by the folded vocabulary — must
// resolve to the checked-out branch, never travel as a literal branch
// name (where a repo-targeted merge would clone --branch "Current" and
// fail audibly on a word the operator never meant as a branch).
func TestResolveMergeTargetForPersistenceReadsCurrentFolded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gittest.Run(t, repo, "init", "-q", "-b", "main")

	for _, spelling := range []string{"", "current", "Current", "CURRENT", " Current "} {
		if got := resolveMergeTargetForPersistence(spelling, repo); got != "main" {
			t.Fatalf("merge_into %q resolved to %q, want the checked-out branch main", spelling, got)
		}
	}
	// A branch name is kept as written — folding is the vocabulary's
	// reading, not a rewrite of the operator's target.
	if got := resolveMergeTargetForPersistence("feature/x", repo); got != "feature/x" {
		t.Fatalf("branch target rewritten to %q, want kept as written", got)
	}
	// A folded vocabulary word over no resolvable HEAD declines to ""
	// (the caller's fallback), never to the literal spelling.
	if got := resolveMergeTargetForPersistence("CURRENT", filepath.Join(repo, "nope")); got != "" {
		t.Fatalf("folded current over an unresolvable HEAD returned %q, want \"\"", got)
	}
}
