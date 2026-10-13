package ir

import (
	"strings"
	"testing"
)

// #1455: an {{input.X}} a node body reads that no incoming edge of the node
// maps is empty on every path — a compile-time warning that names the
// incoming edges, entry node aside.
const neverMappedSrc = `
schema s:
  ok: bool

prompt pu:
  Inventory: {{input.adrs}}

agent survey:
  model: "m"
  user: pu
  output: s

agent pm:
  model: "m"
  user: pu
  output: s

workflow test:
  entry: survey
  survey -> pm
  pm -> done
`

func TestAnInputNoEdgeMapsIsACompileWarning(t *testing.T) {
	r := compileFile(t, neverMappedSrc)
	expectDiag(t, r, DiagInputFieldNeverMapped)
	for _, d := range r.Diagnostics {
		if d.Code != DiagInputFieldNeverMapped {
			continue
		}
		// The hint names the incoming edges — the one place a mapping can
		// land — and the warning stays a warning (warn over reject).
		if !strings.Contains(d.Hint, "incoming edges of \"pm\": `survey -> pm`") {
			t.Errorf("the hint does not name the incoming edges: %s", d.Hint)
		}
		if d.Severity != SeverityWarning {
			t.Errorf("C308 fired as %v, want a warning", d.Severity)
		}
		if d.Message == "" || !strings.Contains(d.Message, "empty on every path") {
			t.Errorf("the message does not say the field is empty on every path: %s", d.Message)
		}
	}
}

// A field another incoming edge maps is mapped — the union across ALL
// incoming edges, loop back-edge and sibling `when` path included.
func TestAnInputAnotherEdgeMapsIsNotACompileWarning(t *testing.T) {
	src := strings.Replace(neverMappedSrc,
		"  survey -> pm\n",
		"  survey -> pm with { adrs: \"{{outputs.survey.ok}}\" }\n",
		1)
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNeverMapped)
}

// The entry node reads the run-level payload, whose keys the compiler
// cannot know: no C308 there, the same silence C034 keeps.
func TestTheEntryInputReadIsNotACompileWarning(t *testing.T) {
	src := strings.Replace(neverMappedSrc, "  entry: survey\n", "  entry: pm\n", 1)
	src = strings.Replace(src, "  survey -> pm\n", "  survey -> pm\n  pm -> done\n", 1)
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNeverMapped)
}

// A subbot or emit `with:` reads the parent's run inputs (no `input:`
// surface on the kind): C149's business, not an incoming-edge mapping's.
func TestASubbotWithInputReadIsNotACompileWarning(t *testing.T) {
	src := `
schema out:
  url: string

subbot child:
  source: "kids/k.bot"
  with { goal: "{{input.goal}}" }
  output: out

workflow p:
  worktree: none
  entry: child
  child -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNeverMapped)
}

// Every node-body site the check owns: a tool command and a compute
// `expr:`. The fail `message:` in the same fixture is NOT one of them — it
// is resolved against the run's inputs, so it belongs to C149
// (TestAFailMessageInputReadIsC149NotC308).
func TestTheInputCheckCoversEveryNodeBodySite(t *testing.T) {
	src := `
schema s:
  ok: bool

tool probe:
  command: "probe {{input.inv}}"

compute tally:
  output: s
  expr:
    ok: "input.inv != ''"

fail stopped:
  code: STOPPED
  message: "inventory: {{input.inv}}"

agent worker:
  model: "m"
  output: s

workflow test:
  worktree: none
  entry: worker
  worker -> probe
  probe -> tally
  tally -> stopped when not ok
  tally -> done when ok
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInputFieldNeverMapped)
	nodes := map[string]int{}
	for _, d := range r.Diagnostics {
		if d.Code == DiagInputFieldNeverMapped {
			nodes[d.NodeID]++
		}
	}
	// One per reading site the node's own input serves: the tool command and
	// the compute field (whose refs arrive through the expression AST).
	for _, id := range []string{"probe", "tally"} {
		if nodes[id] == 0 {
			t.Errorf("the check missed the node-body site on %q: %v", id, r.Diagnostics)
		}
	}
	if nodes["stopped"] != 0 {
		t.Errorf("the fail node's message was held to the incoming-edge rule: %v", r.Diagnostics)
	}
}

// A fail node's `message:` is rendered at fail time by resolveMapping,
// whose `input` arm reads the RUN's inputs — measured end to end: a run
// launched with `inv=…` renders the payload, a run without renders the
// empty string, and a `with { inv: … }` on the incoming edge changes
// neither (pkg/runtime, TestAFailMessageReadsTheRunsInputsNotTheEdges). So
// C308's claim ("empty on every path", remedy: map it on an incoming edge)
// is false there and its remedy inert: the read is C149's class, the one
// that states the run-inputs reading and offers `{{vars.x}}`.
func TestAFailMessageInputReadIsC149NotC308(t *testing.T) {
	src := `
schema s:
  ok: bool

agent worker:
  model: "m"
  output: s

fail stopped:
  code: STOPPED
  message: "inventory: {{input.inv}}"

workflow test:
  worktree: none
  entry: worker
  worker -> stopped
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNeverMapped)
	expectDiag(t, r, DiagWithInputRefNoSchema)
	for _, d := range r.Diagnostics {
		if d.Code != DiagWithInputRefNoSchema {
			continue
		}
		if d.Severity != SeverityWarning {
			t.Errorf("C149 fired as %v on a fail message, want a warning", d.Severity)
		}
		for _, want := range []string{"RUN's inputs", "launch payload", "{{vars.inv}}", "no incoming edge can carry"} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("the C149 message does not say %q: %s", want, d.Message)
			}
		}
	}
	// The mapping C308 used to prescribe does not silence the reading: the
	// surface does not read the node's input at all.
	mapped := strings.Replace(src, "  worker -> stopped\n", "  worker -> stopped with { inv: \"{{outputs.worker.ok}}\" }\n", 1)
	expectDiag(t, compileFile(t, mapped), DiagWithInputRefNoSchema)
}

// An `interaction_prompt:` is the stand-in model's system prompt when the
// node's own delegate asks questions, and ITS input is the question map the
// asking node built (pkg/backend/model, ExecuteHumanLLMForInteraction —
// measured: `{{input.q1}}` renders the question text, a key that is not a
// question stays the literal placeholder). So neither the input schema
// (C034) nor the incoming edges (C308) describe that surface: the compiler
// keeps the silence it keeps for the entry floor, rather than refusing the
// only spelling a question key has.
func TestAnInteractionPromptInputReadIsNotCheckedAgainstTheNodesInput(t *testing.T) {
	src := `
schema s:
  ok: bool

schema in:
  seed: string

prompt ask:
  Answer about {{input.q1}} given {{input.seed}}.

agent lead:
  model: "m"
  output: s

agent worker:
  model: "m"
  input: in
  output: s
  interaction: llm
  interaction_prompt: ask
  interaction_model: "m"

workflow test:
  worktree: none
  entry: lead
  lead -> worker with { seed: "{{outputs.lead.ok}}" }
  worker -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNeverMapped)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
	// The node's OTHER prompts keep the full reading: only the interaction
	// prompt reads the questions.
	withUser := strings.Replace(src, "  interaction_prompt: ask\n", "  interaction_prompt: ask\n  user: ask\n", 1)
	expectDiag(t, compileFile(t, withUser), DiagInputFieldNotInSchema)
}

// Runtime-injected fields (a leading underscore) are the engine's, never an
// edge mapping's: silent.
func TestARuntimeInjectedInputFieldIsSilent(t *testing.T) {
	src := strings.Replace(neverMappedSrc, "{{input.adrs}}", "{{input._session_id}}", 1)
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNeverMapped)
}

// A node body on a node with NO incoming edge at all — unreachable, C016's
// business — stays silent here.
func TestAnUnreachableNodeInputReadIsSilent(t *testing.T) {
	src := neverMappedSrc + `
agent orphan:
  model: "m"
  user: pu
  output: s
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInputFieldNeverMapped) // pm still warns
	for _, d := range r.Diagnostics {
		if d.Code == DiagInputFieldNeverMapped && d.NodeID == "orphan" {
			t.Errorf("the unreachable node was held to the mapping rule: %s", d.Message)
		}
	}
}
