package workspacetrack

import "testing"

// TestStatusBetween_ReportsSkippedPaths is Revi's R89a3d1.
//
// Capture records a file over MaxFileBytes in Snapshot.Skipped and writes
// NO Entry for it. A range derived from Entries alone therefore did two
// wrong things on the in-place backend — the default run shape this panel
// serves: a newly produced oversized deliverable (the media export the
// run exists to produce) was absent from the range entirely, and a file
// that WAS captured and then crossed the cap came back as "D", telling
// the reviewer it had been deleted.
func TestStatusBetween_ReportsSkippedPaths(t *testing.T) {
	base := &Snapshot{
		Entries: []Entry{
			{Path: "kept.txt", Hash: "h1", Size: 4},
			{Path: "grows.bin", Hash: "h2", Size: 8},
		},
		Skipped: []string{"already-huge.mp4"},
	}
	head := &Snapshot{
		Entries: []Entry{{Path: "kept.txt", Hash: "h1", Size: 4}},
		// grows.bin crossed the cap; final.mp4 is brand new and oversized;
		// already-huge.mp4 was uncaptured on both sides.
		Skipped: []string{"grows.bin", "final.mp4", "already-huge.mp4"},
	}

	byPath := map[string]FileChange{}
	for _, c := range StatusBetween(base, head) {
		byPath[c.Path] = c
	}

	if got, ok := byPath["grows.bin"]; !ok {
		t.Error("grows.bin vanished from the range — it still exists, it just crossed the cap")
	} else if got.Status == "D" {
		t.Error("grows.bin reported as deleted — it grew past the capture cap, it was not removed")
	} else if !got.Uncaptured {
		t.Error("grows.bin must be flagged uncaptured; no content was stored, so no diff can be rendered")
	}

	if got, ok := byPath["final.mp4"]; !ok {
		t.Error("final.mp4 is absent — a reviewer would approve the gate without ever being " +
			"shown the run's largest product")
	} else if got.Status != "A" || !got.Uncaptured {
		t.Errorf("final.mp4 = %+v, want an uncaptured addition", got)
	}

	// Uncaptured on both sides: no evidence of change, and listing it at
	// every gate would be noise.
	if _, ok := byPath["already-huge.mp4"]; ok {
		t.Error("already-huge.mp4 was uncaptured on both sides — nothing says it changed")
	}
	if _, ok := byPath["kept.txt"]; ok {
		t.Error("kept.txt is identical on both sides and must not appear")
	}
}
