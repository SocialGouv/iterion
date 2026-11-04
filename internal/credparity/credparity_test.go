package credparity

import (
	"slices"
	"strings"
	"testing"
)

// The table is well-formed: every row is named once, says where it comes
// from, walks a chain its Want can index, and sits where a run of its trust
// can — a fork-lane run is always sandboxed (the PR2b floor).
func TestEveryRowIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Rows() {
		if r.Name == "" || r.Origin == "" {
			t.Errorf("row %q: a row names itself and its origin", r.Name)
		}
		if seen[r.Name] {
			t.Errorf("row %q appears twice", r.Name)
		}
		seen[r.Name] = true
		if len(r.Node.Legs) == 0 {
			t.Errorf("row %q: the node walks no leg", r.Name)
		}
		if !r.Want.Refused() && (r.Want.Leg < 0 || r.Want.Leg >= len(r.Node.Legs)) {
			t.Errorf("row %q: Want spends leg %d of a %d-leg chain", r.Name, r.Want.Leg, len(r.Node.Legs))
		}
		if r.Trust == Fork && !r.Sandboxed {
			t.Errorf("row %q: a fork-lane run is never unsandboxed", r.Name)
		}
		if r.Trust == "" || r.Role == "" {
			t.Errorf("row %q: trust %q role %q — both are stated", r.Name, r.Trust, r.Role)
		}
	}
}

// A Pending entry names the unit of this lane still to land (U2: U1 has
// landed and closed its rows, and no red row lands with U0–U2), one reader at
// most once, and an answer that differs from Want — an entry restating Want
// would pin no divergence and never flip.
func TestEveryPendingEntryPinsADivergenceTheLaneCloses(t *testing.T) {
	for _, r := range Rows() {
		readers := map[Reader]bool{}
		for _, p := range r.Pending {
			if p.Unit != "U2" {
				t.Errorf("row %q: %s pending on %q — U1 has landed and U0–U2 land as one tip, so only U2 greens a row", r.Name, p.Reader, p.Unit)
			}
			if readers[p.Reader] {
				t.Errorf("row %q: %s pinned twice", r.Name, p.Reader)
			}
			readers[p.Reader] = true
			if !slices.Contains(Readers, p.Reader) {
				t.Errorf("row %q: unknown reader %q", r.Name, p.Reader)
			}
			if p.Today == r.Want {
				t.Errorf("row %q: %s's pinned answer is Want itself", r.Name, p.Reader)
			}
			if p.Why == "" {
				t.Errorf("row %q: %s's divergence is unexplained", r.Name, p.Reader)
			}
		}
	}
}

// The rows ADR-161 (decision 9, U0) says are seen red on main are in the
// table: each one U2 greens still pins its reader on U2, and each one U1
// greened pins its reader no more — the harness judges it against Want.
func TestTheSeenRedRowsArePinned(t *testing.T) {
	for _, want := range []struct {
		origin string
		reader Reader
		unit   string // "" — greened by U1, judged against Want
	}{
		{"t50 r2 #1", Meter, "U2"},
		{"t50 r2 #1", Admission, ""},
		{"t50 r2 #2", Admission, ""},
		{"plan ≠ dispatch", Plan, "U2"},
		{"t53 r2 #2", Meter, "U2"},
		{"t53 r2 #2", Admission, ""},
		{"t53 r2 #4", Admission, ""},
		{"t53 r2 #3", Admission, ""},
		{"t53 r3 #2", Admission, ""},
	} {
		found := false
		for _, r := range Rows() {
			if !strings.Contains(r.Origin, want.origin) {
				continue
			}
			_, p := r.Expect(want.reader)
			switch {
			case want.unit == "" && p != nil:
				t.Errorf("row %q from %q still pins %s on %s — U1 greened it", r.Name, want.origin, want.reader, p.Unit)
			case want.unit == "" && (want.reader != Admission || r.Judged()):
				found = true
			case p != nil && p.Unit == want.unit:
				found = true
			}
		}
		if !found {
			t.Errorf("no row from %q judges %s (unit %q)", want.origin, want.reader, want.unit)
		}
	}
}
