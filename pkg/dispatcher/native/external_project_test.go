package native

import (
	"testing"
	"time"
)

// ExternalProject.Equal is what a periodic writer checks before writing at
// all: a card rewritten when nothing changed emits an EvtIssueUpdated the
// trigger spine consumes as `card.updated`. So a field it forgets to compare
// is a spurious card event per card per tick, and a field it compares WRONGLY
// is a real change that never gets written.
func TestExternalProjectEqual(t *testing.T) {
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	base := ExternalProject{
		Owner: "SocialGouv", Number: 203, ItemID: "PVTI_1",
		Status: "Planned", StatusAt: at, StateAt: at,
	}
	if !base.Equal(base) {
		t.Fatal("an identical value must compare equal")
	}

	// Every field must participate: one left out is a change never written.
	for name, mutate := range map[string]func(*ExternalProject){
		"owner":     func(p *ExternalProject) { p.Owner = "Other" },
		"number":    func(p *ExternalProject) { p.Number = 204 },
		"item_id":   func(p *ExternalProject) { p.ItemID = "PVTI_2" },
		"status":    func(p *ExternalProject) { p.Status = "Done" },
		"status_at": func(p *ExternalProject) { p.StatusAt = at.Add(time.Second) },
		"state_at":  func(p *ExternalProject) { p.StateAt = at.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			mutate(&other)
			if base.Equal(other) {
				t.Errorf("%s is not compared — a change to it would never be written", name)
			}
		})
	}
}

// TestExternalProjectEqualComparesInstantsNotRepresentations pins why the
// timestamps use Equal and not ==: a time.Time carries a monotonic reading and
// a location, so two values denoting the same instant are routinely unequal
// under ==. Under == the pass would rewrite every card on every tick — the
// exact churn this guard exists to stop.
func TestExternalProjectEqualComparesInstantsNotRepresentations(t *testing.T) {
	utc := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// Same instant, different Location pointer — so the two structs are
	// distinguishable under ==, which is what makes this a live regression
	// test for the comparison operator Equal must not degrade into.
	paris := utc.In(time.FixedZone("CEST", 2*60*60))
	a := ExternalProject{Status: "Planned", StatusAt: utc, StateAt: utc}
	b := ExternalProject{Status: "Planned", StatusAt: paris, StateAt: paris}
	if !a.Equal(b) {
		t.Error("the same instant in another location must compare equal")
	}
}

// A nil receiver is "nothing recorded", which is never equal to a value the
// caller is about to record — otherwise the FIRST sync would never persist.
func TestExternalProjectEqualNilIsNeverEqual(t *testing.T) {
	var p *ExternalProject
	if p.Equal(ExternalProject{}) {
		t.Error("nil must not compare equal, or the first write is skipped")
	}
}
