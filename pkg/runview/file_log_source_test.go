package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestEnsureLogSource_LiveTailAndRelease verifies the log-stream twin of
// EnsureEventSource: for a run this process didn't launch, EnsureLogSource
// stands up a live buffer fed by an fsnotify tailer of run.log, and the
// refcounted release tears it down. This is the fix for external/dispatcher
// runs whose logs used to only one-shot replay (studio needed a page refresh).
func TestEnsureLogSource_LiveTailAndRelease(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-log-tail"
	runDir := filepath.Join(dir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(runDir, "run.log")
	if err := os.WriteFile(logPath, []byte("first-line\n"), 0o644); err != nil {
		t.Fatalf("seed run.log: %v", err)
	}

	rel, buf := svc.EnsureLogSource(runID)
	if buf == nil {
		t.Fatal("EnsureLogSource returned a nil buffer")
	}
	if svc.GetLogBuffer(runID) == nil {
		t.Fatal("GetLogBuffer nil right after EnsureLogSource")
	}

	sub := buf.Subscribe()
	defer sub.Cancel()

	// Append after subscribing — the tailer must deliver it LIVE.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open run.log: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("second-line\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Sync()

	got := ""
	deadline := time.After(5 * time.Second)
	for !strings.Contains(got, "second-line") {
		select {
		case chunk, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed early; got %q", got)
			}
			got += string(chunk.Bytes)
		case <-deadline:
			t.Fatalf("timeout waiting for live-tailed line; got %q", got)
		}
	}

	// Last release tears down the tailer + buffer.
	rel()
	if svc.GetLogBuffer(runID) != nil {
		t.Fatal("GetLogBuffer non-nil after final release — buffer leaked")
	}
}

// TestEnsureLogSource_Refcount verifies two holders share one source and the
// buffer survives until the LAST release.
func TestEnsureLogSource_Refcount(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const runID = "run-refcount"
	if err := os.MkdirAll(filepath.Join(dir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	rel1, buf1 := svc.EnsureLogSource(runID)
	rel2, buf2 := svc.EnsureLogSource(runID)
	if buf1 == nil || buf2 == nil || buf1 != buf2 {
		t.Fatal("both holders must share the same live buffer")
	}
	rel1()
	if svc.GetLogBuffer(runID) == nil {
		t.Fatal("buffer dropped while a holder remains")
	}
	rel2()
	if svc.GetLogBuffer(runID) != nil {
		t.Fatal("buffer not dropped after final release")
	}
	// Idempotent double-release must not panic or over-decrement.
	rel1()
	rel2()
}
