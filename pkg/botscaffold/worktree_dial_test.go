package botscaffold

import "testing"

// TestSingleAgentTemplateWritesItsWorktreeChoice: the single-agent
// template (every blank gallery entry) writes its worktree choice on BOTH
// branches of the builder's dial, like the gallery partial
// (TestGalleryShapesResolveTheWorktreeDialOff holds the shapes): an unset
// `worktree:` resolves to `auto`, and an isolated run's uncommitted edits
// are wip-banked on the storage branch, never merged into the checkout —
// so "not isolated" has to be written as `none`.
func TestSingleAgentTemplateWritesItsWorktreeChoice(t *testing.T) {
	for _, c := range []struct {
		dial bool
		want string
	}{{false, "none"}, {true, "auto"}} {
		spec := Spec{Slug: "single", Instructions: "Do the thing.", Worktree: c.dial}
		if _, w, _ := scaffoldAndCompile(t, spec); w.Worktree != c.want {
			t.Errorf("single-agent template, dial %v: worktree = %q, want %q", c.dial, w.Worktree, c.want)
		}
	}
}
