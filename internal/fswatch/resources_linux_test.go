package fswatch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Exhaust only a subprocess's descriptor budget. The parent, other tests and
// other pods retain their descriptors and inotify capacity.
func TestWatcherDescriptorExhaustionReportsActualProcessLimits(t *testing.T) {
	if os.Getenv("ITERION_TEST_WATCHER_FD_CHILD") == "1" {
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 64, Max: 64}); err != nil {
			t.Fatal(err)
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
		for _, fragment := range []string{"watcher resources:", "nofile_soft=64", "ordinary_fd=EMFILE", "real_uid="} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("missing %q in %v", fragment, err)
			}
		}
		fmt.Println(err)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWatcherDescriptorExhaustionReportsActualProcessLimits$")
	cmd.Env = append(os.Environ(), "ITERION_TEST_WATCHER_FD_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("descriptor subprocess: %v\n%s", err, out)
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
