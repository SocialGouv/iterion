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

// C333: a connection written on a native action authenticates nothing —
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
		t.Fatalf("connection: on a native action: got %s, want C333", strings.Join(codesOf(diags), "\n"))
	}
}

// C332: on a command:/script: node the list has no consumer — an inert
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
		t.Fatalf("capabilities: on a command: node: got %s, want C332", strings.Join(codesOf(diags), "\n"))
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

// C332's own sentence is the rule: the grant is read ONLY by the action
// recipe's NATIVE transport. An HTTP action (a connection: authenticates it)
// never reads node capabilities either, so capabilities: there is the same
// inert grant reading as configured — the warning must cover it.
func TestCapabilitiesOnAnHTTPActionWarnInert(t *testing.T) {
	_, diags := compileSource(t, `
tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  capabilities: [board.read]
  params:
    body: "done"

workflow main:
  entry: comment
  comment -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	if !hasCode(diags, DiagToolCapabilitiesInert) {
		t.Fatalf("capabilities: on an HTTP action: got %s, want C332 — the grant is inert there", strings.Join(codesOf(diags), "\n"))
	}
}

// C334: a native board action whose grant is declared NOWHERE — not on the
// node, not on the workflow — is refused CAPABILITY_DENIED at run time; the
// compiler can say it earlier and name the capability (the C328 posture:
// warn, with the remedy).
func TestNativeBoardActionWithoutTheGrantAnywhere(t *testing.T) {
	_, diags := compileSource(t, `
tool plan:
  action: board.issue.list

workflow main:
  entry: plan
  plan -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	if !hasCode(diags, DiagNativeActionWithoutGrant) {
		t.Fatalf("board.issue.list with no board.read anywhere: got %s, want C334", strings.Join(codesOf(diags), "\n"))
	}
}

// The same action WITH the grant — on the node, or inherited from the
// workflow — compiles without the warning.
func TestNativeBoardActionWithTheGrantWarnsNot(t *testing.T) {
	for name, src := range map[string]string{
		"granted on the node": `
tool plan:
  action: board.issue.list
  capabilities: [board.read]

workflow main:
  entry: plan
  plan -> done
`,
		"inherited from the workflow": `
tool plan:
  action: board.issue.list

workflow main:
  capabilities: [board.read]
  entry: plan
  plan -> done
`,
		"a declared empty list is a DECLARED no-grant, not an inheritance": `
tool plan:
  action: board.issue.list
  capabilities: []

workflow main:
  capabilities: [board.read]
  entry: plan
  plan -> done
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := compileSource(t, src)
			has := hasCode(diags, DiagNativeActionWithoutGrant)
			if strings.Contains(name, "empty list") {
				if !has {
					t.Errorf("a declared capabilities: [] over a granting workflow must still warn — it is a no-grant (#1801), got %s", strings.Join(codesOf(diags), "\n"))
				}
				return
			}
			if has {
				t.Errorf("the grant is in reach: got %s, want no C334", strings.Join(codesOf(diags), "\n"))
			}
		})
	}
}

// C335: retry: is the HTTP ladder's — Retry-After, idempotency keys,
// ambiguous effects. On a native action it is inert (a store refusal is not
// re-issued in a sleep loop), and an inert property reads as configured.
func TestRetryOnANativeActionWarns(t *testing.T) {
	_, diags := compileSource(t, `
tool plan:
  action: board.issue.list
  capabilities: [board.read]
  retry: 3

workflow main:
  entry: plan
  plan -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	if !hasCode(diags, DiagActionRetryOnNative) {
		t.Fatalf("retry: on a native action: got %s, want C335", strings.Join(codesOf(diags), "\n"))
	}
	// timeout: IS honoured on a native action — no warning for it.
	_, diags2 := compileSource(t, `
tool plan:
  action: board.issue.list
  capabilities: [board.read]
  timeout: 30s

workflow main:
  entry: plan
  plan -> done
`)
	if hasCode(diags2, DiagActionRetryOnNative) {
		t.Errorf("timeout: on a native action must not warn — it is honoured: %s", strings.Join(codesOf(diags2), "\n"))
	}
}

// C334's message must name the case the author is actually in (review r2):
// "declared neither on the node nor on the workflow" is FALSE when the
// workflow grants and the node's declared list overrode it — and the remedy
// there is to drop the override, not to add a grant the workflow already
// holds.
func TestC334MessageDistinguishesAbsentFromOverridden(t *testing.T) {
	t.Run("granted nowhere", func(t *testing.T) {
		_, diags := compileSource(t, `
tool plan:
  action: board.issue.list

workflow main:
  entry: plan
  plan -> done
`)
		msg := diagText(diags, DiagNativeActionWithoutGrant)
		if msg == "" {
			t.Fatal("want C334")
		}
		if !strings.Contains(msg, "neither on the node nor on the workflow") {
			t.Errorf("the nowhere case must say so: %s", msg)
		}
	})
	t.Run("a declared [] overrides a granting workflow", func(t *testing.T) {
		_, diags := compileSource(t, `
tool plan:
  action: board.issue.list
  capabilities: []

workflow main:
  capabilities: [board.read]
  entry: plan
  plan -> done
`)
		msg := diagText(diags, DiagNativeActionWithoutGrant)
		if msg == "" {
			t.Fatal("want C334 — a declared no-grant over a granting workflow IS the denied case")
		}
		if strings.Contains(msg, "neither on the node nor on the workflow") {
			t.Errorf("the workflow DOES declare it — the message lies: %s", msg)
		}
		lower := strings.ToLower(msg)
		for _, want := range []string{"replaces", "drop the node's"} {
			if !strings.Contains(lower, want) {
				t.Errorf("the override case must say the node's list %q and name the remedy (%q): %s", "replaces", want, msg)
			}
		}
	})
	// The third case (review r3): the node declares a list that does NOT
	// hold the capability and the workflow grants nothing either — the
	// message is neither "nowhere" (the node DID declare) nor the override
	// remedy (the workflow holds nothing to inherit).
	t.Run("declared on the node, without the capability, workflow silent", func(t *testing.T) {
		_, diags := compileSource(t, `
tool plan:
  action: board.issue.list
  capabilities: [watch.subscribe]

workflow main:
  entry: plan
  plan -> done
`)
		msg := diagText(diags, DiagNativeActionWithoutGrant)
		if msg == "" {
			t.Fatal("want C334")
		}
		for _, want := range []string{"does not grant either", "to the node's list"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the node-declared case must say %q: %s", want, msg)
			}
		}
	})
}

// diagText returns the first message carrying code, "" when absent.
func diagText(diags []Diagnostic, code DiagCode) string {
	for _, d := range diags {
		if d.Code == code {
			return d.Message
		}
	}
	return ""
}

// A registry recipe naming an iterion host tool is the list's second reader
// (the runtime holds its dispatch to the node's grant, model.nodeCapabilities),
// so the compiler judges it against the SAME list: C328 on the declared list
// that does not hold the tool's capability — a declared `capabilities: []`
// included, which overrides a granting workflow (#1801) — and no C332, since
// the list has a reader. One scenario per arm.
func TestARegistryHostToolIsJudgedAgainstTheToolNodesOwnList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nodeCaps string // the node's capabilities: line, "" for none
		wfCaps   string
		wantC328 bool
	}{
		{"declared empty under a granting workflow", "  capabilities: []\n", "[board.read, board.create]", true},
		{"declared without the tool's capability", "  capabilities: [board.read]\n", "[board.create]", true},
		{"declared with it under a workflow granting nothing", "  capabilities: [board.create]\n", "[]", false},
		{"no line inherits the granting workflow", "", "[board.create]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := compileSource(t, `
tool file:
  command: "mcp.iterion_board.create_issue"
`+tc.nodeCaps+`
workflow main:
  capabilities: `+tc.wfCaps+`
  entry: file
  file -> done
`)
			if errs := errorsOnly(diags); len(errs) > 0 {
				t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
			}
			if got := hasCode(diags, DiagHostToolWithoutCapability); got != tc.wantC328 {
				t.Fatalf("C328 = %v, want %v: %s", got, tc.wantC328, strings.Join(codesOf(diags), "\n"))
			}
			if hasCode(diags, DiagToolCapabilitiesInert) {
				t.Fatalf("C332 on a registry recipe naming a host tool — its list has a reader: %s", strings.Join(codesOf(diags), "\n"))
			}
		})
	}
}

// And the registry recipe that names ANY other tool reads no grant: its
// declared list is inert, C332's case.
func TestCapabilitiesOnARegistryRecipeNamingAnotherToolWarnInert(t *testing.T) {
	_, diags := compileSource(t, `
tool reads:
  command: "read_file"
  capabilities: [board.read]

workflow main:
  entry: reads
  reads -> done
`)
	if !hasCode(diags, DiagToolCapabilitiesInert) {
		t.Fatalf("capabilities: on a registry recipe naming no host tool: got %s, want C332", strings.Join(codesOf(diags), "\n"))
	}
}

// C336: the native verb set is the binary's own (boardcaps.ActionCapability,
// held to the facade by its matrix test), so a verb the facade does not
// expose is known at compile time — admission refuses it on every launch.
// The exposed verbs stay silent, and the message lists them.
func TestANativeVerbTheFacadeDoesNotExposeWarns(t *testing.T) {
	_, diags := compileSource(t, `
tool closeit:
  action: board.issue.close
  capabilities: [board.close]

workflow main:
  entry: closeit
  closeit -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %s", strings.Join(codesOf(errs), "\n"))
	}
	var msg string
	for _, d := range diags {
		if d.Code == DiagNativeVerbNotExposed {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("board.issue.close: got %s, want C336", strings.Join(codesOf(diags), "\n"))
	}
	for _, verb := range []string{"board.issue.list", "board.issue.create", "board.issue.transition"} {
		if !strings.Contains(msg, verb) {
			t.Fatalf("C336 does not list the exposed verb %s: %s", verb, msg)
		}
	}

	_, diags = compileSource(t, boardActionBot)
	if hasCode(diags, DiagNativeVerbNotExposed) {
		t.Fatalf("an exposed verb drew C336: %s", strings.Join(codesOf(diags), "\n"))
	}
}
