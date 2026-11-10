package model

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The dispatcher exists so a caller composing calls on the node's behalf — the
// codemode script host — cannot acquire a second dispatch path. These tests
// drive its OWN entry point, the one such a caller holds, with the context
// newToolDispatcher vouches for. The model loop witnesses the pipeline
// already; what it cannot witness is the door being driven directly, which is
// how the host will drive it.

func TestToolDispatcher_ACallEnteringThroughDispatchMeetsThePermissionGate(t *testing.T) {
	t.Parallel()
	pol, err := permission.NewPolicy(permission.ModeAsk, []string{"Bash(go test:*)"}, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	var executed bool
	d, ctx := newToolDispatcher(context.Background(), bashTool(&executed), nil, nil, nil, nil, pol)

	block, abort := d.dispatch(ctx, bashCall("rm -rf /")[0])
	if abort != nil {
		t.Fatalf("a deny is a tool error the model can act on, not a loop abort: %v", abort)
	}
	if executed {
		t.Error("the gate is the anti-prompt-injection boundary: a denied tool must NOT execute through dispatch either")
	}
	raw, _ := json.Marshal(block)
	if !strings.Contains(string(raw), "denied") {
		t.Errorf("dispatch must return the gate's refusal tool_result, got: %s", raw)
	}
}

func TestToolDispatcher_AnAllowedCallStillRuns(t *testing.T) {
	t.Parallel()
	pol, err := permission.NewPolicy(permission.ModeAsk, []string{"Bash(go test:*)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var executed bool
	d, ctx := newToolDispatcher(context.Background(), bashTool(&executed), nil, nil, nil, nil, pol)

	if _, abort := d.dispatch(ctx, bashCall("go test ./...")[0]); abort != nil {
		t.Fatalf("unexpected abort: %v", abort)
	}
	if !executed {
		t.Error("an allow-listed call must run through dispatch — a gate that refuses everything proves nothing")
	}
}

// TestToolDispatcher_RefusesAContextItDoesNotVouchFor keys on the guard that
// replaces four re-pinned facts.
//
// It is the one witness the model loop cannot stand in for: the loop always
// dispatches with the context newToolDispatcher handed it, so it can never
// present an unvouched one. Three per-node controls — the #1717 narrowing, the
// #2877 child-ask degradation and the parent policy a child inherits — ride
// that context and fail OPEN, so an unvouched dispatch must fail CLOSED.
func TestToolDispatcher_RefusesAContextItDoesNotVouchFor(t *testing.T) {
	t.Parallel()
	var executed bool
	d, nodeCtx := newToolDispatcher(context.Background(), bashTool(&executed), nil, nil, nil, nil, nil)

	// A bare context — the shape a host that cannot thread the node's context
	// would naturally pass, and the shape an earlier revision of these very
	// tests used.
	block, abort := d.dispatch(context.Background(), bashCall("echo hi")[0])
	if abort != nil {
		t.Fatalf("the refusal is a tool error, not a loop abort: %v", abort)
	}
	if executed {
		t.Fatal("a dispatch on an unvouched context must NOT execute: the command narrowing, the child-ask degradation and a child's inherited policy all ride that context and all fail open")
	}
	raw, _ := json.Marshal(block)
	if !strings.Contains(string(raw), "not the one this node's dispatcher vouches for") {
		t.Errorf("the refusal must name what is wrong so a mis-wired host is told loudly, got: %s", raw)
	}

	// The control: the context the constructor handed back works, and so does
	// a descendant of it — a guard that refused those would have closed the
	// door on its only legitimate caller.
	if _, abort := d.dispatch(nodeCtx, bashCall("echo hi")[0]); abort != nil {
		t.Fatalf("the vouched context must dispatch: %v", abort)
	}
	if !executed {
		t.Fatal("the vouched context must reach execution")
	}
	executed = false
	derived, cancel := context.WithCancel(context.WithValue(nodeCtx, struct{ k string }{"unrelated"}, 1))
	defer cancel()
	if _, abort := d.dispatch(derived, bashCall("echo hi")[0]); abort != nil {
		t.Fatalf("a descendant of the vouched context must dispatch: %v", abort)
	}
	if !executed {
		t.Fatal("a descendant carries the node's facts and must reach execution")
	}
}

// TestToolDispatcher_ThePinnedSetFollowsTheToolMap turns the pin's property
// into a gate instead of a sentence.
//
// An earlier revision cached the set at construction and asserted in a comment
// that the tool map never changes over a dispatcher's life. Nothing enforced
// that: a mutant recomputing the set per call was green across the whole
// package, so the cache and the derivation were indistinguishable and a future
// writer would have made the pin silently disagree with the lookup. This test
// is what fails against the cache.
func TestToolDispatcher_ThePinnedSetFollowsTheToolMap(t *testing.T) {
	t.Parallel()
	var seen []string
	var pinned bool
	probe := func(ctx context.Context, _ json.RawMessage) (string, error) {
		set, ok := tool.NodeExecutableToolsFrom(ctx)
		pinned = ok
		seen = seen[:0]
		for name := range set {
			seen = append(seen, name)
		}
		sort.Strings(seen)
		return "ok", nil
	}
	toolMap := map[string]*GenerationTool{"probe": {Name: "probe", Execute: probe}}
	d, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)
	call := toolUseBlock{ID: "tu_1", Name: "probe", PartialJSON: "{}"}

	dispatchAndRead := func(t *testing.T, want []string) {
		t.Helper()
		if _, abort := d.dispatch(ctx, call); abort != nil {
			t.Fatalf("unexpected abort: %v", abort)
		}
		if !pinned {
			t.Fatal("dispatch must pin the node's executable set: tool_search reports \"not pinned\" rather than guessing, so an unpinned call silently changes its answer (#2717)")
		}
		if len(seen) != len(want) {
			t.Fatalf("pinned set = %v, want %v", seen, want)
		}
		for i := range want {
			if seen[i] != want[i] {
				t.Fatalf("pinned set = %v, want %v", seen, want)
			}
		}
	}

	dispatchAndRead(t, []string{"probe"})

	// A tool appears in the map the dispatcher resolves against. The pin is
	// derived from that same map, so it must have gained it too.
	toolMap["Write"] = &GenerationTool{Name: "Write", Execute: probe}
	dispatchAndRead(t, []string{"Write", "probe"})

	// And it must lose what the map lost — a pin that only ever grows would
	// name a tool the lookup can no longer resolve.
	delete(toolMap, "Write")
	dispatchAndRead(t, []string{"probe"})
}
