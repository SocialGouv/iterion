package runview

import (
	"go/parser"
	"go/token"
	"testing"
)

// The sweep is a gate, so its predicate owes proof in BOTH directions: a site
// it stops seeing reads as coverage while covering nothing, and a correct site
// it accuses costs more than a miss because nobody goes looking for a false
// accusation in a green-by-default guard.
//
// The predicate is exercised on source text here rather than on the tree,
// because the cases that matter are the ones the tree does not contain yet.
func TestTheExecutorSpecSweepSeesWhatItClaimsTo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		missing bool
		unread  bool // the predicate gave up on this shape
	}{
		{
			name: "a literal that sets the field",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() { _ = runview.ExecutorSpec{BotID: "b"} }`,
		},
		{
			name: "a literal that omits it",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() { _ = runview.ExecutorSpec{} }`,
			missing: true,
		},
		{
			// An import ALIAS is nobody's error, and a sweep keyed on the
			// package's default name stops seeing every site behind one.
			name: "an aliased import that omits it",
			src: `package p
import rv "github.com/SocialGouv/iterion/pkg/runview"
func f() { _ = rv.ExecutorSpec{} }`,
			missing: true,
		},
		{
			name: "an aliased import that sets it",
			src: `package p
import rv "github.com/SocialGouv/iterion/pkg/runview"
func f() { _ = rv.ExecutorSpec{BotID: "b"} }`,
		},
		{
			// A neighbour type must not be mistaken for it, whatever the
			// package is called.
			name: "a different type of the same package",
			src: `package p
import rv "github.com/SocialGouv/iterion/pkg/runview"
func f() { _ = rv.OtherSpec{} }`,
		},
		{
			// A selector on an identifier that is NOT the runview import.
			name: "a same-named type from another package",
			src: `package p
import other "example.com/other"
func f() { _ = other.ExecutorSpec{} }`,
		},
		{
			// A file that declares its OWN type of that name and never
			// imports runview. Accusing it names a remedy that does not
			// type-check there, and the only way out is an exempt entry
			// about a file the rule has nothing to say about.
			name: "a package-local type of the same name",
			src: `package p
type ExecutorSpec struct{ Name string }
func f() ExecutorSpec { return ExecutorSpec{Name: "x"} }`,
		},
		{
			// …but a dot-import DOES entitle the bare spelling.
			name: "a dot-import that omits the field",
			src: `package p
import . "github.com/SocialGouv/iterion/pkg/runview"
func f() { _ = ExecutorSpec{} }`,
			missing: true,
		},
		{
			name: "field-by-field assignment",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() {
	var s runview.ExecutorSpec
	s.BotID = "b"
	_, _ = runview.BuildExecutor(s)
}`,
		},
		{
			name: "field-by-field, field never assigned",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() {
	var s runview.ExecutorSpec
	_, _ = runview.BuildExecutor(s)
}`,
			missing: true,
		},
		{
			// A factory that fills the spec and RETURNS it: the caller is
			// the one that builds. Covered today only because the real site
			// happens to use a composite literal.
			// A factory that FILLS a spec and returns it is not the site
			// that decides — its caller is. Accusing it failed a
			// legitimate shared-base helper; reporting it as unjudged
			// points at the right place.
			name: "a factory that fills a spec and returns it",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func mk() runview.ExecutorSpec {
	var s runview.ExecutorSpec
	s.RunID = "r"
	return s
}`,
			unread: true,
		},
		{
			// The shape that must NOT be accused: both fields set in plain
			// sight, and the address also handed to a helper for something
			// else. The give-up arm used to win over the assignment.
			name: "both fields set, and the address handed to a helper",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func tune(s *runview.ExecutorSpec) { s.RunID = "r" }
func f() {
	var s runview.ExecutorSpec
	s.BotID = "b"
	s.SandboxTiersKnown = true
	tune(&s)
	_, _ = runview.BuildExecutor(s)
}`,
		},
		{
			// …and the same shape with the field set is clean.
			name: "a factory that sets the field",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func mk() runview.ExecutorSpec {
	var s runview.ExecutorSpec
	s.RunID = "r"
	s.BotID = "b"
	return s
}`,
		},
		{
			// A zero-value sentinel returned beside an error builds nothing,
			// so it owes nothing — and must not be reported as unjudgeable
			// either, which would be noise about a variable nobody uses.
			name: "a zero-value sentinel returned with an error",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func mk(fail bool) (runview.ExecutorSpec, error) {
	var zero runview.ExecutorSpec
	if fail {
		return zero, nil
	}
	s := runview.ExecutorSpec{BotID: "b"}
	return s, nil
}`,
		},
		{
			// `BuildExecutor(*spec)` after `new(...)`: the shape a refactor
			// to a pointer produces, invisible while only bare idents were
			// collected.
			name: "new() handed by dereference",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() {
	spec := new(runview.ExecutorSpec)
	spec.RunID = "r"
	_, _ = runview.BuildExecutor(*spec)
}`,
			missing: true,
		},
		{
			// The give-up the predicate's own comment promises: the callee
			// may be exactly where the field is set, so this correct code
			// must not be accused — but it IS reported as unjudged.
			unread: true,
			name:   "a spec whose address is handed to a helper",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func fill(*runview.ExecutorSpec) {}
func f() {
	var s runview.ExecutorSpec
	fill(&s)
	_, _ = runview.BuildExecutor(s)
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, unread := executorSpecMissesField(file, "BotID")
			if unread != tc.unread {
				t.Errorf("unread = %v, want %v — the shapes this predicate gives up on must be REPORTED, "+
					"or a site drifting into one leaves the sweep covering nothing and saying nothing",
					unread, tc.unread)
			}
			if got != tc.missing {
				verb := "accused correct code"
				if tc.missing {
					verb = "did not see a site that omits the field"
				}
				t.Errorf("executorSpecMissesField = %v, want %v — the sweep %s", got, tc.missing, verb)
			}
		})
	}
}

// The elided walk marks a container's DIRECT elements as specs. Descending
// past them reaches a spec's own field values — and ExecutorSpec has
// value-struct-slice fields, so a perfectly ordinary literal was judged on
// its `RunFallback` element and accused of not setting BotID two lines up.
// Two independent review passes found this shape; it is the same class as
// the give-up-over-assignment accusation, one round later.
func TestTheElidedWalkStopsAtTheSpecsItself(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		missing bool
	}{
		{
			// The false accusation: correct code with a nested literal in
			// one of the spec's own fields.
			name: "an elided container whose spec holds a nested literal",
			src: `package p
import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)
func f() []runview.ExecutorSpec {
	return []runview.ExecutorSpec{{
		BotID:       "b",
		RunFallback: []ir.Fallback{{Backend: "claude_code"}},
	}}
}`,
		},
		{
			// …and the detection it must not cost: the same shape with the
			// field missing is still caught.
			name: "the same shape with the field missing",
			src: `package p
import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)
func f() []runview.ExecutorSpec {
	return []runview.ExecutorSpec{{
		RunID:       "r",
		RunFallback: []ir.Fallback{{Backend: "claude_code"}},
	}}
}`,
			missing: true,
		},
		{
			// Two container levels, field set: judged, not accused.
			name: "two container levels, field set",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() [][]runview.ExecutorSpec {
	return [][]runview.ExecutorSpec{{{BotID: "b"}}}
}`,
		},
		{
			name: "two container levels, field missing",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() [][]runview.ExecutorSpec {
	return [][]runview.ExecutorSpec{{{RunID: "r"}}}
}`,
			missing: true,
		},
		{
			name: "a map of slices of specs, field set",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f() map[string][]runview.ExecutorSpec {
	return map[string][]runview.ExecutorSpec{"a": {{BotID: "b"}}}
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, _ := executorSpecMissesField(file, "BotID")
			if got != tc.missing {
				verb := "accused correct code"
				if tc.missing {
					verb = "did not see a site that omits the field"
				}
				t.Errorf("missing = %v, want %v — the sweep %s", got, tc.missing, verb)
			}
		})
	}
}
