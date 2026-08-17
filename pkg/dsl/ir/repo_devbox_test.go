package ir

import "testing"

// TestCompileRepoDevbox exercises the field end-to-end — parser → AST → IR
// — because the value is only ever read from the compiled workflow: a field
// that parses but never lands in the IR is a switch wired to nothing.
func TestCompileRepoDevbox(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow minimal:
  entry: start
  repo_devbox: off
  start -> done
`
	w := mustCompile(t, src)
	if w.RepoDevbox != "off" {
		t.Errorf("workflow.RepoDevbox = %q, want off", w.RepoDevbox)
	}
}

// TestValidateRepoDevboxInvalid: a typo must be an ERROR, not a silent
// "inherit". The default is ON, so `repo_devbox: of` would keep installing
// the very toolchain the author meant to skip, with nothing said.
func TestValidateRepoDevboxInvalid(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  repo_devbox: bogus
  start -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInvalidRepoDevbox)
}

// TestValidateRepoDevboxValidNoDiag confirms the accepted barewords — and
// the unset case — stay silent.
func TestValidateRepoDevboxValidNoDiag(t *testing.T) {
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
  repo_devbox: ` + v + `
  start -> done
`
			expectNoDiag(t, compileFile(t, src), DiagInvalidRepoDevbox)
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
		expectNoDiag(t, compileFile(t, src), DiagInvalidRepoDevbox)
	})
}
