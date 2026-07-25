package git

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiffLOC_ThreeDotStableWhenTargetAdvances: the run's LOC must be
// anchored at the merge-base, so upstream commits landing on the target
// AFTER the fork don't inflate the run's numbers (the two-dot pitfall).
func TestDiffLOC_ThreeDotStableWhenTargetAdvances(t *testing.T) {
	dir := gitRepo(t)

	// Fork a run branch and add 2 lines in a new file.
	mustRun(t, dir, "checkout", "-q", "-b", "iterion/run/test")
	if err := os.WriteFile(filepath.Join(dir, "run.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "run.txt")
	mustRun(t, dir, "commit", "-q", "-m", "run work")

	// Meanwhile main advances with unrelated churn.
	mustRun(t, dir, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "upstream.txt")
	mustRun(t, dir, "commit", "-q", "-m", "upstream churn")

	added, deleted, ok := DiffLOC(dir, "main", "iterion/run/test")
	if !ok {
		t.Fatal("DiffLOC not ok")
	}
	if added != 2 || deleted != 0 {
		t.Fatalf("LOC = +%d/−%d, want +2/−0 (upstream churn must not count)", added, deleted)
	}
}

func TestDiffLOC_EmptyDiffIsZeroNotMissing(t *testing.T) {
	dir := gitRepo(t)
	mustRun(t, dir, "checkout", "-q", "-b", "noop-branch")
	added, deleted, ok := DiffLOC(dir, "main", "noop-branch")
	if !ok || added != 0 || deleted != 0 {
		t.Fatalf("empty diff = (+%d/−%d, ok=%v), want zeros with ok=true", added, deleted, ok)
	}
}

func TestDiffLOC_UnresolvableRefIsNotOK(t *testing.T) {
	dir := gitRepo(t)
	if _, _, ok := DiffLOC(dir, "main", "deadbeef"); ok {
		t.Fatal("unresolvable final ref must report ok=false")
	}
	if _, _, ok := DiffLOC(dir, "gone-branch", "main"); ok {
		t.Fatal("unresolvable target ref must report ok=false")
	}
	if _, _, ok := DiffLOC(dir, "", "main"); ok {
		t.Fatal("empty target must report ok=false")
	}
}
