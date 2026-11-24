package scripthost

import (
	"strings"
	"testing"
)

// The bounds arrive CLAMPED — Host.Run and RunChild clamp before use — so
// resultFrameCap and clipToolText carry no zero fallback: a caller that skips
// the clamp gets a LOUD refusal at the first oversized frame (the bound named),
// never a silent default it did not choose.
//
// Measured before this test existed, as the ticket #3316 described: the two
// functions disagreed — resultFrameCap(0) silently substituted the default
// while clipToolText(s, 0) returned the caller's whole string UNBOUNDED.
// The ticket's point was that picking one direction was the work.
func TestAZeroBoundIsLoudAndBoundedNeverSilentlyDefaulted(t *testing.T) {
	t.Parallel()

	// resultFrameCap: zero in, a SMALL cap out — the first frame is refused
	// with the number named, which is loud, rather than silently upgraded to
	// the platform default.
	if got := resultFrameCap(Limits{}); got != int64(1<<10) {
		t.Errorf("resultFrameCap(zero limits) = %d, want the envelope-only 1024 — a loud small cap, not a silent default", got)
	}

	// clipToolText: zero in, EMPTY and flagged out — bounded, never the
	// caller's whole string.
	clipped, flag := clipToolText(strings.Repeat("s", 4096), 0)
	if clipped != "" || !flag {
		t.Errorf("clipToolText(s, 0) = (%d bytes, %v), want (\"\", true)", len(clipped), flag)
	}

	// And the normal arm stays exact: under the bound, unchanged.
	kept, flag := clipToolText("abc", 10)
	if kept != "abc" || flag {
		t.Errorf("clipToolText(abc, 10) = (%q, %v), want unchanged and unflagged", kept, flag)
	}
}
