package ir

import "testing"

func skillSrc(nodeSkills string) string {
	return `
agent a:
  model: "test-model"
` + nodeSkills + `

workflow w:
  entry: a
  a -> done
`
}

func countC199(r *CompileResult) int {
	n := 0
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidSkillRef {
			n++
		}
	}
	return n
}

func TestSkillRef_WellFormedNoWarning(t *testing.T) {
	// A well-formed kebab name that does NOT exist in any library must NOT
	// warn — existence is a run-time concern, so compiles stay portable.
	r := compileFile(t, skillSrc(`  skills: ["changelog-writer", "does-not-exist"]`))
	if got := countC199(r); got != 0 {
		t.Errorf("C199 count = %d, want 0 for well-formed names (%v)", got, r.Diagnostics)
	}
}

func TestSkillRef_MalformedWarns(t *testing.T) {
	// A name with a path separator is malformed → one C199 warning.
	r := compileFile(t, skillSrc(`  skills: ["../etc/passwd"]`))
	if got := countC199(r); got != 1 {
		t.Errorf("C199 count = %d, want 1 for malformed name (%v)", got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidSkillRef && d.Severity != SeverityWarning {
			t.Errorf("C199 severity = %v, want warning", d.Severity)
		}
	}
}

func TestSkillRef_WorkflowLevel(t *testing.T) {
	src := `
agent a:
  model: "test-model"

workflow w:
  entry: a
  skills: ["a/b"]
  a -> done
`
	r := compileFile(t, src)
	if got := countC199(r); got != 1 {
		t.Errorf("C199 count = %d, want 1 for malformed workflow-level skill (%v)", got, r.Diagnostics)
	}
}
