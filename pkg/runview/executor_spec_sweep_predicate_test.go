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
			// The give-up the predicate's own comment promises: the callee
			// may be exactly where the field is set, so this correct code
			// must not be accused.
			name: "a spec whose address is handed to a helper",
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
			if got := executorSpecMissesField(file, "BotID"); got != tc.missing {
				verb := "accused correct code"
				if tc.missing {
					verb = "did not see a site that omits the field"
				}
				t.Errorf("executorSpecMissesField = %v, want %v — the sweep %s", got, tc.missing, verb)
			}
		})
	}
}
