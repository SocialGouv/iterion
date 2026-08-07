package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A recovery fork EXECUTING for the ticket occupies the slot its dead
// parent held: forks start via Resume, which never consults
// pipelineQueue, so nothing else accounts for them — and the admission
// loop would over-admit past max-concurrent-pipelines (R00ef2c).
func TestPipelineTicketHoldsSlotRunningFork(t *testing.T) {
	issue := &native.Issue{ID: "native:1", Bot: "review", State: native.StateInProgress}
	terminal := map[string]struct{}{}
	cases := []struct {
		name string
		root *store.Run
		want bool
	}{
		{"running fork holds", &store.Run{ForkedFrom: "p", Status: store.RunStatusRunning}, true},
		{"paused fork holds", &store.Run{ForkedFrom: "p", Status: store.RunStatusPausedWaitingHuman}, true},
		{"failed parent holds", &store.Run{Status: store.RunStatusFailed}, true},
		{"finished fork releases", &store.Run{ForkedFrom: "p", Status: store.RunStatusFinished}, false},
		{"parked fork shell releases", &store.Run{ForkedFrom: "p", Status: store.RunStatusCancelled}, false},
		{"running non-fork does not hold", &store.Run{Status: store.RunStatusRunning}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pipelineTicketHoldsSlot(issue, tc.root, terminal); got != tc.want {
				t.Errorf("pipelineTicketHoldsSlot(%+v) = %v, want %v", tc.root, got, tc.want)
			}
		})
	}
}
