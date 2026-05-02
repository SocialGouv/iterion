package runview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestFileEventSource_TailsAppendsToBroker writes a sequence of
// events to events.jsonl while a tailer goroutine is watching, and
// asserts each one shows up at the broker subscriber in order.
func TestFileEventSource_TailsAppendsToBroker(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-tail"
	if _, err := svc.store.CreateRun(runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	sub := svc.broker.Subscribe(runID)
	defer sub.Cancel()

	done := make(chan struct{})
	defer close(done)
	startEventSource(svc, runID, done)

	// Write three events one at a time, each with a small gap so the
	// tailer's fsnotify Write event fires per append. We assert
	// arrival order against the wire.
	eventsPath := filepath.Join(dir, "runs", runID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer f.Close()

	want := []store.EventType{
		store.EventRunStarted,
		store.EventNodeStarted,
		store.EventRunFinished,
	}
	for i, et := range want {
		evt := store.Event{Seq: int64(i), Type: et, RunID: runID, Timestamp: time.Now().UTC()}
		buf, err := json.Marshal(evt)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(buf, '\n')); err != nil {
			t.Fatalf("write event %d: %v", i, err)
		}
		_ = f.Sync()
	}

	got := make([]store.EventType, 0, len(want))
	deadline := time.After(5 * time.Second)
	for len(got) < len(want) {
		select {
		case evt, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed early; got %v", got)
			}
			got = append(got, evt.Type)
		case <-deadline:
			t.Fatalf("timed out: got %v, want %v", got, want)
		}
	}

	for i, w := range want {
		if got[i] != w {
			t.Errorf("event %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestFileEventSource_DrainsExistingBacklogOnStart verifies the
// tailer replays events that were written BEFORE it started — a
// detached runner can write its first events microseconds before the
// server's tailer is wired up, and we don't want to lose those.
func TestFileEventSource_DrainsExistingBacklogOnStart(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-backlog"
	if _, err := svc.store.CreateRun(runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Write the events BEFORE subscribing or starting the tailer.
	eventsPath := filepath.Join(dir, "runs", runID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 2; i++ {
		evt := store.Event{Seq: int64(i), Type: store.EventNodeStarted, RunID: runID, Timestamp: time.Now().UTC()}
		buf, _ := json.Marshal(evt)
		f.Write(append(buf, '\n'))
	}
	f.Close()

	sub := svc.broker.Subscribe(runID)
	defer sub.Cancel()

	done := make(chan struct{})
	defer close(done)
	startEventSource(svc, runID, done)

	for i := 0; i < 2; i++ {
		select {
		case evt := <-sub.C:
			if evt.Type != store.EventNodeStarted {
				t.Errorf("event %d type = %q, want node_started", i, evt.Type)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for backlog event %d", i)
		}
	}
}

// TestFileEventSource_HandlesPartialLine verifies the tailer doesn't
// publish a half-written event (no trailing newline) — a race that
// can happen if the watcher fires while the runner's appendEvent is
// halfway through Fprintln.
func TestFileEventSource_HandlesPartialLine(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-partial"
	if _, err := svc.store.CreateRun(runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	sub := svc.broker.Subscribe(runID)
	defer sub.Cancel()

	done := make(chan struct{})
	defer close(done)
	startEventSource(svc, runID, done)

	eventsPath := filepath.Join(dir, "runs", runID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, _ := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	defer f.Close()

	// Write a partial line first (no \n), then complete it later.
	evt := store.Event{Seq: 1, Type: store.EventRunStarted, RunID: runID, Timestamp: time.Now().UTC()}
	buf, _ := json.Marshal(evt)
	half := buf[:len(buf)/2]
	f.Write(half)
	f.Sync()

	// Tailer should NOT have published yet — there's no terminating newline.
	select {
	case got := <-sub.C:
		t.Fatalf("subscriber received event from partial line: %+v", got)
	case <-time.After(300 * time.Millisecond):
		// Good — silence is correct.
	}

	// Complete the line.
	f.Write(buf[len(buf)/2:])
	f.Write([]byte("\n"))
	f.Sync()

	select {
	case got := <-sub.C:
		if got.Type != store.EventRunStarted {
			t.Errorf("got %+v, want type=run_started", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for completed event")
	}
}
