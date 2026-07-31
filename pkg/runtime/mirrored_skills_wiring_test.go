package runtime

import (
	"slices"
	"testing"
)

// recordingSkillExecutor captures what the engine hands down. It implements
// only the optional setter, which is how the engine discovers the capability.
type recordingSkillExecutor struct {
	NodeExecutor
	calls [][]string
}

func (r *recordingSkillExecutor) SetMirroredSkills(paths []string) {
	r.calls = append(r.calls, paths)
}

// The engine half of the ownership channel. The executor half is covered in
// pkg/backend/model; between them the chain is engine → executor → Task → argv
// with no implicit link left.
func TestApplyMirroredSkillsReachesTheExecutor(t *testing.T) {
	rec := &recordingSkillExecutor{}
	e := &Engine{executor: rec}

	owned := []string{"/w/.claude/skills/a", "/w/.claude/skills/b.md"}
	e.applyMirroredSkills(owned)
	if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], owned) {
		t.Fatalf("executor received %v, want one call with %v", rec.calls, owned)
	}

	// An empty list is PUSHED, not skipped. Returning early would leave a
	// reused executor advertising the previous run's paths — which name a
	// different workspace entirely.
	e.applyMirroredSkills(nil)
	if len(rec.calls) != 2 {
		t.Fatalf("empty list not pushed: calls = %v", rec.calls)
	}
	if len(rec.calls[1]) != 0 {
		t.Errorf("second call = %v, want it to clear", rec.calls[1])
	}
}

// An executor that does not implement the setter is left alone rather than
// panicking — the engine drives several, and only some consume this.
func TestApplyMirroredSkillsIgnoresExecutorsWithoutTheSetter(t *testing.T) {
	e := &Engine{executor: nil}
	e.applyMirroredSkills([]string{"/w/.claude/skills/a"}) // must not panic
}
