package ir

import "testing"

// TestCompileWorkspaceCheckpoint exercises the field end-to-end — parser →
// AST → IR — because the runner only ever reads it from the compiled
// workflow, which is what travels to the pod. A field that parses but never
// lands in the IR is a switch wired to nothing, and this one decides whether
// a run force-pushes a branch to the repository it was pointed at.
func TestCompileWorkspaceCheckpoint(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow minimal:
  entry: start
  workspace_checkpoint: off
  start -> done
`
	w := mustCompile(t, src)
	if w.WorkspaceCheckpoint != "off" {
		t.Errorf("workflow.WorkspaceCheckpoint = %q, want off", w.WorkspaceCheckpoint)
	}
}

// TestValidateWorkspaceCheckpointInvalid: a typo must be an ERROR, not a
// silent "inherit". The default is ON, so `workspace_checkpoint: of` would
// keep pushing the run's whole tree to the target repository — the exact
// thing an author writing this field is trying to stop — with nothing said.
func TestValidateWorkspaceCheckpointInvalid(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  workspace_checkpoint: bogus
  start -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInvalidWorkspaceCheckpoint)
}

// TestValidateWorkspaceCheckpointValidNoDiag confirms the accepted barewords
// — and the unset case — stay silent.
func TestValidateWorkspaceCheckpointValidNoDiag(t *testing.T) {
	for _, v := range []string{"on", "off"} {
		t.Run(v, func(t *testing.T) {
			src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  workspace_checkpoint: ` + v + `
  start -> done
`
			expectNoDiag(t, compileFile(t, src), DiagInvalidWorkspaceCheckpoint)
		})
	}
	t.Run("unset", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  start -> done
`
		expectNoDiag(t, compileFile(t, src), DiagInvalidWorkspaceCheckpoint)
	})
}
