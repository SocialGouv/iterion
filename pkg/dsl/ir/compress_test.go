package ir

import "testing"

// TestCompileCompress exercises the compress field end-to-end: parser → AST → IR
// on a workflow that sets compress at workflow level, on an agent node, on a
// judge node, and on a tool node. Each value uses one of the accepted
// barewords (on / off / ultra) so the C102 validator stays silent.
func TestCompileCompress(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  hi

prompt usr:
  hi

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  compress: ultra

judge gate:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  compress: off

tool ship:
  command: "true"
  output: empty
  compress: on

workflow minimal:
  entry: start
  compress: on
  start -> gate
  gate -> ship
  ship -> done
`
	w := mustCompile(t, src)

	if w.Compress != "on" {
		t.Errorf("workflow.Compress = %q, want on", w.Compress)
	}
	a, ok := w.Nodes["start"].(*AgentNode)
	if !ok {
		t.Fatalf("start node = %T, want *AgentNode", w.Nodes["start"])
	}
	if a.Compress != "ultra" {
		t.Errorf("agent.Compress = %q, want ultra", a.Compress)
	}
	j, ok := w.Nodes["gate"].(*JudgeNode)
	if !ok {
		t.Fatalf("gate node = %T, want *JudgeNode", w.Nodes["gate"])
	}
	if j.Compress != "off" {
		t.Errorf("judge.Compress = %q, want off", j.Compress)
	}
	tn, ok := w.Nodes["ship"].(*ToolNode)
	if !ok {
		t.Fatalf("ship node = %T, want *ToolNode", w.Nodes["ship"])
	}
	if tn.Compress != "on" {
		t.Errorf("tool.Compress = %q, want on", tn.Compress)
	}
}

// TestValidateCompressInvalid asserts that a typo like `compress: bogus` raises
// the C102 diagnostic on every site (workflow + agent + judge + tool),
// not just one — a silent fallback to "inherit" would defeat the
// purpose of the field.
func TestValidateCompressInvalid(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "workflow",
			src: `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  compress: bogus
  start -> done
`,
		},
		{
			name: "agent",
			src: `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  compress: bogus

workflow w:
  entry: start
  start -> done
`,
		},
		{
			name: "judge",
			src: `
schema empty:
  ok: bool

judge gate:
  model: "test-model"
  output: empty
  compress: bogus

workflow w:
  entry: gate
  gate -> done
`,
		},
		{
			name: "tool",
			src: `
schema empty:
  ok: bool

tool ship:
  command: "true"
  output: empty
  compress: bogus

workflow w:
  entry: ship
  ship -> done
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, tc.src)
			expectDiag(t, r, DiagInvalidCompress)
		})
	}
}

// TestValidateCompressValidNoDiag confirms that the three accepted barewords
// ("on", "off", "ultra") never trigger C102.
func TestValidateCompressValidNoDiag(t *testing.T) {
	for _, v := range []string{"on", "off", "ultra"} {
		t.Run(v, func(t *testing.T) {
			src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  compress: ` + v + `

workflow w:
  entry: start
  compress: ` + v + `
  start -> done
`
			r := compileFile(t, src)
			expectNoDiag(t, r, DiagInvalidCompress)
		})
	}
}
