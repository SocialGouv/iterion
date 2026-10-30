package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The check is a predicate, so its two halves are exercised directly: a real
// path under the root passes, every shape that makes the source the sandbox's
// choice fails — including the one that does not exist yet, which must pass
// before creation and fail after a link appears.
func TestRealBindSourceRefusesWhatTheSandboxCouldChoose(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	real := filepath.Join(root, "store", "cache")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RealBindSource(root, real); err != nil {
		t.Errorf("a real directory under the root must pass: %v", err)
	}
	if err := RealBindSource(root, filepath.Join(root, "store", "absent", "deeper")); err != nil {
		t.Errorf("a path that does not exist yet cannot be a symlink, so it must pass: %v", err)
	}
	if err := RealBindSource(root, filepath.Join(outside, "x")); err == nil {
		t.Errorf("a path outside the root must be refused")
	}

	linked := filepath.Join(root, "store", "linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if err := RealBindSource(root, linked); err == nil {
		t.Errorf("a symlink whose target leaves the root must be refused")
	}
	if err := RealBindSource(root, filepath.Join(linked, "deeper")); err == nil {
		t.Errorf("a path THROUGH a symlink that leaves the root must be refused — the kernel follows it at mount time")
	}

	// EnsureRealBindSourceDir asks again after creating, so a link that
	// appears between the check and the MkdirAll cannot survive into the plan.
	ensured := filepath.Join(root, "store", "ensured")
	if err := EnsureRealBindSourceDir(root, ensured, 0o700); err != nil {
		t.Errorf("creating a real directory under the root must pass: %v", err)
	}
	if info, err := os.Lstat(ensured); err != nil || !info.IsDir() {
		t.Errorf("the source was not created: %v", err)
	}
	if err := EnsureRealBindSourceDir(root, filepath.Join(linked, "x"), 0o700); err == nil {
		t.Errorf("creating through an escaping symlink must be refused, not followed")
	}
}

// The question the driver re-asks at creation time is the SAME predicate: a
// leaf that was real when the plan was built and is a link by the time the
// container is created must fail the second ask, with both sides of the
// question live — the real leaf passes, the swapped one refuses.
func TestRealBindSourceAnswersTheSwapInsideThePlanToCreateWindow(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	leaf := filepath.Join(root, "store", "scratch")
	if err := EnsureRealBindSourceDir(root, leaf, 0o700); err != nil {
		t.Fatalf("the plan-time ask must pass on the real leaf: %v", err)
	}
	// The concurrent run swaps the leaf: the directory becomes a symlink to a
	// host path the sandbox chose.
	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, leaf); err != nil {
		t.Fatal(err)
	}
	err := RealBindSource(root, leaf)
	if err == nil {
		t.Fatal("the swapped leaf passed the creation-time ask — the window is open")
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("the refusal must name the swap it caught: %v", err)
	}
}
