package model

import "testing"

func TestResolveSkillHints_NodePlusWorkflowUnion(t *testing.T) {
	e := &ClawExecutor{
		wfSkills: []string{"house-style"},
		skillHints: map[string]string{
			"changelog-writer": "Writes changelogs",
			"house-style":      "House writing style",
			"unused":           "not referenced",
		},
	}
	hints := e.resolveSkillHints([]string{"changelog-writer"})
	// Union of node ["changelog-writer"] + workflow ["house-style"], sorted,
	// filtered to resolved skills. "unused" is in the library but not referenced.
	if len(hints) != 2 {
		t.Fatalf("hints = %+v, want 2 (node ∪ workflow, referenced only)", hints)
	}
	if hints[0].Name != "changelog-writer" || hints[1].Name != "house-style" {
		t.Errorf("hints not sorted by name: %+v", hints)
	}
	if hints[0].Description != "Writes changelogs" {
		t.Errorf("description not resolved: %+v", hints[0])
	}
}

func TestResolveSkillHints_DropsUnresolved(t *testing.T) {
	e := &ClawExecutor{
		skillHints: map[string]string{"present": "here"},
	}
	// "absent" was referenced but never resolved in the library (not in the
	// hint map) → dropped (the runtime mirror already warned).
	hints := e.resolveSkillHints([]string{"present", "absent"})
	if len(hints) != 1 || hints[0].Name != "present" {
		t.Fatalf("hints = %+v, want only [present]", hints)
	}
}

func TestResolveSkillHints_NoHintsNil(t *testing.T) {
	e := &ClawExecutor{}
	if got := e.resolveSkillHints([]string{"x"}); got != nil {
		t.Errorf("hints = %+v, want nil when no skill hints set", got)
	}
}
