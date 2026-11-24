package ir

import "testing"

// The empty-list trap of #1801: a node's `capabilities: []` is the author
// declaring that the node holds NO host capability — an explicit empty
// grant — where an absent `capabilities:` line inherits the workflow's
// list. The two are told apart by nilness through compile, because the
// executor resolves the inheritance on nil (`if effectiveCaps == nil`).
func TestCompileDistinguishesDeclaredEmptyCapabilitiesFromAbsent(t *testing.T) {
	w := mustCompile(t, `agent inherits:
  model: "gpt-4"
  output: s

agent holds_none:
  model: "gpt-4"
  capabilities: []
  output: s

agent explicit:
  model: "gpt-4"
  capabilities: [board.read]
  output: s

schema s:
  ok: bool

workflow w:
  entry: inherits
  capabilities: [board.read]
  inherits -> holds_none
  holds_none -> explicit
  explicit -> done
`)
	get := func(id string) *AgentNode {
		t.Helper()
		n, ok := w.Nodes[id].(*AgentNode)
		if !ok {
			t.Fatalf("node %q missing or not an agent", id)
		}
		return n
	}
	if got := get("inherits").Capabilities; got != nil {
		t.Fatalf("an absent capabilities: must stay nil (inherit the workflow's), got %#v", got)
	}
	if got := get("holds_none").Capabilities; got == nil {
		t.Fatal("`capabilities: []` must compile to a DECLARED empty list (no grant), got nil (inherit)")
	} else if len(got) != 0 {
		t.Fatalf("`capabilities: []` must be empty, got %#v", got)
	}
	if got := get("explicit").Capabilities; len(got) != 1 || got[0] != "board.read" {
		t.Fatalf("a non-empty list is unchanged, got %#v", got)
	}
}

// At the workflow level the distinction carries no inheritance — the
// workflow is the root of the chain — but the declared empty list still
// round-trips: unparse writes `capabilities: []` back, and the author
// document carries it.
func TestCompileWorkflowCapabilitiesDeclaredEmpty(t *testing.T) {
	w := mustCompile(t, `agent a:
  model: "gpt-4"
  output: s

schema s:
  ok: bool

workflow w:
  entry: a
  capabilities: []
  a -> done
`)
	if w.Capabilities == nil {
		t.Fatal("a workflow's `capabilities: []` must compile to a DECLARED empty list, got nil (absent)")
	}
	if len(w.Capabilities) != 0 {
		t.Fatalf("`capabilities: []` must be empty, got %#v", w.Capabilities)
	}
}
