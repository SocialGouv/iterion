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
