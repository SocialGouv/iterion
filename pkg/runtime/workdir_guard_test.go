package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// staged makes dir with a file and a subdirectory in it: only a recursive
// removal gets rid of it.
func staged(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The reclaim sorts what outlived a suite by cause — never released,
// unremovable on release, re-created after release — removes it by recorded
// path, and deregisters it; a released dir that is gone is not reported, and
// anything outside the throw-away prefix is refused and survives: a
// mis-recorded key must never become a RemoveAll.
func TestReclaimThrowAwayWorkDirs_SortsByCauseAndRemovesOnlyItsOwn(t *testing.T) {
	var live, ever sync.Map
	root := t.TempDir()
	never := staged(t, filepath.Join(root, throwAwayWorkDirPrefix+"1"))
	unremovable := staged(t, filepath.Join(root, throwAwayWorkDirPrefix+"2"))
	recreated := staged(t, filepath.Join(root, "nested", "TestX", "001", throwAwayWorkDirPrefix+"3"))
	gone := filepath.Join(root, throwAwayWorkDirPrefix+"4")
	foreign := staged(t, filepath.Join(root, "not-a-throw-away"))
	for _, d := range []string{never, unremovable, recreated, gone, foreign} {
		ever.Store(d, struct{}{})
	}
	live.Store(never, nil)
	live.Store(unremovable, errors.New("permission denied"))
	live.Store(foreign, nil)

	leaks, err := reclaimThrowAwayWorkDirs(&live, &ever)
	if want := []string{never, foreign}; !slices.Equal(leaks.NeverReleased, want) {
		t.Fatalf("never released = %v, want %v", leaks.NeverReleased, want)
	}
	if len(leaks.Unremovable) != 1 || !strings.HasPrefix(leaks.Unremovable[0], unremovable+" ") || !strings.Contains(leaks.Unremovable[0], "permission denied") {
		t.Fatalf("unremovable = %v, want %s with its error", leaks.Unremovable, unremovable)
	}
	if want := []string{recreated}; !slices.Equal(leaks.Recreated, want) {
		t.Fatalf("re-created = %v, want %v", leaks.Recreated, want)
	}
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want the refusal of %s", err, foreign)
	}
	for _, d := range []string{never, unremovable, recreated} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("%s survived its reclaim (lstat err=%v)", d, err)
		}
		if _, ok := live.Load(d); ok {
			t.Fatalf("%s is still registered after its reclaim", d)
		}
	}
	if _, err := os.Lstat(filepath.Join(foreign, "sub", "f")); err != nil {
		t.Fatalf("the refused %s was touched: %v", foreign, err)
	}
	if _, ok := live.Load(foreign); !ok {
		t.Fatalf("the refused %s was deregistered", foreign)
	}
}

// The test main's verdict: the run's own code when nothing is left, a
// failure naming the cause otherwise.
func TestNoThrowAwayWorkDirLeaks(t *testing.T) {
	t.Run("nothing left keeps the run's code", func(t *testing.T) {
		var live, ever sync.Map
		for _, code := range []int{0, 3} {
			if got := noThrowAwayWorkDirLeaks(&live, &ever, code, io.Discard); got != code {
				t.Fatalf("verdict %d for a clean run of code %d", got, code)
			}
		}
	})
	for _, tc := range []struct {
		name, want string
		live       any
		isLive     bool
	}{
		{name: "never released", want: "never released", isLive: true},
		{name: "unremovable", want: "could not be removed", live: errors.New("permission denied"), isLive: true},
		{name: "re-created after its release", want: "re-created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var live, ever sync.Map
			d := staged(t, filepath.Join(t.TempDir(), throwAwayWorkDirPrefix+"5"))
			ever.Store(d, struct{}{})
			if tc.isLive {
				live.Store(d, tc.live)
			}
			var out bytes.Buffer
			if got := noThrowAwayWorkDirLeaks(&live, &ever, 0, &out); got != 1 || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("verdict %d, report %q; want 1 naming %q", got, out.String(), tc.want)
			}
		})
	}
}

// A throw-away workdir that cannot be removed stays registered, so the test
// main still reports it instead of losing it along with the engine's memory
// of it.
func TestReleaseTempWorkDir_AnUnremovableDirStaysRegistered(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes entries of a read-only directory: the unremovable dir cannot be staged")
	}
	t.Setenv("TMPDIR", t.TempDir())
	e := &Engine{}
	e.defaultWorkDir()
	dir := e.workDirTemp
	if dir == "" {
		t.Fatal("no throw-away workdir: the test no longer runs from the package directory")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(locked, 0o700)
		liveThrowAwayWorkDirs.Delete(dir)
	})

	e.releaseTempWorkDir()
	v, ok := liveThrowAwayWorkDirs.Load(dir)
	if !ok {
		t.Fatalf("the unremovable throw-away workdir %s was deregistered", dir)
	}
	if _, isErr := v.(error); !isErr {
		t.Fatalf("the unremovable throw-away workdir %s is registered without its removal error (%v)", dir, v)
	}
}

// A throw-away workdir stays one after its owner released it: a child engine
// handed it — still running, as an abandoned fan-out branch's child can be —
// records nothing on its run.
func TestThrowAwayWorkDir_StaysUnrecordableAfterItsRelease(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	owner := &Engine{}
	owner.defaultWorkDir()
	dir := owner.workDirTemp
	if dir == "" {
		t.Fatal("no throw-away workdir: the test no longer runs from the package directory")
	}
	owner.releaseTempWorkDir()

	exec := newStubExecutor()
	exec.on("analyze", func(map[string]any) (map[string]any, error) { return map[string]any{"summary": "needs review"}, nil })
	s := tmpStore(t)
	child := New(humanWorkflow(), s, exec, WithWorkDir(dir))
	if err := child.Run(context.Background(), "child-after-release", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("child Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(context.Background(), "child-after-release")
	if err != nil {
		t.Fatal(err)
	}
	if r.WorkDir != "" {
		t.Fatalf("the child recorded %q, its parent's released throw-away workdir", r.WorkDir)
	}
}
