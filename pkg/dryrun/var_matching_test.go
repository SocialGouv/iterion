package dryrun

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestTheDryRunDoesNotInventAValueItsOwnPatternRefuses.
//
// launchInputs seeds a shape for every var the launch left out, and the
// engine's launch gate then JUDGES those seeds — so a shape the declared
// pattern refuses kills the dry run at node 0 over a value the operator
// never typed. `iterion validate --exec` is the surface #1350 sells; it
// must not be the surface #1350 breaks.
//
// The enum branch of VarValue exists for exactly this reason (it picks a
// declared value rather than "x"); this is the same courtesy for a pattern.
//
// Mutation that reddens it: delete the Matching branch from VarValue, so a
// pattern-bearing var is seeded "x" again.
func TestTheDryRunDoesNotInventAValueItsOwnPatternRefuses(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		// seeded is what VarValue must produce; "" with absent=true means
		// the var is left out of launchInputs entirely.
		seeded string
		absent bool
	}{
		{name: "digits", pattern: `^[0-9]+$`, seeded: "0"},
		// The case that separates the ladder's ORDER from its membership:
		// `""` is admissible here too, and picking it would render an
		// empty string into a command where a number is meant.
		{name: "digits or empty", pattern: `^[0-9]*$`, seeded: "0"},
		{name: "lowercase", pattern: `^[a-z]+$`, seeded: "x"},
		{name: "empty allowed", pattern: `^$`, seeded: ""},
		{name: "the dogfood shape", pattern: `^[A-Za-z0-9._][A-Za-z0-9._-]*$|^$`, seeded: "x"},
		// No shape this package produces satisfies it: the var is left
		// unsupplied rather than seeded with something the gate refuses.
		{name: "no admissible shape", pattern: `^https://[a-z]+$`, absent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &ir.Var{Name: "port", Type: ir.VarString, Matching: tc.pattern}
			got := VarValue(v, true, false)
			if tc.absent {
				if got != nil {
					t.Fatalf("VarValue invented %#v for a pattern that admits none of its shapes", got)
				}
				return
			}
			if got != tc.seeded {
				t.Fatalf("VarValue = %#v, want %q", got, tc.seeded)
			}
			// The seed must satisfy the pattern the engine will check it
			// against — the assertion the whole test exists for.
			ok, err := ir.ValueMatchesPattern(tc.pattern, tc.seeded)
			if err != nil || !ok {
				t.Fatalf("the seeded value %q does not satisfy %q (ok=%v err=%v)", tc.seeded, tc.pattern, ok, err)
			}
		})
	}
}

// TestLaunchInputsOmitsAVarItCannotSatisfy: VarValue's nil has to reach
// the map as an ABSENCE, not as a nil entry — the gate judges every key
// present, and a nil under a declared var would be refused as "not a
// string".
func TestLaunchInputsOmitsAVarItCannotSatisfy(t *testing.T) {
	wf := &ir.Workflow{
		Name: "dryrun_matching",
		Vars: map[string]*ir.Var{
			"reachable":   {Name: "reachable", Type: ir.VarString, Matching: `^[a-z]+$`},
			"unreachable": {Name: "unreachable", Type: ir.VarString, Matching: `^https://[a-z]+$`},
			"plain":       {Name: "plain", Type: ir.VarString},
		},
	}
	inputs := launchInputs(wf, nil, true)

	if got, ok := inputs["reachable"]; !ok || got != "x" {
		t.Errorf("reachable = %#v (present=%v), want \"x\"", got, ok)
	}
	if got, ok := inputs["plain"]; !ok || got != "x" {
		t.Errorf("plain = %#v (present=%v), want \"x\"", got, ok)
	}
	if got, ok := inputs["unreachable"]; ok {
		t.Errorf("unreachable must be absent from launchInputs, got %#v", got)
	}
}

// TestTheDryRunDoesNotSeedAnEnumValueThePatternRefuses.
//
// A var may declare BOTH constraints, and they are enforced independently
// at launch — so the dry run's enum pick has to satisfy the pattern too.
// Without this, seeding `enum[0]` (or `enum[last]` on the false pass) kills
// the pass at node 0 over a value the operator never typed, on the one
// declaration shape docs/dsl.md blesses.
//
// Mutation that reddens it: return enumValue(...) instead of
// enumValueMatching(...) in VarValue.
func TestTheDryRunDoesNotSeedAnEnumValueThePatternRefuses(t *testing.T) {
	// "slow-and-careful" is a declared value that the pattern refuses; it
	// is what the false pass would otherwise pick.
	v := &ir.Var{
		Name:       "mode",
		Type:       ir.VarString,
		EnumValues: []string{"fast", "slow-and-careful"},
		Matching:   `^[a-z]+$`,
	}
	for _, bias := range []bool{true, false} {
		got := VarValue(v, bias, false)
		if got != "fast" {
			t.Errorf("bias=%v: VarValue = %#v, want the only enum value the pattern admits (%q)", bias, got, "fast")
		}
	}

	// No declared value is admissible: the var is left unsupplied rather
	// than seeded with one the launch gate will refuse.
	none := &ir.Var{
		Name:       "mode",
		Type:       ir.VarString,
		EnumValues: []string{"Fast-Mode", "slow-and-careful"},
		Matching:   `^[a-z]+$`,
	}
	for _, bias := range []bool{true, false} {
		if got := VarValue(none, bias, false); got != nil {
			t.Errorf("bias=%v: VarValue = %#v, want nil — no declared value satisfies the pattern", bias, got)
		}
	}

	// An unconstrained enum keeps its historical pick exactly.
	plain := &ir.Var{Name: "mode", Type: ir.VarString, EnumValues: []string{"a", "b"}}
	if got := VarValue(plain, true, false); got != "a" {
		t.Errorf("bias=true: VarValue = %#v, want \"a\"", got)
	}
	if got := VarValue(plain, false, false); got != "b" {
		t.Errorf("bias=false: VarValue = %#v, want \"b\"", got)
	}
}

// TestAnOmittedVarIsNotReportedAsUndeclared: an undeclared {{vars.X}} is a
// COMPILE error (C033), so it never reaches a dry run — which makes "no
// such var is declared" false every time the dry run could print it for a
// var the seeding left out. Mutation that reddens it: drop the "vars" arm
// from whyUnresolved.
func TestAnOmittedVarIsNotReportedAsUndeclared(t *testing.T) {
	wf := &ir.Workflow{
		Name: "dryrun_unresolved",
		Vars: map[string]*ir.Var{
			"endpoint": {Name: "endpoint", Type: ir.VarString, Matching: `^https://[a-z.]+$`},
		},
	}
	x := &Executor{wf: wf}
	got := x.whyUnresolved("vars.endpoint")
	if strings.Contains(got, "no such var is declared") {
		t.Errorf("a declared var the dry run could not seed is reported as undeclared: %q", got)
	}
	if !strings.Contains(got, "--var") {
		t.Errorf("the explanation must say how to proceed, got %q", got)
	}
	// A name the workflow really does not declare keeps the plain reading.
	if got := x.whyUnresolved("vars.nowhere"); !strings.Contains(got, "no such var is declared") {
		t.Errorf("an undeclared name must still read as undeclared, got %q", got)
	}

	// A declared var unresolved for ANY OTHER reason must not be told it
	// failed a pattern: a message that names a cause it never checked is
	// false the day a second cause appears.
	wf.Vars["plain"] = &ir.Var{Name: "plain", Type: ir.VarString, HasDefault: true, Default: "d"}
	got = x.whyUnresolved("vars.plain")
	if strings.Contains(got, "matching") {
		t.Errorf("a defaulted var with no pattern is blamed on a pattern: %q", got)
	}
	if strings.Contains(got, "no such var is declared") {
		t.Errorf("a declared var is reported as undeclared: %q", got)
	}
}
