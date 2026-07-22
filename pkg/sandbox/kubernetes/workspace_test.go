package kubernetes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCloneRoot verifies the worktree→clone-root resolution that
// populateWorkspace relies on: a git worktree's `.git` is a pointer file,
// so the k8s driver must copy the real clone (with objects + origin), not
// the worktree, into the sandbox.
func TestResolveCloneRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "clone")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", repo)
	git("-C", repo, "commit", "-q", "--allow-empty", "-m", "init")

	// plain clone → itself
	if got := resolveCloneRoot(ctx, repo); !sameDir(got, repo) {
		t.Errorf("resolveCloneRoot(clone) = %q, want %q", got, repo)
	}

	// worktree (its .git is a pointer file) → the clone root
	wt := filepath.Join(dir, "wt")
	git("-C", repo, "worktree", "add", "-q", "--detach", wt)
	if got := resolveCloneRoot(ctx, wt); !sameDir(got, repo) {
		t.Errorf("resolveCloneRoot(worktree) = %q, want clone root %q", got, repo)
	}

	// non-git dir → itself (best-effort fallback)
	plain := t.TempDir()
	if got := resolveCloneRoot(ctx, plain); got != plain {
		t.Errorf("resolveCloneRoot(non-git) = %q, want %q", got, plain)
	}
}

func sameDir(a, b string) bool {
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	return erra == nil && errb == nil && ra == rb
}

// TestFixupWorkspaceGitScript runs the post-populate fixup script (the
// exact bytes the driver ships to the pod) against a real clone shaped
// like the runner's: a credential store whose helper records the HOST
// absolute path, plus a stale .git/worktrees registration. After the
// script, the helper must point at the workspace-local credential file
// and the stale registration must be gone — the two host-path leftovers
// that break in-pod `git push` on the tar-copied workspace.
func TestFixupWorkspaceGitScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", dir)
	// The runner's credential store, recorded with a HOST path that does
	// not exist in the pod.
	credPath := filepath.Join(dir, ".git", "iterion-credentials")
	if err := os.WriteFile(credPath, []byte("https://oauth2:tok@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("-C", dir, "config", "credential.helper", "store --file=/host/clone/.git/iterion-credentials")
	// A stale worktree registration from the engine's host worktree.
	if err := os.MkdirAll(filepath.Join(dir, ".git", "worktrees", "run-x"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", fixupWorkspaceGitScript, "sh", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixup script: %v\n%s", err, out)
	}

	out, err := exec.Command("git", "-C", dir, "config", "credential.helper").Output()
	if err != nil {
		t.Fatalf("read credential.helper: %v", err)
	}
	want := "store --file=" + credPath
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("credential.helper = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "worktrees")); !os.IsNotExist(err) {
		t.Errorf("stale .git/worktrees registration survived: %v", err)
	}

	// Non-git workspace: the script is a clean no-op.
	plain := t.TempDir()
	cmd = exec.Command("sh", "-c", fixupWorkspaceGitScript, "sh", plain)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixup on non-git dir: %v\n%s", err, out)
	}
}

// TestWorkspaceFileTarget pins the containment rules for the
// WorkspaceFileRefresher path join.
func TestWorkspaceFileTarget(t *testing.T) {
	got, err := workspaceFileTarget("/ws", ".git/iterion-credentials")
	if err != nil || got != "/ws/.git/iterion-credentials" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, rel := range []string{"", "/abs", "../up", "a/../b", ".", "..", "a\nb"} {
		if _, err := workspaceFileTarget("/ws", rel); err == nil {
			t.Errorf("relPath %q: expected an error", rel)
		}
	}
	if _, err := workspaceFileTarget("", "x"); err == nil {
		t.Error("empty workspace: expected an error")
	}
}
