package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRefusalFor_aLineageNamesEveryConsentItStillNeeds: a child whose
// parent's sandbox was copy-based and kept its scratch in the container needs
// both consents. Whichever one is missing, the refusal names it — and names
// the one given as still needed, so an operator following it does not trade
// one refusal for the other. Both given, nothing is refused.
func TestRefusalFor_aLineageNamesEveryConsentItStillNeeds(t *testing.T) {
	l := sharedLineage{copyBased: true, scratchContainerLocal: true}
	r := &store.Run{ID: "child", ParentRunID: "parent"}
	for _, tc := range []struct {
		name          string
		force, accept bool
		code          ErrorCode
		hintHas       []string
	}{
		{"neither", false, false, ErrCodeScratchNotPortable, []string{"--accept-scratch-loss", "--force"}},
		{"--force alone", true, false, ErrCodeScratchNotPortable, []string{"--accept-scratch-loss", "keep --force"}},
		{"the consent alone", false, true, ErrCodeResumeInvalid, []string{"--force", "keep --accept-scratch-loss"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := l.refusalFor(r, tc.force, tc.accept)
			var rt *RuntimeError
			if !errors.As(err, &rt) || rt.Code != tc.code {
				t.Fatalf("refusalFor(force=%v, accept=%v) = %v, want %s", tc.force, tc.accept, err, tc.code)
			}
			said := rt.Hint + " " + err.Error()
			for _, want := range tc.hintHas {
				if !strings.Contains(said, want) {
					t.Fatalf("refusalFor(force=%v, accept=%v) does not name %q: %v — hint: %s", tc.force, tc.accept, want, err, rt.Hint)
				}
			}
		})
	}
	if err := l.refusalFor(r, true, true); err != nil {
		t.Fatalf("both consents given: %v, want no refusal", err)
	}
}

// TestAlsoNamingSourceChange_aGivenForceIsKept: a lineage refusal that also
// names a changed source tells the operator who already gave --force to keep
// it — never to add a flag already given — and tells the one who did not to
// add it.
func TestAlsoNamingSourceChange_aGivenForceIsKept(t *testing.T) {
	r := &store.Run{ID: "child", ParentRunID: "parent", WorkflowHash: "sha256:launch"}
	for _, forced := range []bool{true, false} {
		e := &Engine{workflowHash: "sha256:edited", forceResume: forced}
		err := e.alsoNamingSourceChange(r, sharedLineage{copyBased: true, scratchContainerLocal: true}.refusalFor(r, forced, false))
		var rt *RuntimeError
		if !errors.As(err, &rt) || !strings.Contains(rt.Message, "the workflow source has also changed") {
			t.Fatalf("forced=%v: %v, want a refusal naming the source change", forced, err)
		}
		keep, add := strings.Contains(rt.Hint, "keep --force"), strings.Contains(rt.Hint, "add --force")
		if forced && (!keep || add) || !forced && !add {
			t.Fatalf("forced=%v: hint %q", forced, rt.Hint)
		}
		// A resume given --force does not need it besides: the surfaces say
		// "also needs --force" only to one that was not.
		if RemedyOf(err).AlsoNeedsForce == forced {
			t.Fatalf("forced=%v: also_needs_force=%v, want %v", forced, RemedyOf(err).AlsoNeedsForce, !forced)
		}
	}
}
