package native

import (
	"sync"
	"testing"
	"time"
)

// TestSubscribe_DeliversNewTransitions verifies the events.jsonl tailer
// delivers issue_state_changed events appended after Subscribe (with the
// from/to payload) and never replays history written before it.
func TestSubscribe_DeliversNewTransitions(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Pre-subscribe activity: the issue_created event must NOT be
	// replayed to a subscriber that starts at EOF.
	iss, err := s.Create(Issue{Title: "watch me", State: "backlog"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var (
		mu   sync.Mutex
		seen []Event
	)
	cancel, err := s.Subscribe(func(e Event) {
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
	})
	if err != nil {
		t.Skipf("Subscribe unavailable on host (fsnotify): %v", err)
	}
	t.Cleanup(cancel)

	// Post-subscribe transition: must be delivered.
	if _, err := s.SetState(iss.ID, "ready"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	got := waitForStateEvent(t, &mu, &seen, iss.ID)
	if from, _ := got.Payload["from"].(string); from != "backlog" {
		t.Errorf("payload.from = %q, want backlog", from)
	}
	if to, _ := got.Payload["to"].(string); to != "ready" {
		t.Errorf("payload.to = %q, want ready", to)
	}

	// EOF-start guard: a replay of pre-subscribe history would surface
	// the issue_created event. It must never appear.
	mu.Lock()
	defer mu.Unlock()
	for _, e := range seen {
		if e.Type == EvtIssueCreated {
			t.Fatalf("issue_created replayed — tailer did not start at EOF")
		}
	}
}

// TestSubscribe_CancelStopsDelivery verifies cancel() halts the tailer:
// transitions after cancel are not delivered.
func TestSubscribe_CancelStopsDelivery(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	iss, err := s.Create(Issue{Title: "x", State: "backlog"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var (
		mu    sync.Mutex
		count int
	)
	cancel, err := s.Subscribe(func(Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	if err != nil {
		t.Skipf("Subscribe unavailable on host (fsnotify): %v", err)
	}
	cancel()

	if _, err := s.SetState(iss.ID, "ready"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	// Give any (buggy) delivery a chance to land.
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Errorf("delivered %d events after cancel, want 0", count)
	}
}

func waitForStateEvent(t *testing.T, mu *sync.Mutex, seen *[]Event, issueID string) Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, e := range *seen {
			if e.Type == EvtIssueState && e.IssueID == issueID {
				mu.Unlock()
				return e
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("issue_state_changed for %s not delivered within deadline", issueID)
	return Event{}
}
