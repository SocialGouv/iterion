package runstream

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// readToEnd collects every event until the subscription's Events close.
func readToEnd(t *testing.T, sub EventSubscription, within time.Duration) []*store.Event {
	t.Helper()
	var got []*store.Event
	deadline := time.After(within)
	for {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				return got
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatalf("stream did not end within %v; got %d events", within, len(got))
		}
	}
}

// F1: the engine writes a terminal status before its terminal event. A stream
// that observed the status must not end before that event is delivered.
func TestFileSourceEndOfEventsBarrier(t *testing.T) {
	prev, prevSettle := fileTerminalPollInterval, fileTerminalSettleWindow
	// A wide settle window keeps the 150 ms hold below well inside it on a
	// loaded host.
	fileTerminalPollInterval, fileTerminalSettleWindow = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { fileTerminalPollInterval, fileTerminalSettleWindow = prev, prevSettle })
	src, st, _ := newFileSourceFixture(t)
	defer src.Close()
	ctx := t.Context()
	const runID = "barrier"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeStarted, NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	sub, err := src.SubscribeEvents(ctx, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case batch, ok := <-sub.Events():
		if !ok || len(batch) != 1 || batch[0].Seq != 0 {
			t.Fatalf("first batch = %+v (open %v)", batch, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("seed event not delivered")
	}
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	// Many 10 ms polls observe the terminal status before its event exists.
	time.Sleep(150 * time.Millisecond)
	if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventRunFinished}); err != nil {
		t.Fatal(err)
	}
	got := readToEnd(t, sub, 5*time.Second)
	for _, ev := range got {
		if ev.Type == store.EventRunFinished && ev.Seq == 1 {
			return
		}
	}
	t.Fatalf("the stream ended on the terminal status before its run_finished event: got %+v", got)
}

// barrierFixture tightens the terminal poll, sets the settle window, and
// returns a FileSource with a run carrying events of the given types.
func barrierFixture(t *testing.T, settle time.Duration, types ...store.EventType) (*FileSource, *store.FilesystemRunStore) {
	t.Helper()
	prev, prevSettle := fileTerminalPollInterval, fileTerminalSettleWindow
	fileTerminalPollInterval, fileTerminalSettleWindow = 10*time.Millisecond, settle
	t.Cleanup(func() { fileTerminalPollInterval, fileTerminalSettleWindow = prev, prevSettle })
	src, st, _ := newFileSourceFixture(t)
	t.Cleanup(func() { _ = src.Close() })
	if _, err := st.CreateRun(t.Context(), "barrier", "wf", nil); err != nil {
		t.Fatal(err)
	}
	for _, typ := range types {
		if _, err := st.AppendEvent(t.Context(), "barrier", store.Event{Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	return src, st
}

// F1: a resume reopens the run's events. An earlier run_failed does not let a
// later terminal status end the stream before the new terminal event.
func TestFileSourceEndOfEventsBarrierResumeReopens(t *testing.T) {
	src, st := barrierFixture(t, 5*time.Second, store.EventRunFailed, store.EventRunResumed)
	sub, err := src.SubscribeEvents(t.Context(), "barrier", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 1, 5*time.Second)
	if err := st.UpdateRunStatus(t.Context(), "barrier", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := st.AppendEvent(t.Context(), "barrier", store.Event{Type: store.EventRunFinished}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range readToEnd(t, sub, 10*time.Second) {
		if ev.Type == store.EventRunFinished {
			return
		}
	}
	t.Fatal("the resumed run's stream ended on a stale run_failed before its run_finished")
}

// F1: a terminal event that never comes is not awaited past the settle window.
func TestFileSourceEndOfEventsBarrierIsBounded(t *testing.T) {
	src, st := barrierFixture(t, 300*time.Millisecond, store.EventNodeStarted)
	sub, err := src.SubscribeEvents(t.Context(), "barrier", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	if err := st.UpdateRunStatus(t.Context(), "barrier", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	expectEnd(t, sub, 5*time.Second, "terminal status without a terminal event")
}

// F1: a terminal event the cursor excludes still ends the events: the
// subscriber already holds it. A lazy reconnect to a finished run ends at once.
func TestFileSourceEndOfEventsBarrierCursorPastTerminalEvent(t *testing.T) {
	src, st := barrierFixture(t, time.Hour, store.EventNodeStarted, store.EventRunFinished)
	if err := st.UpdateRunStatus(t.Context(), "barrier", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	sub, err := src.SubscribeEventsCursor(t.Context(), "barrier", EventCursor{Seq: 1, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if got := readToEnd(t, sub, 5*time.Second); len(got) != 0 {
		t.Fatalf("delivered %+v past the cursor", got)
	}
}
