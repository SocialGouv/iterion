package claudesdk

import "testing"

// The delegate captures this same id from this same source under the same
// rule. Two readings of one fact with different rules is a trap for whoever
// wires the second one — this accessor has no caller today, which is
// exactly how such a trap survives.
func TestNoteSessionIDPrefersInitAndKeepsTheFirst(t *testing.T) {
	t.Run("nothing announced, nothing recorded", func(t *testing.T) {
		var s Session
		s.noteSessionID("", false)
		s.noteSessionID("", true)
		if got := s.SessionID(); got != "" {
			t.Fatalf("SessionID() = %q, want empty", got)
		}
	})

	t.Run("a non-init id is provisional until init speaks", func(t *testing.T) {
		var s Session
		s.noteSessionID("s-hook", false)
		if got := s.SessionID(); got != "s-hook" {
			t.Fatalf("SessionID() = %q, want s-hook — a stream that broke before init still names its session", got)
		}
		s.noteSessionID("s-init", true)
		if got := s.SessionID(); got != "s-init" {
			t.Fatalf("SessionID() = %q, want s-init — init is the CLI announcing THIS session; an event that arrived first must not pin its own", got)
		}
	})

	t.Run("later ids never displace the announced session", func(t *testing.T) {
		var s Session
		for _, m := range []struct {
			id       string
			fromInit bool
		}{{"s-init", true}, {"s-sub", false}, {"", true}, {"s-second-init", true}} {
			s.noteSessionID(m.id, m.fromInit)
		}
		if got := s.SessionID(); got != "s-init" {
			t.Fatalf("SessionID() = %q, want s-init — neither a sub-agent nor a second init displaces the session this object opened", got)
		}
	})
}
