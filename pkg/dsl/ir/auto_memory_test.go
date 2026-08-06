package ir

import (
	"strings"
	"testing"
)

// TestCompileAutoMemory exercises `auto_memory:` end-to-end (parser → AST →
// IR) at the workflow level and on both LLM node kinds.
func TestCompileAutoMemory(t *testing.T) {
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
  auto_memory: on

judge gate:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  auto_memory: off

workflow minimal:
  entry: start
  auto_memory: on
  start -> gate
  gate -> done
`
	w := mustCompile(t, src)

	if w.AutoMemory != "on" {
		t.Errorf("workflow.AutoMemory = %q, want on", w.AutoMemory)
	}
	a, ok := w.Nodes["start"].(*AgentNode)
	if !ok {
		t.Fatalf("start node = %T, want *AgentNode", w.Nodes["start"])
	}
	if a.AutoMemory != "on" {
		t.Errorf("agent.AutoMemory = %q, want on", a.AutoMemory)
	}
	// A node's `off` must reach the IR verbatim, not collapse to "" —
	// otherwise it would read as "inherit" and the workflow's `on` would
	// silently win over an explicit per-node opt-out.
	j, ok := w.Nodes["gate"].(*JudgeNode)
	if !ok {
		t.Fatalf("gate node = %T, want *JudgeNode", w.Nodes["gate"])
	}
	if j.AutoMemory != "off" {
		t.Errorf("judge.AutoMemory = %q, want off", j.AutoMemory)
	}
}

// A typo must not read as "inherit" — that is a silent opt-out of the very
// thing the author asked for, so it is an error (C131) at every site.
func TestValidateAutoMemoryInvalid(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
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
  auto_memory: bogus
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
  auto_memory: bogus

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

judge start:
  model: "test-model"
  output: empty
  auto_memory: bogus

workflow w:
  entry: start
  start -> done
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectDiag(t, compileFile(t, tc.src), DiagInvalidAutoMemory)
		})
	}
}

// `ultra` is valid for compress: and not for auto_memory: — a value that is
// legal on the neighbouring knob is the typo most likely to be made.
func TestValidateAutoMemoryRejectsCompressValues(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  auto_memory: ultra

workflow w:
  entry: start
  start -> done
`
	expectDiag(t, compileFile(t, src), DiagInvalidAutoMemory)
}

func TestValidateAutoMemoryUnsupportedBackend(t *testing.T) {
	mk := func(backend, value string) string {
		return `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: "` + backend + `"
  auto_memory: ` + value + `

workflow w:
  entry: start
  start -> done
`
	}

	t.Run("warns on a backend that ignores it", func(t *testing.T) {
		expectDiag(t, compileFile(t, mk("kimi", "on")), DiagAutoMemoryNotSupported)
	})
	for _, backend := range []string{"claude_code", "claw", "pi"} {
		t.Run("silent on "+backend, func(t *testing.T) {
			expectNoDiag(t, compileFile(t, mk(backend, "on")), DiagAutoMemoryNotSupported)
		})
	}
	// `off` on an unsupported backend is not worth a diagnostic: the node
	// gets exactly what it asked for. Warning there would also fire on every
	// node of a mixed-backend workflow whose default is off.
	t.Run("silent when the node asks for off", func(t *testing.T) {
		expectNoDiag(t, compileFile(t, mk("kimi", "off")), DiagAutoMemoryNotSupported)
	})
	// The workflow default reaches every node, so warning per-node would be
	// noise on any workflow that mixes backends — as long as SOMETHING can
	// honour it, the setting is doing its job and the nodes that cannot are
	// simply unaffected.
	t.Run("silent when the workflow default reaches at least one backend that honours it", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: "kimi"

judge check:
  model: "test-model"
  output: empty
  backend: "claw"

workflow w:
  entry: start
  auto_memory: on
  start -> check
  check -> done
`
		expectNoDiag(t, compileFile(t, src), DiagAutoMemoryNotSupported)
	})
	// But a workflow-level `on` that NOTHING can honour is inert, and the
	// runtime skips the mirror without a word — the author sees a memory that
	// is simply always empty. One warning about the workflow, not one per
	// node.
	t.Run("warns when no node in the workflow can honour the default", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: "kimi"

workflow w:
  entry: start
  auto_memory: on
  start -> done
`
		expectDiag(t, compileFile(t, src), DiagAutoMemoryNotSupported)
	})
	// Every node opting out is a DIFFERENT failure from an unsupported
	// backend, and saying the wrong one sends the author hunting a problem
	// that is not there.
	t.Run("distinguishes an all-opted-out workflow from an unsupported one", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: "claw"
  auto_memory: off

workflow w:
  entry: start
  auto_memory: on
  start -> done
`
		diags := compileFile(t, src)
		expectDiag(t, diags, DiagAutoMemoryNotSupported)
		for _, d := range diags.Diagnostics {
			if d.Code == DiagAutoMemoryNotSupported && strings.Contains(d.Message, "wired for claude_code") {
				t.Errorf("claw IS supported here — the node opted out; message blames the backend: %s", d.Message)
			}
		}
	})
	// Both causes at once must not be reported as one. Blaming the backends on
	// a workflow that HAS a wired one reads as a contradiction, and half the
	// real answer — the author's own per-node `off` — goes unsaid.
	t.Run("names both causes when a workflow has each", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent a:
  model: "test-model"
  output: empty
  backend: "claw"
  auto_memory: off

judge b:
  model: "test-model"
  output: empty
  backend: "kimi"

workflow w:
  entry: a
  auto_memory: on
  a -> b
  b -> done
`
		diags := compileFile(t, src)
		expectDiag(t, diags, DiagAutoMemoryNotSupported)
		for _, d := range diags.Diagnostics {
			if d.Code != DiagAutoMemoryNotSupported {
				continue
			}
			if !strings.Contains(d.Message, "auto_memory: off") {
				t.Errorf("the opted-out node is half the reason and must be named: %s", d.Message)
			}
			if strings.Contains(d.Message, "NO agent/judge node can honour it") {
				t.Errorf("claw IS wired here — blaming the backends alone contradicts the message's own list: %s", d.Message)
			}
		}
	})
	// An unresolved backend is left alone: the runtime falls through to env
	// and host credential detection, so the compiler genuinely cannot know
	// and a guess here would be a false warning on a workflow that works.
	t.Run("silent when the effective backend is unresolved", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  auto_memory: on
  start -> done
`
		expectNoDiag(t, compileFile(t, src), DiagAutoMemoryNotSupported)
	})
}

// The likeliest shape in a real bot: the backend is declared once on the
// workflow, not repeated on every node. Reading only the node's own
// `backend:` missed it, so the author asking for auto_memory got nothing —
// no warning, and a runtime that correctly skips the mirror without a word.
func TestValidateAutoMemoryUsesTheEffectiveBackend(t *testing.T) {
	unsupported := `
schema empty:
  ok: bool

agent w:
  model: "test-model"
  output: empty
  auto_memory: on

workflow win:
  entry: w
  default_backend: "kimi"
  w -> done
`
	expectDiag(t, compileFile(t, unsupported), DiagAutoMemoryNotSupported)

	// A node's own backend still wins over the workflow default, both ways.
	nodeOverridesToSupported := `
schema empty:
  ok: bool

agent w:
  model: "test-model"
  output: empty
  backend: "claw"
  auto_memory: on

workflow win:
  entry: w
  default_backend: "kimi"
  w -> done
`
	expectNoDiag(t, compileFile(t, nodeOverridesToSupported), DiagAutoMemoryNotSupported)

	// No backend anywhere: the resolver still has env and host credential
	// detection to go, so the compiler cannot know and must stay silent.
	unknown := `
schema empty:
  ok: bool

agent w:
  model: "test-model"
  output: empty
  auto_memory: on

workflow win:
  entry: w
  w -> done
`
	expectNoDiag(t, compileFile(t, unknown), DiagAutoMemoryNotSupported)
}
