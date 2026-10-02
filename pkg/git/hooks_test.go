package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A repository a run worked in carries the run's hooks and the run's config —
// `core.fsmonitor` names a program git runs whenever it refreshes the index.
// The git this package runs there executes neither.

func plantHook(t *testing.T, repo, name, body string) {
	t.Helper()
	hooks := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// plantMonitor points the repository's core.fsmonitor at a program that
// leaves marker behind when git runs it.
func plantMonitor(t *testing.T, repo, marker string) {
	t.Helper()
	monitor := filepath.Join(t.TempDir(), "monitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\n: > '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "config", "core.fsmonitor", monitor)
}

func TestStatusRunsNoRepositoryHook(t *testing.T) {
	repo := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	plantHook(t, repo, "post-index-change", "#!/bin/sh\n: > '"+marker+"'\n")
	plantMonitor(t, repo, marker)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(repo); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("reading the status of a run's repository ran a program the repository names")
	}
}

// A delegation snapshots the source run's working tree into a commit another
// run starts from, in the source run's repository: its hooks would run inside
// the snapshot — and a reference-transaction hook refuses the ref the
// snapshot is kept under.
func TestSnapshotWorkingTreeRunsNoRepositoryHook(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("the run's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	plantHook(t, repo, "post-index-change", "#!/bin/sh\n: > '"+marker+"'\n")
	plantHook(t, repo, "reference-transaction", "#!/bin/sh\n[ \"$1\" = prepared ] && exit 1\nexit 0\n")
	_, tree, err := SnapshotWorkingTree(repo, "run-a/fp")
	if err != nil {
		t.Fatalf("SnapshotWorkingTree: %v — a hook of the run's repository decided the snapshot", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the snapshot ran a hook of the run's repository")
	}
	ls := exec.Command("git", NoAutoMaintenance("ls-tree", "-r", "--name-only", tree)...)
	ls.Dir = repo
	ls.Env = gitTestEnv()
	out, err := ls.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "new.txt") {
		t.Fatalf("the snapshot lost the run's change:\n%s", out)
	}
}
