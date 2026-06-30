package ir

import (
	"testing"
	"time"
)

// emitWaitSrc builds a minimal valid emit→wait workflow. The fragments for the
// emit and wait bodies are spliced in so individual tests can omit fields.
func emitWaitSrc(emitBody, waitBody string) string {
	return `
schema payload:
  value: int

compute seed:
  output: payload
  expr:
    value: "1"

emit e1:
` + emitBody + `

wait w1:
` + waitBody + `

workflow w:
  entry: seed
  seed -> e1
  e1 -> w1
  w1 -> done
`
}

func TestEmitWait_Compiles(t *testing.T) {
	wf := mustCompile(t, emitWaitSrc(
		"  event: \"ready\"\n  with { value: \"{{outputs.seed.value}}\" }",
		"  event: \"ready\"\n  timeout: \"5s\"\n  output: payload",
	))

	en, ok := wf.Nodes["e1"].(*EmitNode)
	if !ok {
		t.Fatalf("e1 is %T, want *EmitNode", wf.Nodes["e1"])
	}
	if en.Event != "ready" {
		t.Errorf("emit Event = %q, want ready", en.Event)
	}
	if len(en.With) != 1 || en.With[0].Key != "value" {
		t.Errorf("emit With = %+v, want one mapping for key value", en.With)
	}

	wn, ok := wf.Nodes["w1"].(*WaitNode)
	if !ok {
		t.Fatalf("w1 is %T, want *WaitNode", wf.Nodes["w1"])
	}
	if wn.Event != "ready" {
		t.Errorf("wait Event = %q, want ready", wn.Event)
	}
	if wn.Timeout != 5*time.Second {
		t.Errorf("wait Timeout = %s, want 5s", wn.Timeout)
	}
	if wn.OutputSchema != "payload" {
		t.Errorf("wait OutputSchema = %q, want payload", wn.OutputSchema)
	}
}

func TestWait_TimeoutRequired(t *testing.T) {
	// No timeout → C197.
	r := compileFile(t, emitWaitSrc(
		"  event: \"ready\"",
		"  event: \"ready\"",
	))
	if got := countCode(r, DiagWaitNoTimeout); got != 1 {
		t.Errorf("C197 (no timeout) = %d, want 1 (%v)", got, r.Diagnostics)
	}

	// Non-positive timeout → C197.
	r = compileFile(t, emitWaitSrc(
		"  event: \"ready\"",
		"  event: \"ready\"\n  timeout: \"0s\"",
	))
	if got := countCode(r, DiagWaitNoTimeout); got != 1 {
		t.Errorf("C197 (zero timeout) = %d, want 1 (%v)", got, r.Diagnostics)
	}

	// A valid timeout clears it.
	r = compileFile(t, emitWaitSrc(
		"  event: \"ready\"",
		"  event: \"ready\"\n  timeout: \"30s\"",
	))
	if got := countCode(r, DiagWaitNoTimeout); got != 0 {
		t.Errorf("C197 with valid timeout = %d, want 0 (%v)", got, r.Diagnostics)
	}
}

func TestEvent_NoName(t *testing.T) {
	// Emit and wait both missing `event:` → two C196.
	r := compileFile(t, emitWaitSrc(
		"  with { value: \"{{outputs.seed.value}}\" }",
		"  timeout: \"5s\"",
	))
	if got := countCode(r, DiagEventNoName); got != 2 {
		t.Errorf("C196 (no event name) = %d, want 2 (%v)", got, r.Diagnostics)
	}
}

func TestEvent_DanglingWarns(t *testing.T) {
	// wait awaits "missing" which no emit produces, and emit "ready" has no
	// consumer → two C198 warnings.
	r := compileFile(t, emitWaitSrc(
		"  event: \"ready\"",
		"  event: \"missing\"\n  timeout: \"5s\"",
	))
	if got := countCode(r, DiagEventNoListener); got != 2 {
		t.Errorf("C198 (dangling event) = %d, want 2 (%v)", got, r.Diagnostics)
	}

	// Matched names → no C198.
	r = compileFile(t, emitWaitSrc(
		"  event: \"ready\"",
		"  event: \"ready\"\n  timeout: \"5s\"",
	))
	if got := countCode(r, DiagEventNoListener); got != 0 {
		t.Errorf("C198 with matched event = %d, want 0 (%v)", got, r.Diagnostics)
	}
}
