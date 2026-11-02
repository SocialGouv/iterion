package runtime

import (
	"strings"
	"testing"
)

// The launch guard refuses every shape whose landing intent can never fire:
// a child whose own workflow declares `worktree: none` (no worktree of its
// own, no finalization of its own), and a child on a deployment whose run
// store has no filesystem root — the cloud plane runs every child in place
// and no bank ever writes a subbot child's branch, so FinalBranch stays
// empty and the instance's outcome router declines the intent in silence on
// every sweep. Every other shape passes — "auto" over a rooted store may
// finalize, and an intent the node never armed is C330's compile-time
// refusal, not a launch error.
func TestRefuseUnlandableSubbotLanding(t *testing.T) {
	tests := []struct {
		name       string
		req        SubbotRequest
		childMode  string
		storeRoot  string
		wantRefuse bool
	}{
		{"intent on a worktree:none child", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "none", "/store", true},
		{"no intent on a worktree:none child", SubbotRequest{NodeID: "lot"}, "none", "/store", false},
		{"intent on the auto default", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "auto", "/store", false},
		{"intent on an unset mode", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "", "/store", false},
		{"intent on the cloud plane (no store root)", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "auto", "", true},
		{"intent on an unset mode over the cloud plane", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "", "", true},
		{"no intent on the cloud plane", SubbotRequest{NodeID: "lot"}, "auto", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RefuseUnlandableSubbotLanding(tc.req, tc.childMode, tc.storeRoot)
			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("RefuseUnlandableSubbotLanding() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RefuseUnlandableSubbotLanding() = nil, want a refusal: the intent would sit on the run doc unfired forever")
			}
			if !strings.Contains(err.Error(), "auto_merge") {
				t.Errorf("refusal %q does not name %q — the author cannot act on it", err, "auto_merge")
			}
			if tc.childMode == "none" && !strings.Contains(err.Error(), "worktree: none") {
				t.Errorf("refusal %q does not name %q — the author cannot act on it", err, "worktree: none")
			}
			if tc.storeRoot == "" && !strings.Contains(err.Error(), "outcome router") {
				t.Errorf("refusal %q does not name the silent decline (%q missing) — the author cannot judge the shape", err, "outcome router")
			}
		})
	}
}
