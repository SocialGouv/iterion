package runtime

import (
	"strings"
	"testing"
)

// The launch guard refuses exactly one shape: the landing intent armed on a
// child whose own workflow declares `worktree: none`. Every other worktree
// mode passes — "auto" may finalize (and the engine's own degrade paths stay
// the engine's judgment), and an intent the node never armed is C330's
// compile-time refusal, not a launch error.
func TestRefuseUnlandableSubbotLanding(t *testing.T) {
	tests := []struct {
		name       string
		req        SubbotRequest
		childMode  string
		wantRefuse bool
	}{
		{"intent on a worktree:none child", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "none", true},
		{"no intent on a worktree:none child", SubbotRequest{NodeID: "lot"}, "none", false},
		{"intent on the auto default", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "auto", false},
		{"intent on an unset mode", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RefuseUnlandableSubbotLanding(tc.req, tc.childMode)
			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("RefuseUnlandableSubbotLanding() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RefuseUnlandableSubbotLanding() = nil, want a refusal: the intent would sit on the run doc unfired forever")
			}
			for _, want := range []string{"worktree: none", "auto_merge"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q — the author cannot act on it", err, want)
				}
			}
		})
	}
}
