package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shape of a driver with NO host filesystem — the kubernetes one.
//
// It cannot bind-mount anything, so it copies the workspace to the same
// absolute path inside the pod and leaves WorkspaceFolder EMPTY. That is the
// rule containerWorkspaceFolder states for the docker driver too ("an empty
// one means the same absolute path as on the host"), and it is the only shape
// production runs ever take.
//
// Measured before this bench existed: every child-resource fixture set
// WorkspaceFolder to a real directory, so the empty case was never exercised —
// and both functions below were wrong for it, in opposite ways. One refused
// loudly, the other acted on a RELATIVE ".claude".
func k8sShapedEngine(t *testing.T, workspace string, run copySandboxRun) *Engine {
	t.Helper()
	return &Engine{
		workDir:       workspace,
		sharedSandbox: &SharedSandbox{Run: run, WorkspaceFolder: ""},
		resourceScope: &runResourceScope{path: workspace, borrowed: true, owned: map[string]string{}},
	}
}

// A snapshot on a driver without bind mounts must SUCCEED, and must round-trip
// the workspace's own resources.
//
// Before the fix this returned "child resources: shared sandbox has no
// workspace path", which failed the subbot node outright: measured in
// production, nine identical run failures in 22 minutes, every corpus
// extension blocked.
func TestSnapshotSucceedsWhenTheDriverHasNoBindMount(t *testing.T) {
	workspace := t.TempDir()
	writeClaudeFile(t, workspace, ".claude/settings.json", "parent")
	writeClaudeFile(t, workspace, ".claude/skills/kept/SKILL.md", "parent")

	e := k8sShapedEngine(t, workspace, copySandboxRun{root: workspace})
	restore, err := e.snapshotSharedChildResources(context.Background(), "backup-no-bind-mount")
	if err != nil {
		t.Fatalf("snapshot refused a workspace it can perfectly well reach: %v", err)
	}

	// A child replaces the parent's resources, then the restore puts them back.
	if err := os.RemoveAll(filepath.Join(workspace, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	writeClaudeFile(t, workspace, ".claude/settings.json", "child")
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workspace, ".claude", "settings.json"))
	if err != nil || string(body) != "parent" {
		t.Errorf("settings.json = %q (err %v), want the parent's bytes back", body, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".claude", "skills", "kept", "SKILL.md")); err != nil {
		t.Errorf("the parent's skill was not restored: %v", err)
	}
}

// The reset must act on the workspace's OWN `.claude`, never on a relative one.
//
// This is the half that failed in SILENCE: `path.Join("", ".claude")` is the
// relative ".claude", and the script then runs `rm -rf ".claude/skills"`
// against whatever cwd the exec lands in. A loud refusal is recoverable; a
// recursive delete in the wrong directory is not.
func TestResetActsOnTheWorkspaceNotOnTheExecCwd(t *testing.T) {
	workspace := t.TempDir()
	writeClaudeFile(t, workspace, ".claude/skills/borrowed/SKILL.md", "child")

	// The exec runs with the test process's cwd. Put a decoy `.claude` there:
	// a relative root would delete THIS one and leave the workspace untouched,
	// which is exactly the silent wrong target.
	decoy := t.TempDir()
	writeClaudeFile(t, decoy, ".claude/skills/decoy/SKILL.md", "innocent")
	t.Chdir(decoy)

	e := k8sShapedEngine(t, workspace, copySandboxRun{root: workspace})
	if err := e.clearBorrowedSandboxResources(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, ".claude", "skills")); !os.IsNotExist(err) {
		t.Errorf("the workspace's borrowed skills survived the reset (err %v) — "+
			"the reset acted somewhere else", err)
	}
	if _, err := os.Stat(filepath.Join(decoy, ".claude", "skills", "decoy", "SKILL.md")); err != nil {
		t.Errorf("the reset deleted a `.claude` in the exec's cwd, which it does not own: %v", err)
	}
}

// The refusal that must SURVIVE: no absolute path on either side.
//
// Removing the old error entirely would have turned a loud failure into the
// relative-root delete above. The guard moves to the state that really is
// broken, and it must still bite.
func TestNoAbsolutePathAnywhereIsStillRefused(t *testing.T) {
	e := &Engine{
		workDir:       "",
		sharedSandbox: &SharedSandbox{Run: copySandboxRun{root: t.TempDir()}, WorkspaceFolder: ""},
		resourceScope: &runResourceScope{path: "", borrowed: true, owned: map[string]string{}},
	}
	_, err := e.snapshotSharedChildResources(context.Background(), "backup-nowhere")
	if err == nil {
		t.Fatal("a shared sandbox with no absolute path anywhere was accepted — " +
			"the root would be the relative \".claude\"")
	}
	if !strings.Contains(err.Error(), "no absolute shared workspace path") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	if err := e.clearBorrowedSandboxResources(context.Background()); err == nil {
		t.Fatal("the reset accepted a relative root — this is the recursive delete")
	}
}

// A second copy-based driver that copies to a FIXED root — the shape
// RunInfo.WorkspacePath's own doc invites, "copies this into the sandbox at
// [Spec.WorkspaceFolder] (default `/workspace`)" — must be refused BY NAME,
// not silently reset somewhere else.
//
// The handle exposes no accessor for its root (the refresher addresses files
// relative to a root it never surfaces), so the derivation cannot ask. It
// asserts instead: a root the sandbox does not have is a named refusal.
//
// Without the assertion this test passes vacuously: `set -eu` lets both halves
// through, since `cp` of nothing and `rm -rf` of nothing each exit 0 — the
// wrong-root case reads as a clean no-op, which is the silent failure the
// review asked about.
func TestAWorkspaceRootTheSandboxDoesNotHaveIsRefusedByName(t *testing.T) {
	// The driver's copy lives here; the engine will derive the host path below,
	// which is exactly what a fixed-root driver would make absent in the pod.
	elsewhere := t.TempDir()
	writeClaudeFile(t, elsewhere, ".claude/skills/kept/SKILL.md", "in the pod")

	absent := filepath.Join(t.TempDir(), "copied-somewhere-else")
	e := k8sShapedEngine(t, absent, copySandboxRun{root: elsewhere})

	_, err := e.snapshotSharedChildResources(context.Background(), "backup-wrong-root")
	if err == nil {
		t.Fatal("the snapshot accepted a workspace root absent from the sandbox")
	}
	if !strings.Contains(err.Error(), "absent from the sandbox") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	if err := e.clearBorrowedSandboxResources(context.Background()); err == nil {
		t.Fatal("the reset accepted a workspace root absent from the sandbox — " +
			"this is the silent wrong-tree reset")
	}
	// And it touched nothing where the copy actually is.
	if _, err := os.Stat(filepath.Join(elsewhere, ".claude", "skills", "kept", "SKILL.md")); err != nil {
		t.Errorf("the refused reset still deleted from the real copy: %v", err)
	}
}
