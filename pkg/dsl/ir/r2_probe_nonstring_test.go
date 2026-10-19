package ir

import "testing"

// The override layer's comment used to claim the name stays absent "where it
// is not (an engine-supplied name, a non-string)". resolveVarText returns a
// non-string raw TYPED and without error — so a plain non-string override is
// PUT, not absent. This witness pins that: the comment (and any future code
// change matching it) must not model a non-string as unreadable.
//
// Mutation witness: making read() skip a non-string raw (the behavior the
// wrong comment described) reds this test on its PRESENT assertion.
func TestR2Probe_NonStringOverrideIsPut(t *testing.T) {
	w := &Workflow{Vars: map[string]*Var{
		"cfg": {Name: "cfg", Type: VarJSON, HasDefault: true, Default: `{"backend":"claw"}`},
	}}
	doc := map[string]any{"backend": "openai_compatible/gw-1"}
	view, late := LaunchVarsView[any](w, map[string]any{"cfg": doc})
	got, present := view["cfg"]
	if !present {
		t.Fatal("a plain non-string (json document) override was left ABSENT — contradicts the executed reading (put typed when ResolveVarText reads it)")
	}
	if _, ok := got.(map[string]any); !ok {
		t.Fatalf("view[cfg] = %T, want the typed document", got)
	}
	if late["cfg"] {
		t.Error("late[cfg] = true for a literal document")
	}
}
