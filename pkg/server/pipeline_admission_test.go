package server

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The admission loop's pick order IS the prioritization contract of the
// pipeline board's Todo lane: highest Priority launches first, equal
// priorities go oldest-first. A regression here silently reorders which
// pipeline the operator's ranking starts next.
func TestSortReadyTickets_PriorityDescThenOldest(t *testing.T) {
	at := func(min int) time.Time {
		return time.Date(2026, 7, 15, 10, min, 0, 0, time.UTC)
	}
	tickets := []*native.Issue{
		{ID: "old-low", Priority: 1, CreatedAt: at(0)},
		{ID: "new-high", Priority: 5, CreatedAt: at(30)},
		{ID: "old-mid", Priority: 3, CreatedAt: at(5)},
		{ID: "new-mid", Priority: 3, CreatedAt: at(20)},
		{ID: "unprioritized", Priority: 0, CreatedAt: at(1)},
	}
	sortReadyTickets(tickets)
	got := make([]string, len(tickets))
	for i, iss := range tickets {
		got[i] = iss.ID
	}
	want := []string{"new-high", "old-mid", "new-mid", "old-low", "unprioritized"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launch order = %v, want %v", got, want)
		}
	}
}
