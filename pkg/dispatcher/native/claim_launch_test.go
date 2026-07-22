package native

import (
	"sync"
	"testing"
)

// TestClaimForLaunchSingleWinner proves the CAS: exactly one of many
// concurrent callers transitions a Ready ticket to InProgress (PR #193 M2).
func TestClaimForLaunchSingleWinner(t *testing.T) {
	s := newTestStore(t)
	iss, err := s.Create(Issue{Title: "launch-me", State: StateReady, Bot: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const racers = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, won, err := s.ClaimForLaunch(iss.ID)
			if err != nil {
				t.Errorf("ClaimForLaunch: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
				if claimed == nil || claimed.State != StateInProgress {
					t.Errorf("winner got %v, want state=%s", claimed, StateInProgress)
				}
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (double-launch race)", wins)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateInProgress {
		t.Fatalf("final state = %s, want %s", got.State, StateInProgress)
	}
}

// TestClaimForLaunchNotReady confirms a ticket that is not in StateReady is
// not claimable — no error, won=false — so the CAS gates only launch-eligible
// tickets.
func TestClaimForLaunchNotReady(t *testing.T) {
	s := newTestStore(t)
	iss, err := s.Create(Issue{Title: "backlog", State: StateInbox, Bot: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claimed, won, err := s.ClaimForLaunch(iss.ID)
	if err != nil {
		t.Fatalf("ClaimForLaunch: %v", err)
	}
	if won || claimed != nil {
		t.Fatalf("claimed a non-Ready ticket: won=%v claimed=%v", won, claimed)
	}
	got, _ := s.Get(iss.ID)
	if got.State != StateInbox {
		t.Fatalf("state changed to %s, want it left at %s", got.State, StateInbox)
	}
}
