package runview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	connspec "github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A bot whose `action:` nodes nothing here can serve is refused at ADMISSION
// (#1072 honesty 1). Before this, every input of the verdict was known at
// launch and the run was started anyway: it spent its earlier nodes, took
// their effects, reached the action node and died there with "this process has
// no connector catalog wired" — the same verdict, after the money.
func TestAnActionNodeIsRefusedAtAdmission(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	comment := &ir.ToolNode{Action: "forgejo.issue.comment", Connection: "forge_main"}
	comment.ID = "comment"

	_, err = BuildExecutor(ExecutorSpec{
		Ctx:      context.Background(),
		Store:    st,
		RunID:    "run-action-admission",
		Workflow: &ir.Workflow{Name: "canary", Nodes: map[string]ir.Node{"comment": comment}},
		// Connectors deliberately unset: the cloud plane, the runner, and any
		// local install with no catalog all arrive here exactly like this.
	})
	if err == nil {
		t.Fatal("a workflow with an action node and no connector catalog must be refused at admission, not at the node")
	}
	var unserved *UnservedActionsError
	if !errors.As(err, &unserved) {
		t.Fatalf("the refusal must be typed *UnservedActionsError so a caller can tell it from a per-connection grant refusal: %T %v", err, err)
	}
	if len(unserved.Nodes) != 1 || unserved.Nodes[0].NodeID != "comment" {
		t.Errorf("Nodes = %+v, want the one action node", unserved.Nodes)
	}
	// The message names what to fix. "Refused" with no package to install is
	// no better than the mid-run failure it replaces.
	for _, want := range []string{"comment", "forgejo.issue.comment", "forgejo", "forge_main", "iterion connections add", "098-connector-catalog.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
}

// stubResolver is a catalog that is WIRED. What it would answer does not
// matter here: admission asks whether anything can serve the node at all.
type stubResolver struct{}

func (stubResolver) ResolveAction(context.Context, string, string) (*connspec.Package, connspec.Operation, exec.Credential, string, error) {
	return nil, connspec.Operation{}, exec.Credential{}, "", errors.New("stubResolver is wired, not functional")
}

// The refusal fires on the ONE case it is about and on no other: a workflow
// with no action node is none of its business, and a wired catalog answers the
// question.
func TestAdmissionRefusesOnlyTheUnservedCase(t *testing.T) {
	action := &ir.ToolNode{Action: "forgejo.issue.comment", Connection: "forge_main"}
	action.ID = "a"
	shell := &ir.ToolNode{Command: "go test ./..."}
	shell.ID = "s"
	agent := &ir.AgentNode{}
	agent.ID = "g"

	for _, tc := range []struct {
		name    string
		nodes   map[string]ir.Node
		wired   bool
		wantErr bool
	}{
		{"action node, nothing wired", map[string]ir.Node{"a": action}, false, true},
		{"action node, catalog wired", map[string]ir.Node{"a": action}, true, false},
		{"no action node, nothing wired", map[string]ir.Node{"s": shell, "g": agent}, false, false},
		{"empty workflow", map[string]ir.Node{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var connectors model.ConnectorResolver
			if tc.wired {
				connectors = stubResolver{}
			}
			err := admitConnectorActions(&ir.Workflow{Name: "canary", Nodes: tc.nodes}, connectors)
			if tc.wantErr && err == nil {
				t.Error("want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want no refusal, got %v", err)
			}
		})
	}
}

// The listing an operator and this test both read is ORDERED by node id, not
// by Go's map iteration. Eight nodes inserted in reverse: an unsorted walk
// lands on the sorted sequence once in 8! attempts, and Go randomises map
// order on every iteration.
func TestActionNodesAreOrderedByNodeID(t *testing.T) {
	nodes := map[string]ir.Node{}
	var want []string
	for i := 8; i >= 1; i-- {
		id := fmt.Sprintf("n%d", i)
		n := &ir.ToolNode{Action: fmt.Sprintf("forgejo.issue.verb%d", i), Connection: "forge_main"}
		n.ID = id
		nodes[id] = n
		want = append([]string{id}, want...)
	}
	got := ActionNodes(&ir.Workflow{Name: "canary", Nodes: nodes})
	if len(got) != len(want) {
		t.Fatalf("ActionNodes returned %d nodes, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].NodeID != w {
			t.Fatalf("ActionNodes order = %v, want sorted by node id %v", actionNodeIDs(got), want)
		}
	}
	// The connector is the part a tier serves, split off the operation id.
	if got[0].Connector != "forgejo" {
		t.Errorf("Connector = %q, want forgejo — the refusal names the package to install", got[0].Connector)
	}
}

func actionNodeIDs(ns []ActionNode) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.NodeID)
	}
	return out
}

// The refusal is LOAD-BEARING — it stops a run — so it owes a greppable way
// out, and the way out owes a witness that it is the only spelling that works.
// A bot whose action node is on a branch this launch will not take, and a
// caller running ONE node rather than a run, are both legitimate.
func TestTheAdmissionRefusalHasExactlyOneWayOut(t *testing.T) {
	action := &ir.ToolNode{Action: "forgejo.issue.comment", Connection: "forge_main"}
	action.ID = "a"
	wf := &ir.Workflow{Name: "canary", Nodes: map[string]ir.Node{"a": action}}

	for _, tc := range []struct {
		value   string
		wantErr bool
	}{
		{"off", false},
		{"", true},
		{"refuse", true},
		// A near miss must NOT read as the opt-out: it would turn a typo into
		// a silently disabled guard.
		{"0", true},
		{"OFF", true},
		{"false", true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(ConnectorAdmissionEnv, tc.value)
			err := admitConnectorActions(wf, nil)
			if tc.wantErr && err == nil {
				t.Errorf("%s=%q must not lift the refusal", ConnectorAdmissionEnv, tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%s=%q must lift it: %v", ConnectorAdmissionEnv, tc.value, err)
			}
		})
	}
	// And the refusal NAMES the variable, or the way out is one only its
	// author can find.
	t.Setenv(ConnectorAdmissionEnv, "")
	err := admitConnectorActions(wf, nil)
	if err == nil || !strings.Contains(err.Error(), ConnectorAdmissionEnv) {
		t.Errorf("the refusal must name %s: %v", ConnectorAdmissionEnv, err)
	}
}
