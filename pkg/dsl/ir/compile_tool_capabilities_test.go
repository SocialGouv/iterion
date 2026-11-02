package ir

import (
	"strings"
	"testing"
)

// `capabilities:` on a tool node (ADR-151, #78=A): one authorization
// surface — the node declares the grant exactly like an agent or judge, and
// the native action's connector needs no `connection:`.

const boardActionBot = `
tool plan:
  action: board.issue.list
  capabilities: [board.read]
  params:
    label: "rgaa-audit"

workflow main:
  entry: plan
  plan -> done
`

// The happy path: a native board action with its read grant compiles clean
// — no C261 (a native connector holds no credential), and the grant reaches
// the IR.
func TestBoardActionWithCapabilitiesCompilesClean(t *testing.T) {
	wf, diags := compileSource(t, boardActionBot)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	if hasCode(diags, DiagToolCapabilitiesInert) || hasCode(diags, DiagActionConnectionOnNative) {
		t.Fatalf("unexpected warnings: %s", strings.Join(codesOf(diags), "\n"))
	}
	node := wf.Nodes["plan"].(*ToolNode)
	if len(node.Capabilities) != 1 || node.Capabilities[0] != "board.read" {
		t.Fatalf("plan.Capabilities = %v, want [board.read]", node.Capabilities)
	}
	if node.Action != "board.issue.list" {
		t.Fatalf("plan.Action = %q", node.Action)
	}
}

// The #1801 nilness rule holds on a tool node too: a declared
// `capabilities: []` is a declared NO-GRANT (non-nil empty), an absent line
// is inheritance (nil) — the distinction the runtime's effective grant
// reads.
func TestToolCapabilitiesDeclaredEmptyVersusAbsent(t *testing.T) {
	wf, diags := compileSource(t, `
tool reads_nothing:
  action: board.issue.list
  capabilities: []

tool inherits:
  action: board.issue.list

workflow main:
  capabilities: [board.read]
  entry: reads_nothing
  reads_nothing -> inherits
  inherits -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	empty := wf.Nodes["reads_nothing"].(*ToolNode)
	if empty.Capabilities == nil {
		t.Error("a declared `capabilities: []` compiled to nil — a declared no-grant turned back into an inheritance (#1801)")
	}
	if len(empty.Capabilities) != 0 {
		t.Errorf("Capabilities = %v, want declared empty", empty.Capabilities)
	}
	if wf.Nodes["inherits"].(*ToolNode).Capabilities != nil {
		t.Error("an absent capabilities: line must stay nil (inherit the workflow's)")
	}
}

// C261 stays the rule for every HTTP connector: only the registered native
// ids are credential-free.
func TestHTTPActionStillNeedsAConnection(t *testing.T) {
	_, diags := compileSource(t, `
tool comment:
  action: forgejo.issue.comment

workflow main:
  entry: comment
  comment -> done
`)
	if !hasCode(diags, DiagActionNoConnection) {
		t.Fatalf("an HTTP action without connection: got %s, want C261", strings.Join(codesOf(diags), "\n"))
	}
}

// C330: a connection written on a native action authenticates nothing —
// warned, not refused.
func TestConnectionOnANativeActionWarns(t *testing.T) {
	_, diags := compileSource(t, `
tool plan:
  action: board.issue.list
  connection: board_main
  capabilities: [board.read]

workflow main:
  entry: plan
  plan -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	if !hasCode(diags, DiagActionConnectionOnNative) {
		t.Fatalf("connection: on a native action: got %s, want C330", strings.Join(codesOf(diags), "\n"))
	}
}

// C329: on a command:/script: node the list has no consumer — an inert
// grant reads as configured.
func TestCapabilitiesOnAShellToolWarnInert(t *testing.T) {
	_, diags := compileSource(t, `
tool listing:
  command: `+"`ls`"+`
  capabilities: [board.read]

workflow main:
  entry: listing
  listing -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	if !hasCode(diags, DiagToolCapabilitiesInert) {
		t.Fatalf("capabilities: on a command: node: got %s, want C329", strings.Join(codesOf(diags), "\n"))
	}
}

// The same shape/known checks judge a tool node's entries — one surface:
// a malformed capability is C081 on a tool node exactly as on an agent.
func TestToolCapabilityShapeIsChecked(t *testing.T) {
	_, diags := compileSource(t, `
tool plan:
  action: board.issue.list
  capabilities: [Board.Read]

workflow main:
  entry: plan
  plan -> done
`)
	if !hasCode(diags, DiagMalformedCapability) {
		t.Fatalf("malformed capability on a tool node: got %s, want C081", strings.Join(codesOf(diags), "\n"))
	}
}
