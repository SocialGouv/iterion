package fswatch

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

// childNoFile is the descriptor ceiling the child lowers itself to; small
// enough to exhaust in a few dozen opens, large enough for the test runtime.
const childNoFile = 64

// watcherResourcesEvidence is the prefix this package attaches to a resource
// refusal. Asserted on the error AND on the child's output.
const watcherResourcesEvidence = "watcher resources:"

// Exhaust only a subprocess's descriptor budget. The parent, other tests and
// other pods retain their descriptors and inotify capacity. The child's
// verdict reaches the parent two ways, and both are needed: a test binary
// that selects no test prints a warning and exits 0, so the status alone
// cannot tell "the assertions passed" from "the child ran nothing". The
// -test.run pattern is derived from t.Name() for the same reason, and the
// parent requires the evidence line in the child's output.
func TestWatcherDescriptorExhaustionReportsActualProcessLimits(t *testing.T) {
	if os.Getenv("ITERION_TEST_WATCHER_FD_CHILD") == "1" {
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
		w, err := NewWatcher()
		for _, f := range held {
			_ = f.Close()
		}
		if w != nil {
			_ = w.Close()
			t.Fatal("watcher unexpectedly opened")
		}
		if !errors.Is(err, syscall.EMFILE) {
			t.Fatalf("lost original errno: %v", err)
		}
		for _, fragment := range []string{watcherResourcesEvidence, "nofile_soft=64", "ordinary_fd=EMFILE", "real_uid="} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("missing %q in %v", fragment, err)
			}
		}
		// The parent reads this line: it is the child's proof that it ran.
		fmt.Println(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), "ITERION_TEST_WATCHER_FD_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("descriptor subprocess: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), watcherResourcesEvidence) {
		t.Fatalf("the subprocess exited 0 without printing %q — it selected no test, so nothing was proven:\n%s", watcherResourcesEvidence, out)
	}
}

func TestWatcherResourceEvidenceDoesNotInventTheCause(t *testing.T) {
	original := fmt.Errorf("inotify init: %w", syscall.EMFILE)
	err := resourceError(original)
	if !errors.Is(err, syscall.EMFILE) || !strings.Contains(err.Error(), "ordinary_fd=open_ok") || !strings.Contains(err.Error(), "max_user_instances=") {
		t.Fatalf("missing evidence with descriptor headroom: %v", err)
	}
	denied := fmt.Errorf("watcher: %w", syscall.EACCES)
	if resourceError(denied) != denied {
		t.Fatal("non-resource error was changed")
	}
}
