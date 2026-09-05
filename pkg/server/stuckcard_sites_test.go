package server

import (
	"os"
	"regexp"
	"testing"
)

// TestEveryStuckCardSiteNamesTheRecordedRun: the decision table tells a
// pruned pointer (StuckGiveUp) from a card that never recorded a run
// (StuckReleaseOnly) through StuckCard.RecordedRunID. A construction
// site that leaves it empty silently demotes every pruned pointer it
// judges to the release-only row — the gate-off sweeps did exactly that
// while the gated reap did not, two authorities judging one card
// differently. Pin every literal in this file.
func TestEveryStuckCardSiteNamesTheRecordedRun(t *testing.T) {
	src, err := os.ReadFile("boarddispatch.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`(?s)dispatcher\.StuckCard\{.*?\n\t*\}`)
	sites := literal.FindAll(src, -1)
	if len(sites) < 3 {
		t.Fatalf("expected the three StuckCard sites (reap, un-leased sweep, recovery sweep), found %d", len(sites))
	}
	for _, site := range sites {
		if !regexp.MustCompile(`RecordedRunID:\s*cand\.Claim\.LastRunID`).Match(site) {
			t.Fatalf("a StuckCard site does not name the recorded run — a pruned pointer there reads as \"no run\":\n%s", site)
		}
	}
}
