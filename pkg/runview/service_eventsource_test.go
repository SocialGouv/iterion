package runview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestEnsureEventSource_RefcountedTailer verifies the on-demand file
// tailer that bridges a not-in-process run's events.jsonl into the
// broker (the dispatcher-run live-WS fix): a single tailer is shared
// across holders, keeps delivering while ≥1 holder remains, and is
// torn down (map entry removed) only when the last holder releases.
func TestEnsureEventSource_RefcountedTailer(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-ensure"
	if _, err := svc.store.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	sub := svc.broker.Subscribe(runID)
	defer sub.Cancel()

	eventsPath := filepath.Join(dir, "runs", runID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer f.Close()

	appendEvent := func(seq int64, et store.EventType) {
		evt := store.Event{Seq: seq, Type: et, RunID: runID, Timestamp: time.Now().UTC()}
		buf, _ := json.Marshal(evt)
		if _, werr := f.Write(append(buf, '\n')); werr != nil {
			t.Fatalf("write: %v", werr)
		}
		_ = f.Sync()
	}
	expect := func(want store.EventType) {
		t.Helper()
		select {
		case evt, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed early; wanted %q", want)
			}
			if evt.Type != want {
				t.Fatalf("got %q, want %q", evt.Type, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	// Two holders → one tailer.
	release1 := svc.ensureEventSource(runID)
	release2 := svc.ensureEventSource(runID)
	svc.fileSrcMu.Lock()
	if h := svc.fileSrcs[runID]; h == nil || h.refs != 2 {
		svc.fileSrcMu.Unlock()
		t.Fatalf("after 2 ensureEventSource: want one handle refs=2, got %+v", h)
	}
	svc.fileSrcMu.Unlock()

	appendEvent(0, store.EventRunStarted)
	expect(store.EventRunStarted)

	// Releasing one holder must NOT stop delivery.
	release1()
	appendEvent(1, store.EventNodeStarted)
	expect(store.EventNodeStarted)

	svc.fileSrcMu.Lock()
	if h := svc.fileSrcs[runID]; h == nil || h.refs != 1 {
		svc.fileSrcMu.Unlock()
		t.Fatalf("after 1 release: want handle refs=1, got %+v", h)
	}
	svc.fileSrcMu.Unlock()

	// Last release tears the tailer down and removes the map entry.
	release2()
	svc.fileSrcMu.Lock()
	_, present := svc.fileSrcs[runID]
	svc.fileSrcMu.Unlock()
	if present {
		t.Fatalf("after last release: handle should be removed")
	}

	// Double-release is a no-op (sync.Once guard), must not panic or
	// underflow the refcount of a re-created handle.
	release2()
	release1()
}
