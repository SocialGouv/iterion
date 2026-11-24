package credparity

import "testing"

// Check judges reader's answer got for the row, both sides read through
// project — the reader's own terms (a meter key, a slot, a verdict) of a
// Spend. With no Pending entry, got must be Want's projection. Pinned, got
// must be the pinned answer's projection and that projection must still
// differ from Want's: once the unit fixes the reader, the row turns red until
// the unit deletes the entry.
func Check[T comparable](t testing.TB, r Row, reader Reader, got T, project func(Spend) T) {
	t.Helper()
	want, p := r.Expect(reader)
	w := project(want)
	if p == nil {
		if got != w {
			t.Errorf("%s answers %v, want %v (row %q, from %s)", reader, got, w, r.Name, r.Origin)
		}
		return
	}
	today := project(p.Today)
	if today == w {
		t.Fatalf("%s's pin answers %v, Want's own reading: the entry pins nothing (row %q)", reader, today, r.Name)
	}
	if got == w {
		t.Errorf("%s now answers Want (%v): the divergence %s pinned on %s is gone — %s drops this Pending entry (row %q)", reader, got, p.Why, p.Unit, p.Unit, r.Name)
		return
	}
	if got != today {
		t.Errorf("%s answers %v, neither Want %v nor the pinned %v (%s, %s) — the divergence moved (row %q)", reader, got, w, today, p.Why, p.Unit, r.Name)
	}
}
