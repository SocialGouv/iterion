package runview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A run id whose document is gone is a stale index entry, not a corrupt
// run: it logs once at Debug — never at Warn — and is not re-logged on
// every UI poll. A genuinely unreadable document rates a Warn, also
// once: a corrupt run.json does not heal between two polls, and the
// repeated lines were drowning the instance log (several per second).
func TestListRunRecordsCtxSkipsStaleAndCorruptRunsQuietly(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	// One healthy run so the listing has something to return.
	if _, err := st.CreateRun(context.Background(), "run-ok", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Stale index entry: the id exists, run.json does not.
	if err := os.MkdirAll(filepath.Join(storeDir, "runs", "run-gone"), 0o755); err != nil {
		t.Fatalf("mkdir stale run: %v", err)
	}
	// Corrupt document: present but unreadable.
	corruptDir := filepath.Join(storeDir, "runs", "run-corrupt")
	if err := os.MkdirAll(corruptDir, 0o755); err != nil {
		t.Fatalf("mkdir corrupt run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "run.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt run.json: %v", err)
	}

	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Three polls, as the UI would do.
	for i := 0; i < 3; i++ {
		runs, err := svc.ListRunRecordsCtx(context.Background(), ListFilter{})
		if err != nil {
			t.Fatalf("ListRunRecordsCtx #%d: %v", i, err)
		}
		if len(runs) != 1 || runs[0].ID != "run-ok" {
			t.Fatalf("listing #%d = %+v, want only run-ok", i, runs)
		}
	}

	log := buf.String()
	lineCount := func(id string) int {
		n := 0
		for _, line := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
			if strings.Contains(line, id) {
				n++
				if id == "run-gone" && !strings.Contains(line, "🔍") {
					t.Errorf("stale run-gone must log at Debug (🔍), got: %s", line)
				}
				if id == "run-corrupt" && !strings.Contains(line, "⚠️") {
					t.Errorf("corrupt run-corrupt must log at Warn (⚠️), got: %s", line)
				}
			}
		}
		return n
	}
	if got := lineCount("run-gone"); got != 1 {
		t.Errorf("run-gone logged on %d lines over 3 polls, want 1 (log-once)\nlog:\n%s", got, log)
	}
	if got := lineCount("run-corrupt"); got != 1 {
		t.Errorf("run-corrupt logged on %d lines over 3 polls, want 1 (log-once)\nlog:\n%s", got, log)
	}
}

// A context-derived failure (client disconnect, request deadline — the
// mongo store honours the caller's ctx) is transient: the document may
// be perfectly readable. It must be reported WITHOUT memoising —
// otherwise one cancelled listing marks every remaining id "corrupt"
// and permanently silences the diagnostic the Warn branch exists for.
func TestLogSkippedRunDoesNotMemoizeTransientErrors(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	svc := &Service{logger: logger}

	ctxErr := fmt.Errorf("store/mongo: find run: %w", context.Canceled)
	for i := 0; i < 3; i++ {
		svc.logSkippedRun("run-transient", ctxErr)
	}
	log := buf.String()
	lineCount := 0
	for _, line := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		if strings.Contains(line, "run-transient") {
			lineCount++
			if strings.Contains(line, "⚠️") {
				t.Errorf("a transient ctx error must not rate a Warn, got: %s", line)
			}
		}
	}
	if lineCount != 3 {
		t.Errorf("transient error logged %d times over 3 calls, want 3 (reported, never memoised)\nlog:\n%s", lineCount, log)
	}

	// And a genuinely unreadable document for the SAME id still warns:
	// the transient calls must not have consumed its one Warn slot.
	svc.logSkippedRun("run-transient", errors.New("store: decode run run-transient: invalid character"))
	if !strings.Contains(buf.String(), "⚠️") {
		t.Errorf("a later corrupt document for the same id must still Warn\nlog:\n%s", buf.String())
	}
}

// The Warn dedup is time-boxed, not once-forever: a transient blip
// (mongo server-selection, EMFILE) that marked an id must not silence
// its diagnostic for the process lifetime — after the interval, the
// same unreadable run warns again.
func TestLogSkippedRunRelogsWarnAfterInterval(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	svc := &Service{logger: logger}

	decodeErr := errors.New("store: decode run run-x: invalid character")
	svc.logSkippedRun("run-x", decodeErr)
	svc.logSkippedRun("run-x", decodeErr) // inside the interval — deduped
	warnCount := func() int {
		n := 0
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.Contains(line, "run-x") && strings.Contains(line, "⚠️") {
				n++
			}
		}
		return n
	}
	if got := warnCount(); got != 1 {
		t.Fatalf("warn count inside the interval = %d, want 1", got)
	}
	// Simulate the interval elapsing (the map holds the last-log time).
	svc.skipRunLogged.Store("corrupt:run-x", time.Now().Add(-skipWarnRelogAfter-time.Second))
	svc.logSkippedRun("run-x", decodeErr)
	if got := warnCount(); got != 2 {
		t.Errorf("warn count after the interval = %d, want 2 (the diagnostic re-arms)", got)
	}
}
