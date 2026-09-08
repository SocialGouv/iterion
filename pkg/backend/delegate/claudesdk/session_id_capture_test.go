package claudesdk

import "testing"

// The delegate captures this same id from this same source with a
// first-non-empty rule. Two readings of one fact with opposite rules is a
// trap for whoever wires the second one — this accessor has no caller
// today, which is exactly how such a trap survives.
func TestNoteSessionIDKeepsTheFirstNonEmpty(t *testing.T) {
	var s Session
	for _, id := range []string{"", "s-first", "", "s-second"} {
		s.noteSessionID(id)
	}
	if got := s.SessionID(); got != "s-first" {
		t.Fatalf("SessionID() = %q, want s-first — a later id must not displace the session this object opened", got)
	}

	var empty Session
	empty.noteSessionID("")
	if got := empty.SessionID(); got != "" {
		t.Fatalf("SessionID() = %q, want empty — nothing announced, nothing recorded", got)
	}
}
