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

// Every node-body site the check owns: a tool command, a compute `expr:`,
// a fail `message:`.
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
	sites := 0
	for _, d := range r.Diagnostics {
		if d.Code == DiagInputFieldNeverMapped {
			sites++
		}
	}
	// One per reading site: the command, the compute field, the fail
	// message. (compute `expr:` refs arrive through the expression AST, the
	// fail message through its own walk.)
	if sites < 3 {
		t.Errorf("the check met %d of the node-body sites, want one per site: %v", sites, r.Diagnostics)
	}
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
