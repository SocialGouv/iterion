// The native action path (ADR-151, #2834): a `tool … action: board.*` node
// executes IN THE ENGINE through the registered transport, gated by the
// node's capabilities — the same grant an LLM node's board MCP tools answer
// to (#78=A). External test package: it imports boardops, and the seam's
// rule is that boardops never imports this one (#2857).

package model_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

func newBoardStore(t *testing.T) *native.Store {
	t.Helper()
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func boardActionNode(action string, caps []string) *ir.ToolNode {
	return &ir.ToolNode{
		BaseNode:     ir.BaseNode{ID: "plan"},
		Action:       action,
		Capabilities: caps,
	}
}

func TestNativeBoardActionReadsTheStoreWithNoModelAndNoCredential(t *testing.T) {
	t.Parallel()
	store := newBoardStore(t)
	if _, err := store.Create(native.Issue{Title: "Filed from the plan", Labels: []string{"rgaa-audit"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	node := boardActionNode("board.issue.list", []string{"board.read"})
	node.Params = []ir.ActionParam{{Key: "label", Value: "rgaa-audit"}}

	// NO ConnectorResolver wired, on purpose: the native transport owns the
	// board connector, and the HTTP catalog is never consulted for it.
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))

	out, err := e.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	items, ok := out["data"].([]any)
	if !ok {
		t.Fatalf("data is %T, want an array of issues", out["data"])
	}
	if len(items) != 1 {
		t.Fatalf("got %d issues, want the one carrying the label", len(items))
	}
	card := items[0].(map[string]any)
	if card["title"] != "Filed from the plan" {
		t.Errorf("title = %v", card["title"])
	}
}

// #78=A's rule at runtime: without the grant — and with a DECLARED
// `capabilities: []`, which is a no-grant, not an inheritance (#1801) — the
// native action is refused by the same caps.Has denial an agent's MCP call
// hears.
func TestNativeBoardActionWithoutTheGrantIsDenied(t *testing.T) {
	t.Parallel()
	store := newBoardStore(t)
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))

	for name, node := range map[string]*ir.ToolNode{
		"no capabilities line": boardActionNode("board.issue.list", nil),
		"declared empty":       boardActionNode("board.issue.list", []string{}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.Execute(context.Background(), node, nil)
			if err == nil || !strings.Contains(err.Error(), "capability denied") {
				t.Fatalf("Execute: err = %v, want the capability denial", err)
			}
			// The denial survives the node boundary TYPED — that is what lets
			// the recovery dispatcher refuse to re-run the node for the same
			// answer: the same IR meets the same grant on every attempt.
			if got := recovery.Classify(err); got != runtime.ErrCodeCapabilityDenied {
				t.Errorf("recovery.Classify = %q, want %q — a refused grant must not buy a retry", got, runtime.ErrCodeCapabilityDenied)
			}
			action, code := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
			if action.Kind == runtime.RecoveryRetrySameNode {
				t.Errorf("the engine scheduled a RETRY of a refused grant (delay %s) — the next attempt meets the same capabilities", action.Delay)
			}
			if !strings.Contains(action.Reason, "declare the capability") {
				t.Errorf("recovery reason = %q, want CAPABILITY_DENIED's own remedy — the unregistered-code fall-through names the gap, not the cure", action.Reason)
			}
			if code != runtime.ErrCodeCapabilityDenied {
				t.Errorf("dispatched code = %q, want %q", code, runtime.ErrCodeCapabilityDenied)
			}
		})
	}
}

// Inheritance is the LLM rule, unchanged: a node with no `capabilities:`
// line runs under the WORKFLOW's grant.
func TestNativeBoardActionInheritsTheWorkflowGrant(t *testing.T) {
	t.Parallel()
	store := newBoardStore(t)
	wf := &ir.Workflow{Capabilities: []string{"board.read"}}
	e := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))

	if _, err := e.Execute(context.Background(), boardActionNode("board.issue.list", nil), nil); err != nil {
		t.Fatalf("Execute with the workflow's grant inherited: %v", err)
	}
}

// And its other half (#1801): a DECLARED `capabilities: []` replaces the
// workflow's list instead of inheriting it, so a granting workflow does not
// open the board to a node that declared no grant. The tool node reads the
// one inheritance rule every LLM node reads (effectiveCapabilitiesOf).
func TestNativeBoardActionADeclaredEmptyListOverridesAGrantingWorkflow(t *testing.T) {
	t.Parallel()
	store := newBoardStore(t)
	wf := &ir.Workflow{Capabilities: []string{"board.read"}}
	e := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))

	_, err := e.Execute(context.Background(), boardActionNode("board.issue.list", []string{}), nil)
	if err == nil || !strings.Contains(err.Error(), "capability denied") {
		t.Fatalf("Execute: err = %v, want the capability denial — a declared `capabilities: []` grants nothing, whatever the workflow holds", err)
	}
	if got := recovery.Classify(err); got != runtime.ErrCodeCapabilityDenied {
		t.Errorf("recovery.Classify = %q, want %q", got, runtime.ErrCodeCapabilityDenied)
	}
}

// A verb the facade does not expose yet fails by name at the node too —
// admission is the early refusal, not the only one: this path is what an IR
// built off the admission path meets.
func TestNativeActionAVerbTheFacadeDoesNotExpose(t *testing.T) {
	t.Parallel()
	store := newBoardStore(t)
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))

	_, err := e.Execute(context.Background(), boardActionNode("board.issue.close", []string{"board.close"}), nil)
	if err == nil || !strings.Contains(err.Error(), "exposes no operation") {
		t.Fatalf("Execute: err = %v, want the facade gap named", err)
	}
}

// Registration is the seam: two transports answering one connector id is a
// wiring ambiguity, resolved loudly — the first registration stands.
func TestADuplicateTransportRegistrationKeepsTheFirst(t *testing.T) {
	t.Parallel()
	firstStore := newBoardStore(t)
	secondStore := newBoardStore(t)
	// The observable difference between the two owners: the first store is
	// EMPTY, the second holds one issue.
	if _, err := secondStore.Create(native.Issue{Title: "behind the duplicate"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithActionTransports(
			boardops.NewActionTransport(firstStore, boardops.CallEnv{}),
			boardops.NewActionTransport(secondStore, boardops.CallEnv{}),
		))

	node := boardActionNode("board.issue.list", []string{"board.read"})
	out, err := e.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	items, _ := out["data"].([]any)
	if len(items) != 0 {
		t.Fatalf("got %d issues — the SECOND registration served, arrival order deciding an ambiguity", len(items))
	}
}

// A native board action keeps a secret's PLACEHOLDER. The board is iterion's
// own store: it persists the card and shows it to every board.read holder,
// so the `params` sink — materialised for a vendor call — has no call to
// serve. An agent filing the same body through the board MCP tools files the
// placeholder (keepsPlaceholders); moving the write to a native action must
// not change that. Both arrivals are driven: the author's own
// `{{secrets.X}}`, and a placeholder that rides in on data. The oracle reads
// the BOARD, then the node's output.
func TestANativeBoardWriteKeepsTheSecretPlaceholder(t *testing.T) {
	t.Parallel()
	const value = "ghp_synthetic_board_secret_2834"
	store := newBoardStore(t)
	guard := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: value}}, secretguard.DefaultConfig())
	ph := secretguard.PlaceholderForName("GH_TOKEN")
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithSecretGuard(guard),
		model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))

	node := boardActionNode("board.issue.create", []string{"board.create"})
	node.Params = []ir.ActionParam{
		{Key: "title", Value: "probe {{secrets.GH_TOKEN}}", Refs: []*ir.Ref{{Kind: ir.RefSecrets, Path: []string{"GH_TOKEN"}, Raw: "{{secrets.GH_TOKEN}}"}}},
		{Key: "body", Value: "{{input.body}}", Refs: []*ir.Ref{{Kind: ir.RefInput, Path: []string{"body"}, Raw: "{{input.body}}"}}},
	}
	out, err := e.Execute(context.Background(), node, map[string]any{"body": "ran: curl -H 'Authorization: Bearer " + ph + "' https://api.github.com"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	issues, err := store.List(native.ListFilter{})
	if err != nil || len(issues) != 1 {
		t.Fatalf("List: %d issue(s), err %v — want the one card filed", len(issues), err)
	}
	for field, text := range map[string]string{"title": issues[0].Title, "body": issues[0].Body} {
		if strings.Contains(text, value) {
			t.Errorf("the card's %s on the board carries the secret's VALUE: %q", field, text)
		}
		if !strings.Contains(text, ph) {
			t.Errorf("the card's %s lost the placeholder: %q", field, text)
		}
	}
	rendered, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if strings.Contains(string(rendered), value) {
		t.Errorf("the node's output carries the secret's value downstream: %s", rendered)
	}
}
