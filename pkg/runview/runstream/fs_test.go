package runstream

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func newFileSourceFixture(t *testing.T) (*FileSource, *store.FilesystemRunStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return NewFileSource(st, dir, quietLogger()), st, dir
}

func fastTerminalPoll(t *testing.T) {
	t.Helper()
	prev := fileTerminalPollInterval
	fileTerminalPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { fileTerminalPollInterval = prev })
}

// TestFileSource_EventsReplayTailTerminal covers the full cross-store
// event lifecycle: persisted replay, live tail of appended events
// (deduped against the replay), and stream close on a terminal run.json
// flip — with the pre-flip append flushed by the final drain.
func TestFileSource_EventsReplayTailTerminal(t *testing.T) {
	fastTerminalPoll(t)
	src, st, _ := newFileSourceFixture(t)
	const runID = "x-events"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.AppendEvent(context.Background(), runID, store.Event{Type: store.EventNodeStarted, RunID: runID, NodeID: "n"}); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	sub, err := src.SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	var got []*store.Event
	deadline := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream closed during replay; got %d", len(got))
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatalf("timeout on replay; got %d", len(got))
		}
	}
	for i, ev := range got {
		if ev.Seq != int64(i) {
			t.Fatalf("replay[%d].Seq = %d, want %d", i, ev.Seq, i)
		}
	}

	// Live append reaches the subscriber (the foreign daemon keeps
	// writing to events.jsonl).
	if _, err := st.AppendEvent(context.Background(), runID, store.Event{Type: store.EventNodeFinished, RunID: runID, NodeID: "late"}); err != nil {
		t.Fatalf("append live: %v", err)
	}

	deadline = time.After(5 * time.Second)
	for len(got) < 4 {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream closed before the live event; got %d", len(got))
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatal("timeout on live tail")
		}
	}
	if got[3].NodeID != "late" || got[3].Seq != 3 {
		t.Fatalf("live event = %+v, want NodeID late seq 3", got[3])
	}

	// Terminal flip closes the stream after a final drain.
	if err := st.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	deadline = time.After(5 * time.Second)
	for {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				return // closed — the WS layer sends `terminated` on this
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatal("timeout waiting for terminal close")
		}
	}
}

// TestFileSource_LogsDrainTailTerminal covers the cross-store log
// lifecycle: from_offset drain, live appends, terminal close.
func TestFileSource_LogsDrainTailTerminal(t *testing.T) {
	fastTerminalPoll(t)
	src, st, dir := newFileSourceFixture(t)
	const runID = "x-logs"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	logPath := filepath.Join(dir, "runs", runID, "run.log")
	if err := os.WriteFile(logPath, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	sub, err := src.SubscribeLogs(context.Background(), runID, 4)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	defer sub.Close()

	buf := []byte("0123") // client-held prefix; offsets are absolute
	collect := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for string(buf) != want {
			select {
			case chunk, ok := <-sub.Chunks():
				if !ok {
					t.Fatalf("stream closed early; have %q want %q", buf, want)
				}
				if chunk.Offset > int64(len(buf)) {
					t.Fatalf("gap: have %d bytes, chunk at %d", len(buf), chunk.Offset)
				}
				if end := chunk.Offset + int64(len(chunk.Data)); end > int64(len(buf)) {
					buf = append(buf[:chunk.Offset], chunk.Data...)
				}
			case <-deadline:
				t.Fatalf("timeout; have %q want %q", buf, want)
			}
		}
	}
	collect("0123456789")

	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("ABC"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()
	collect("0123456789ABC")

	if err := st.UpdateRunStatus(context.Background(), runID, store.RunStatusCancelled, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sub.Chunks():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for terminal close")
		}
	}
}

// TestFileSource_LogsMissingFileClosesImmediately: a run.log absent at
// subscribe time will never exist — the stream must close with zero
// chunks instead of polling forever.
func TestFileSource_LogsMissingFileClosesImmediately(t *testing.T) {
	src, st, _ := newFileSourceFixture(t)
	const runID = "x-nolog"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	sub, err := src.SubscribeLogs(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	select {
	case chunk, ok := <-sub.Chunks():
		if ok {
			t.Fatalf("unexpected chunk %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close for a missing run.log")
	}
}

// TestFileSource_CloseCancelsSubscriptions: Close on the source ends
// every open subscription.
func TestFileSource_CloseCancelsSubscriptions(t *testing.T) {
	fastTerminalPoll(t)
	src, st, _ := newFileSourceFixture(t)
	const runID = "x-close"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	sub, err := src.SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	_ = src.Close()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sub.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("subscription still open after source Close")
		}
	}
}
