package runview

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
