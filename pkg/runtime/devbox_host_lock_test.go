package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// stubHostDevboxInstall replaces the host-devbox seams with an install that
// runs `mutate` in the project dir — the way a real `devbox install` rewrites
// or creates the repository's devbox.lock on a host whose plugin registry is
// newer than the pin.
func stubHostDevboxInstall(t *testing.T, mutate func(projectDir string)) *[]string {
	t.Helper()
	installs := &[]string{}
	origLook, origRun := hostDevboxLookPath, runHostDevboxInstall
	hostDevboxLookPath = func() (string, error) { return "/fake/devbox", nil }
	runHostDevboxInstall = func(_ context.Context, _, projectDir string, _ *iterlog.Logger) error {
		*installs = append(*installs, projectDir)
		if mutate != nil {
			mutate(projectDir)
		}
		return nil
	}
	t.Cleanup(func() { hostDevboxLookPath, runHostDevboxInstall = origLook, origRun })
	return installs
}

func runHostDevboxEngine(t *testing.T, workDir, runID string) map[string]any {
	t.Helper()
	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(workDir),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted")
	}
	return data
}

// TestEngineRun_HostDevbox_RepoLockRewrittenByInstallIsRestored: the two
// wave-6 dogfoods (#1459) — `devbox install` in the run's worktree bumped
// the lock's plugin_version before the first node ran, and both bots' scope
// gates refused the run on a file they never touched. The pinned content
// comes back, and the event says so.
func TestEngineRun_HostDevbox_RepoLockRewrittenByInstallIsRestored(t *testing.T) {
	const pinned = `{"lockfile_version":"1","packages":{"nodejs_24@latest":{"plugin_version":"0.0.4"}}}`
	const drifted = `{"lockfile_version":"1","packages":{"nodejs_24@latest":{"plugin_version":"0.0.5"}}}`
	installs := stubHostDevboxInstall(t, func(projectDir string) {
		if err := os.WriteFile(filepath.Join(projectDir, devboxLockName), []byte(drifted), 0o644); err != nil {
			t.Fatalf("mutate lock: %v", err)
		}
	})
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")
	lock := filepath.Join(workDir, devboxLockName)
	if err := os.WriteFile(lock, []byte(pinned), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	data := runHostDevboxEngine(t, workDir, "run-devbox-lock-restored")

	if len(*installs) != 1 || (*installs)[0] != workDir {
		t.Fatalf("installs = %v, want [%s]", *installs, workDir)
	}
	got, err := os.ReadFile(lock)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if string(got) != pinned {
		t.Fatalf("devbox.lock after the run = %s, want the repository's pinned content", got)
	}
	kept := stringsFromAny(data["lock_kept"])
	if len(kept) != 1 || !strings.Contains(kept[0], "rewrote "+lock) || !strings.Contains(kept[0], "restored") {
		t.Fatalf("event lock_kept = %v, want one note naming the rewritten lock and its restore", kept)
	}
	if errs := stringsFromAny(data["errors"]); len(errs) != 0 {
		t.Fatalf("event errors = %v, want none: a restored lock is not an install failure", errs)
	}
}

// TestEngineRun_HostDevbox_RepoLockCreatedByInstallIsRemoved: a repository
// that tracks no lock must not gain an untracked one from the run's setup.
func TestEngineRun_HostDevbox_RepoLockCreatedByInstallIsRemoved(t *testing.T) {
	stubHostDevboxInstall(t, func(projectDir string) {
		if err := os.WriteFile(filepath.Join(projectDir, devboxLockName), []byte(`{"lockfile_version":"1"}`), 0o644); err != nil {
			t.Fatalf("create lock: %v", err)
		}
	})
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")
	lock := filepath.Join(workDir, devboxLockName)

	data := runHostDevboxEngine(t, workDir, "run-devbox-lock-created")

	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("devbox.lock exists after the run (stat err=%v), want it removed: the repository tracks none", err)
	}
	kept := stringsFromAny(data["lock_kept"])
	if len(kept) != 1 || !strings.Contains(kept[0], "created "+lock) || !strings.Contains(kept[0], "removed") {
		t.Fatalf("event lock_kept = %v, want one note naming the created lock and its removal", kept)
	}
}

// TestEngineRun_HostDevbox_RepoLockUntouchedIsLeftAlone: an install that
// leaves the lock as it found it yields no write and no note — the common
// case must stay silent.
func TestEngineRun_HostDevbox_RepoLockUntouchedIsLeftAlone(t *testing.T) {
	const pinned = `{"lockfile_version":"1","packages":{"go@1.26":{}}}`
	stubHostDevboxInstall(t, nil)
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")
	lock := filepath.Join(workDir, devboxLockName)
	if err := os.WriteFile(lock, []byte(pinned), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}

	data := runHostDevboxEngine(t, workDir, "run-devbox-lock-untouched")

	after, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("devbox.lock was rewritten (mtime %v → %v) although the install left it alone", before.ModTime(), after.ModTime())
	}
	if _, present := data["lock_kept"]; present {
		t.Fatalf("event carries lock_kept = %v for an untouched lock, want the field absent", data["lock_kept"])
	}
}

// TestEngineRun_HostDevbox_RepoLockRemovedByInstallIsRestored: a lock the
// install deleted comes back too, and the note says removed, not rewritten.
func TestEngineRun_HostDevbox_RepoLockRemovedByInstallIsRestored(t *testing.T) {
	const pinned = `{"lockfile_version":"1","packages":{"go@1.26":{}}}`
	stubHostDevboxInstall(t, func(projectDir string) {
		if err := os.Remove(filepath.Join(projectDir, devboxLockName)); err != nil {
			t.Fatalf("remove lock: %v", err)
		}
	})
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")
	lock := filepath.Join(workDir, devboxLockName)
	if err := os.WriteFile(lock, []byte(pinned), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	data := runHostDevboxEngine(t, workDir, "run-devbox-lock-removed")

	got, err := os.ReadFile(lock)
	if err != nil || string(got) != pinned {
		t.Fatalf("devbox.lock after the run = %q (err=%v), want the pinned content restored", got, err)
	}
	kept := stringsFromAny(data["lock_kept"])
	if len(kept) != 1 || !strings.Contains(kept[0], "removed "+lock) || strings.Contains(kept[0], "rewrote") {
		t.Fatalf("event lock_kept = %v, want one note saying the lock was removed and restored", kept)
	}
}

// TestEngineRun_HostDevbox_RepoLockUnreadableIsNotTakenForAbsent: a lock the
// engine cannot read before the install is reported and left alone — never
// read as "the repository tracks none" and removed after the install, even
// when the install leaves it readable (the shape that turns a read error
// into "created by devbox").
func TestEngineRun_HostDevbox_RepoLockUnreadableIsNotTakenForAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 000 file")
	}
	const pinned = `{"lockfile_version":"1","packages":{"go@1.26":{}}}`
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")
	lock := filepath.Join(workDir, devboxLockName)
	if err := os.WriteFile(lock, []byte(pinned), 0o000); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lock, 0o644) })
	installs := stubHostDevboxInstall(t, func(projectDir string) {
		// devbox leaves the file readable: the post-install read succeeds.
		if err := os.Chmod(filepath.Join(projectDir, devboxLockName), 0o644); err != nil {
			t.Fatalf("chmod lock: %v", err)
		}
	})

	data := runHostDevboxEngine(t, workDir, "run-devbox-lock-unreadable")

	if len(*installs) != 1 {
		t.Fatalf("installs = %v, want the install to run regardless", *installs)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("devbox.lock was removed after the run (%v) — an unreadable lock was taken for absent", err)
	}
	if _, present := data["lock_kept"]; present {
		t.Fatalf("event carries lock_kept = %v for a lock the engine could not read, want none", data["lock_kept"])
	}
	errs := stringsFromAny(data["errors"])
	if len(errs) != 1 || !strings.Contains(errs[0], "read the repo devbox.lock before install") {
		t.Fatalf("event errors = %v, want one entry saying the lock could not be read before the install", errs)
	}
}

// TestDevboxInstallSnippet_RepoLockIsKept: the sandbox twin of the host
// path. The in-place workspace install snapshots the lock under /tmp,
// restores it when `devbox install` changed it, removes one the install
// created; the staged bot install (a per-run copy) gets none of this.
func TestDevboxInstallSnippet_RepoLockIsKept(t *testing.T) {
	snippet := devboxInstallSnippet([]devboxProject{
		{label: "repo", dir: "/workspace"},
		{label: "bot", dir: botDevboxDir, stageFrom: "/run/iterion/bundle"},
	})
	for _, want := range []string{
		"_lk=/workspace/devbox.lock",
		"cp \"$_lk\" \"$_pre\"",
		"cmp -s \"$_lk\" \"$_pre\"",
		"cp \"$_pre\" \"$_lk\"",
		"devbox rewrote /workspace/devbox.lock",
		"devbox created /workspace/devbox.lock",
		"rm -f \"$_pre\"",
	} {
		if !strings.Contains(snippet, want) {
			t.Errorf("snippet lacks %q:\n%s", want, snippet)
		}
	}
	if strings.Contains(snippet, "_lk="+botDevboxDir) || strings.Count(snippet, "cmp -s") != 1 {
		t.Errorf("the lock-keeping applies to the in-place repo install only, not to the staged bot copy:\n%s", snippet)
	}
	if strings.Contains(snippet, "/workspace/devbox.lock.iterion") {
		t.Errorf("the pre-install copy must live under /tmp, never beside the lock in the worktree:\n%s", snippet)
	}
}
