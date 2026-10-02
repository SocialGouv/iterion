package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// NewStore opens one inotify instance per native board store, unconditionally,
// and logs a refused one with %v and nothing else (store.go, "native index
// watcher unavailable"). On a runner where the inotify budget is shared per
// UID, "too many open files" alone does not say which ceiling was hit — the
// descriptors of this process, or the instances of every container under that
// UID. Routing the default constructor through fswatch is what puts the
// constructor's failure-time observations (real UID, descriptor ceilings,
// inotify sysctls) into that log; this test fails on the bare message.
//
// The descriptor budget is exhausted in a subprocess: the parent, its sibling
// tests and the other pods on the node keep their descriptors and their
// inotify capacity. The child's verdict reaches the parent two ways, and both
// are needed: a test binary that selects no test prints a warning and exits 0,
// so the status alone cannot tell "the assertions passed" from "the child ran
// nothing". The -test.run pattern is derived from t.Name() for the same
// reason, and the parent requires the evidence line in the child's output.
func TestIndexWatcherDefaultConstructorReportsResourceEvidence(t *testing.T) {
	if os.Getenv("ITERION_TEST_NATIVE_WATCHER_FD_CHILD") == "1" {
		var host unix.Rlimit
		_ = unix.Getrlimit(unix.RLIMIT_NOFILE, &host)
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: childNoFile, Max: childNoFile}); err != nil {
			t.Fatalf("lower RLIMIT_NOFILE to %d: %v (host soft=%d hard=%d; an unprivileged process cannot raise a hard limit it lowered)", childNoFile, err, host.Cur, host.Max)
		}
		var held []*os.File
		for {
			f, err := os.Open(os.DevNull)
			if err != nil {
				if !errors.Is(err, syscall.EMFILE) {
					t.Fatal(err)
				}
				break
			}
			held = append(held, f)
		}
		w, err := fsWatcherCtor()()
		for _, f := range held {
			_ = f.Close()
		}
		if w != nil {
			_ = w.Close()
			t.Fatal("watcher unexpectedly opened with the descriptor budget exhausted")
		}
		if !errors.Is(err, syscall.EMFILE) {
			t.Fatalf("lost the original errno: %v", err)
		}
		for _, fragment := range []string{watcherResourcesEvidence, "nofile_soft=64", "ordinary_fd=EMFILE", "real_uid="} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("the refusal lacks %q — the default constructor bypasses fswatch: %v", fragment, err)
			}
		}
		// The parent reads this line: it is the child's proof that it ran.
		fmt.Println(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), "ITERION_TEST_NATIVE_WATCHER_FD_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("descriptor subprocess: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), watcherResourcesEvidence) {
		t.Fatalf("the subprocess exited 0 without printing %q — it selected no test, so nothing was proven:\n%s", watcherResourcesEvidence, out)
	}
}

// childNoFile is the descriptor ceiling the child lowers itself to; small
// enough to exhaust in a few dozen opens, large enough for the test runtime.
const childNoFile = 64

// watcherResourcesEvidence is the prefix fswatch attaches to a resource
// refusal. Asserted on the error AND on the child's output.
const watcherResourcesEvidence = "watcher resources:"
