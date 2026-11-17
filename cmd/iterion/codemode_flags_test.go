package main

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The CLI half of the run-level codemode override (#3345): the flags project
// onto the resolver's rung type, and an untouched set projects NIL — a launch
// that expressed nothing must not travel as a declaration.
func TestCodemodeFlagsProjectBoundsOrNothing(t *testing.T) {
	t.Parallel()
	var untouched codemodeFlagSet
	if b := untouched.bounds(); b != nil {
		t.Errorf("an untouched flag set projected %+v, want nil — the launch would read as having chosen", b)
	}

	c := codemodeFlagSet{toolCalls: 256, wall: 90 * time.Second}
	b := c.bounds()
	if b == nil {
		t.Fatal("a flag set with two bounds projected nil")
	}
	if b.ToolCalls != 256 || b.Wall != 90*time.Second {
		t.Errorf("bounds = %+v, want ToolCalls 256 / Wall 90s", b)
	}
	// The fields the flags left at zero stay zero: the resolver's field-by-
	// field rule reads them as "did not choose" against the authored rungs.
	if b.Memory != 0 || b.CPU != 0 || b.ToolResultBytes != 0 {
		t.Errorf("bounds = %+v, want the unset flags to stay zero", b)
	}
	if want := (&ir.CodemodeBounds{ToolCalls: 256, Wall: 90 * time.Second}); *b != *want {
		t.Errorf("bounds = %+v, want %+v", b, want)
	}
}
