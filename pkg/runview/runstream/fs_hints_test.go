package runstream

import (
	"errors"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SocialGouv/iterion/pkg/store"
)

// seedHintRun creates a running run with one event (seq 0).
func seedHintRun(t *testing.T, st *store.FilesystemRunStore, runID string) {
	t.Helper()
	if _, err := st.CreateRun(t.Context(), runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(t.Context(), runID, store.Event{Type: store.EventNodeStarted, NodeID: "seed"}); err != nil {
		t.Fatal(err)
	}
}

// receiveSeq waits for a batch whose last event has seq want.
func receiveSeq(t *testing.T, sub EventSubscription, want int64, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream ended before seq %d", want)
			}
			if len(batch) > 0 && batch[len(batch)-1].Seq == want {
				return
			}
		case <-deadline:
			t.Fatalf("seq %d not delivered within %v", want, within)
		}
	}
}

// expectOpen fails when the subscription delivers or ends within d.
func expectOpen(t *testing.T, sub EventSubscription, d time.Duration, what string) {
	t.Helper()
	select {
	case batch, ok := <-sub.Events():
		t.Fatalf("%s: got %+v (open %v)", what, batch, ok)
	case <-time.After(d):
	}
}

// expectEnd waits for the Events channel to close.
func expectEnd(t *testing.T, sub EventSubscription, within time.Duration, what string) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case _, ok := <-sub.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("%s: the stream did not end within %v", what, within)
		}
	}
}

// T4: with notifications unavailable and a polling cadence out of reach, a
// Drain hint alone drains the retained selection.
func TestFileSourceDrainHintWithoutWatcher(t *testing.T) {
	prevWatcher, prevFallback := newTailWatcher, tailPollFallbackInterval
	newTailWatcher = func() (*fsnotify.Watcher, error) { return nil, errors.New("notifications unavailable") }
	tailPollFallbackInterval = time.Hour
	t.Cleanup(func() { newTailWatcher, tailPollFallbackInterval = prevWatcher, prevFallback })
	src, st, _ := newFileSourceFixture(t)
	defer src.Close()
	const runID = "wake"
	seedHintRun(t, st, runID)
	wake := make(chan struct{}, 1)
	sub, err := src.SubscribeEventsHinted(t.Context(), runID, EventCursor{}, FileStreamHints{Drain: wake, TerminalPoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	if _, err := st.AppendEvent(t.Context(), runID, store.Event{Type: store.EventNodeFinished, NodeID: "late"}); err != nil {
		t.Fatal(err)
	}
	expectOpen(t, sub, 500*time.Millisecond, "an append drained without a wake")
	wake <- struct{}{}
	receiveSeq(t, sub, 1, 5*time.Second)
}

// T5r: a Terminal hint checks the status at once and ends the stream after a
// final drain; without it a far terminal poll leaves the stream open. The
// hint is the producer's barrier: no terminal event is awaited.
func TestFileSourceTerminalHint(t *testing.T) {
	prev := fileTerminalSettleWindow
	fileTerminalSettleWindow = time.Hour
	t.Cleanup(func() { fileTerminalSettleWindow = prev })
	src, st, _ := newFileSourceFixture(t)
	defer src.Close()
	const runID = "hint"
	seedHintRun(t, st, runID)
	term := make(chan struct{}, 1)
	sub, err := src.SubscribeEventsHinted(t.Context(), runID, EventCursor{}, FileStreamHints{Terminal: term, TerminalPoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	// While the run is running, the poller consumes a hint (and finds no
	// terminal status): nothing else reads it before a terminal status.
	term <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for len(term) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the poller never consumed a Terminal hint")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The poller's immediate first check has run long before this.
	time.Sleep(100 * time.Millisecond)
	if err := st.UpdateRunStatus(t.Context(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	expectOpen(t, sub, 500*time.Millisecond, "a terminal status ended the stream without a poll or a hint")
	// One hint: the poller confirms the terminal status on it, and that
	// confirmation alone ends the stream (no settle wait, window 1 h).
	term <- struct{}{}
	expectEnd(t, sub, 5*time.Second, "terminal hint")
}

// T5r: a positive TerminalPoll replaces the default poll interval.
func TestFileSourceTerminalPollHint(t *testing.T) {
	prevPoll, prevSettle := fileTerminalPollInterval, fileTerminalSettleWindow
	fileTerminalPollInterval, fileTerminalSettleWindow = time.Hour, 100*time.Millisecond
	t.Cleanup(func() { fileTerminalPollInterval, fileTerminalSettleWindow = prevPoll, prevSettle })
	src, st, _ := newFileSourceFixture(t)
	defer src.Close()
	const runID = "poll"
	seedHintRun(t, st, runID)
	sub, err := src.SubscribeEventsHinted(t.Context(), runID, EventCursor{}, FileStreamHints{TerminalPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	// The poller's immediate first check has run long before this: only a
	// later poll can observe the flip.
	time.Sleep(100 * time.Millisecond)
	if err := st.UpdateRunStatus(t.Context(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	expectEnd(t, sub, 2*time.Second, "10 ms terminal poll")
}

// T16: a closed FileSource refuses a subscription with the typed, permanent
// ErrSourceClosed.
func TestFileSourceClosedRefusesWithErrSourceClosed(t *testing.T) {
	src, st, _ := newFileSourceFixture(t)
	seedHintRun(t, st, "closed")
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	for name, subscribe := range map[string]func() error{
		"events": func() error { _, err := src.SubscribeEvents(t.Context(), "closed", 0); return err },
		"hinted": func() error {
			_, err := src.SubscribeEventsHinted(t.Context(), "closed", EventCursor{}, FileStreamHints{})
			return err
		},
		"logs": func() error { _, err := src.SubscribeLogs(t.Context(), "closed", 0); return err },
	} {
		err := subscribe()
		if !errors.Is(err, ErrSourceClosed) || !IsPermanentError(err) {
			t.Fatalf("%s after Close = %v, want the permanent ErrSourceClosed", name, err)
		}
	}
}
