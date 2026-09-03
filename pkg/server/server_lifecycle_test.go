package server

import "testing"

// The forge refresh worker only refreshes tokens expiring within Lead, on a
// fixed ticker: with Lead at or below the tick period, a 1h installation
// token can expire between two ticks, the refresh phase-locks onto the
// expiry instant, and every run launched just before that instant is sealed
// a token with seconds of life (see the constants' comment for the incident
// this encodes).
func TestForgeRefreshLeadExceedsTickPeriod(t *testing.T) {
	if forgeRefreshLead <= forgeRefreshTick {
		t.Fatalf("forgeRefreshLead (%v) must exceed forgeRefreshTick (%v): a lead at or below the tick period lets a token expire between two sweeps and phase-locks its refresh onto the expiry instant", forgeRefreshLead, forgeRefreshTick)
	}
}
