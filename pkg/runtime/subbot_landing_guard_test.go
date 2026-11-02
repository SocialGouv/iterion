package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// landingGuardRun is a non-nil sandbox.Run that is never called: the guard
// only reads ParentSandbox.Run's presence (the parent's sandbox started),
// never its behaviour.
type landingGuardRun struct{ sandbox.Run }

// The launch guard refuses every shape whose landing intent can never fire:
// a child whose worktree mode is anything but the canonical `auto` — the
// declared `worktree: none`, an unset or foreign value (the engine builds a
// child worktree for `auto` and nothing else) — a child on a deployment
// whose run store has no filesystem root — the cloud plane runs every child
// in place and no bank ever writes a subbot child's branch, so FinalBranch
// stays empty and the instance's outcome router declines the intent in
// silence on every sweep — and a child that adopts its parent's live
// sandbox, which mounts ONE workspace whatever the child declares
// (runsInParentWorkspace), so no worktree mode gives the child a
// finalization of its own. Every other shape passes — "auto" over a rooted
// store may finalize, and an intent the node never armed is C330's
// compile-time refusal, not a launch error.
func TestRefuseUnlandableSubbotLanding(t *testing.T) {
	sharedSandbox := &SharedSandbox{Run: landingGuardRun{}}
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
		{"intent on an unset mode", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "", "/store", true},
		{"intent on a foreign mode", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "Auto", "/store", true},
		{"no intent on an unset mode", SubbotRequest{NodeID: "lot"}, "", "/store", false},
		{"intent on the cloud plane (no store root)", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "auto", "", true},
		{"intent on an unset mode over the cloud plane", SubbotRequest{AutoMerge: true, NodeID: "lot"}, "", "", true},
		{"no intent on the cloud plane", SubbotRequest{NodeID: "lot"}, "auto", "", false},
		// The adopted parent sandbox dominates: it overrides `isolated:` and
		// any worktree mode, so the intent is dead whatever the child
		// declares, over a rooted store as over the cloud plane.
		{"intent under an adopted parent sandbox", SubbotRequest{AutoMerge: true, NodeID: "lot", ParentSandbox: sharedSandbox}, "auto", "/store", true},
		{"intent under an adopted sandbox on a worktree:none child", SubbotRequest{AutoMerge: true, NodeID: "lot", ParentSandbox: sharedSandbox}, "none", "/store", true},
		{"intent under an adopted sandbox over the cloud plane", SubbotRequest{AutoMerge: true, NodeID: "lot", ParentSandbox: sharedSandbox}, "auto", "", true},
		{"no intent under an adopted parent sandbox", SubbotRequest{NodeID: "lot", ParentSandbox: sharedSandbox}, "auto", "/store", false},
		// A carried-but-unstarted sandbox (nil Run — WithSharedSandbox
		// ignores it, the child starts none) leaves the worktree decision
		// to the mode.
		{"intent under an unstarted parent sandbox", SubbotRequest{AutoMerge: true, NodeID: "lot", ParentSandbox: &SharedSandbox{}}, "auto", "/store", false},
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
			if tc.childMode == "none" && tc.req.ParentSandbox == nil && !strings.Contains(err.Error(), "worktree: none") {
				t.Errorf("refusal %q does not name %q — the author cannot act on it", err, "worktree: none")
			}
			if tc.storeRoot == "" && tc.req.ParentSandbox == nil && !strings.Contains(err.Error(), "outcome router") {
				t.Errorf("refusal %q does not name the silent decline (%q missing) — the author cannot judge the shape", err, "outcome router")
			}
			if tc.req.ParentSandbox != nil && tc.req.ParentSandbox.Run != nil && !strings.Contains(err.Error(), "sandbox") {
				t.Errorf("refusal %q does not name the shared sandbox — the author cannot act on it", err)
			}
		})
	}
}
