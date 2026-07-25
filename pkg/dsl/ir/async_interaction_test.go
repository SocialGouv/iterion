package ir

import (
	"testing"
	"time"
)

// Async human interaction (ADR-081): interaction: async + await_answers
// nodes, diagnostics C240–C242.

func asyncSrc(awaitBody string) string {
	return `
schema out:
  text: string

prompt work:
  Do the work.

agent asker:
  model: "anthropic/claude-sonnet-4-6"
  interaction: async
  user: work
  output: out

await_answers gate:
` + awaitBody + `

workflow w:
  entry: asker
  asker -> gate
  gate -> done
`
}

func TestAsyncInteraction_Compiles(t *testing.T) {
	wf := mustCompile(t, asyncSrc("  from: asker\n  timeout: \"30m\""))

	an, ok := wf.Nodes["asker"].(*AgentNode)
	if !ok {
		t.Fatalf("asker is %T, want *AgentNode", wf.Nodes["asker"])
	}
	if an.Interaction != InteractionAsync {
		t.Errorf("asker.Interaction = %v, want async", an.Interaction)
	}

	gn, ok := wf.Nodes["gate"].(*AwaitAnswersNode)
	if !ok {
		t.Fatalf("gate is %T, want *AwaitAnswersNode", wf.Nodes["gate"])
	}
	if gn.From != "asker" {
		t.Errorf("gate.From = %q, want asker", gn.From)
	}
	if gn.Timeout != 30*time.Minute {
		t.Errorf("gate.Timeout = %s, want 30m", gn.Timeout)
	}
}

func TestAwaitAnswers_TimeoutRequired(t *testing.T) {
	// No timeout → C241.
	r := compileFile(t, asyncSrc("  from: asker"))
	if got := countCode(r, DiagAwaitAnswersNoTimeout); got != 1 {
		t.Errorf("C241 (no timeout) = %d, want 1 (%v)", got, r.Diagnostics)
	}
	// Non-positive timeout → C241.
	r = compileFile(t, asyncSrc("  from: asker\n  timeout: \"0s\""))
	if got := countCode(r, DiagAwaitAnswersNoTimeout); got != 1 {
		t.Errorf("C241 (zero timeout) = %d, want 1 (%v)", got, r.Diagnostics)
	}
	// Valid timeout clears it.
	r = compileFile(t, asyncSrc("  from: asker\n  timeout: \"10m\""))
	if got := countCode(r, DiagAwaitAnswersNoTimeout); got != 0 {
		t.Errorf("C241 with valid timeout = %d, want 0 (%v)", got, r.Diagnostics)
	}
}

func TestAwaitAnswers_BadFromWarns(t *testing.T) {
	// from: unknown node → C242 warning.
	r := compileFile(t, asyncSrc("  from: nobody\n  timeout: \"10m\""))
	if got := countCode(r, DiagAwaitAnswersBadFrom); got != 1 {
		t.Errorf("C242 (unknown from) = %d, want 1 (%v)", got, r.Diagnostics)
	}

	// from: a node that is not interaction: async → C242 warning.
	src := `
schema out:
  text: string

prompt work:
  Do the work.

agent plain:
  model: "anthropic/claude-sonnet-4-6"
  user: work
  output: out

await_answers gate:
  from: plain
  timeout: "10m"

workflow w:
  entry: plain
  plain -> gate
  gate -> done
`
	r = compileFile(t, src)
	if got := countCode(r, DiagAwaitAnswersBadFrom); got != 1 {
		t.Errorf("C242 (non-async from) = %d, want 1 (%v)", got, r.Diagnostics)
	}

	// A matching async node clears it; empty from: is always fine.
	r = compileFile(t, asyncSrc("  from: asker\n  timeout: \"10m\""))
	if got := countCode(r, DiagAwaitAnswersBadFrom); got != 0 {
		t.Errorf("C242 with async from = %d, want 0 (%v)", got, r.Diagnostics)
	}
	r = compileFile(t, asyncSrc("  timeout: \"10m\""))
	if got := countCode(r, DiagAwaitAnswersBadFrom); got != 0 {
		t.Errorf("C242 with empty from = %d, want 0 (%v)", got, r.Diagnostics)
	}
}

func TestAsyncInteraction_OnHumanNodeErrors(t *testing.T) {
	src := `
schema out:
  text: string

human review:
  interaction: async
  output: out

workflow w:
  entry: review
  review -> done
`
	r := compileFile(t, src)
	if got := countCode(r, DiagAsyncOnHuman); got != 1 {
		t.Errorf("C240 (async on human) = %d, want 1 (%v)", got, r.Diagnostics)
	}
}
