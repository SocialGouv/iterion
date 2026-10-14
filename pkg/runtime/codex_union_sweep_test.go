package runtime

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// nodeOptIn is one IR-visible opt-in the sweep below applies to a node: a name
// for the reader and the mutation itself.
type nodeOptIn struct {
	name string
	node func(*ir.AgentNode)
	flow func(*ir.Workflow)
}

// theFieldsTheHarnessOwns are the fields the sweep sets itself — the route and
// the declared list are its two axes, and the id is the graph's.
var theFieldsTheHarnessOwns = map[string]bool{
	"ID": true, "Name": true, "Tools": true, "Backend": true, "Fallbacks": true,
}

// theValuesThatOpenAnAppend gives the value a field needs to MEAN something:
// `auto_memory: x` is not a mode and appends nothing, so a sentinel would sweep
// the field and prove nothing. Every other field gets a non-zero sentinel by
// kind (see sentinelFor), which is what makes the sweep exhaustive over the
// node's fields rather than over a list someone maintains.
var theValuesThatOpenAnAppend = map[string]any{
	"AutoMemory":      "on",
	"Interaction":     ir.InteractionHuman,
	"ReasoningEffort": "ultracode",
	"AmbientContext":  "all",
	"Compress":        "on",
	"Permission":      "ask",
	"Capabilities":    []string{"board.read", "runs.read"},
}

// sentinelFor builds a non-zero value for a field the table above does not
// name. A value that is merely non-zero is enough: what the sweep needs is for
// the field to be SET, so any append gated on it fires.
func sentinelFor(name string, f reflect.Value) (reflect.Value, bool) {
	if v, ok := theValuesThatOpenAnAppend[name]; ok {
		rv := reflect.ValueOf(v)
		if !rv.Type().ConvertibleTo(f.Type()) {
			return reflect.Value{}, false
		}
		return rv.Convert(f.Type()), true
	}
	switch f.Kind() {
	case reflect.String:
		return reflect.ValueOf("x").Convert(f.Type()), true
	case reflect.Bool:
		return reflect.ValueOf(true), true
	case reflect.Int, reflect.Int64:
		return reflect.ValueOf(int64(1)).Convert(f.Type()), true
	case reflect.Slice:
		if f.Type().Elem().Kind() != reflect.String {
			// A slice of structs (`fallbacks:`) is a route, not an opt-in:
			// the sweep's own axis already walks every route.
			return reflect.Value{}, false
		}
		return reflect.ValueOf([]string{"x"}).Convert(f.Type()), true
	case reflect.Pointer:
		if f.Type().Elem().Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		return reflect.New(f.Type().Elem()), true
	}
	return reflect.Value{}, false
}

// everyNodeOptIn enumerates one mutation per settable field of ir.AgentNode,
// embedded structs included, PLUS the append axes whose opt-in is declared on
// the workflow (toolSurfaceArmAppendAxes carries those halves).
//
// Reflection rather than a list: the condition the union's arm rests on is
// about what the runtime APPENDS, and an append is gated on a field. A list
// would answer for the fields someone remembered — which is how a claw-only
// append of a name no codex route receives can ship with every gate green.
func everyNodeOptIn(t *testing.T) []nodeOptIn {
	t.Helper()
	var out []nodeOptIn
	var walk func(prefix string, typ reflect.Type, path []int)
	walk = func(prefix string, typ reflect.Type, path []int) {
		for i := 0; i < typ.NumField(); i++ {
			sf := typ.Field(i)
			if !sf.IsExported() {
				continue
			}
			here := append(append([]int{}, path...), i)
			if sf.Anonymous && sf.Type.Kind() == reflect.Struct {
				walk(prefix, sf.Type, here)
				continue
			}
			if theFieldsTheHarnessOwns[sf.Name] {
				continue
			}
			name, idx := sf.Name, here
			out = append(out, nodeOptIn{
				name: "node." + name,
				node: func(n *ir.AgentNode) {
					f := reflect.ValueOf(n).Elem()
					for _, j := range idx {
						f = f.Field(j)
					}
					if v, ok := sentinelFor(name, f); ok {
						f.Set(v)
					}
				},
			})
		}
	}
	walk("node.", reflect.TypeOf(ir.AgentNode{}), nil)
	for _, axis := range toolSurfaceArmAppendAxes {
		if axis.flow == nil {
			continue
		}
		out = append(out, nodeOptIn{name: "workflow: " + axis.name, node: axis.node, flow: axis.flow})
	}
	if len(out) < reflect.TypeOf(ir.AgentNode{}).NumField() {
		t.Fatalf("the sweep built %d opt-ins for a node with %d fields: it no longer walks the IR it claims to", len(out), reflect.TypeOf(ir.AgentNode{}).NumField())
	}
	return out
}

// TestNoRouteAppendsAWorkspaceSafeNameTheCodexRouteDoesNotReceive is the class
// the union-over-routes arm of llmToolSurfaceCanWrite rests on.
//
// That arm asks the route question over the names the node holds on ANY route,
// while the question is per route: a codex node with a `fallbacks:` route onto
// claw is charged with claw's appends too. It is sound only while every name
// the runtime appends on one route, and that the engine reads as
// workspace-safe, is one codex sandboxes read-only — otherwise a legitimate
// codex fan-out is refused a shared worktree for a tool its codex route never
// receives, and nothing the author can see explains it.
//
// The condition is read where it lives: the fields an append is gated on. Each
// field of the node is set in turn, every route is asked what it then holds,
// and any workspace-safe name a route adds that the codex route does not
// receive must be codex-read-only. Both halves are producer-answered — the
// executor's own seam for what a route holds, the delegate's own classifier for
// the sandbox mode.
func TestNoRouteAppendsAWorkspaceSafeNameTheCodexRouteDoesNotReceive(t *testing.T) {
	// `grep` is the declared list on purpose: a declared name is on EVERY
	// route, so it can never be a route's own append — and declaring one of
	// the names the runtime itself appends would hide that append from the
	// sweep. codex reads `grep` as a read, so the declaration buys the
	// read-only sandbox and the extras below are the only thing that moves it.
	declared := []string{"grep"}
	routes := []string{
		delegate.BackendClaw, delegate.BackendCodex, delegate.BackendClaudeCode,
		delegate.BackendPi, delegate.BackendKimi, delegate.BackendGrok, delegate.BackendOpenCode,
	}
	held := func(optIn nodeOptIn, backend string, ultra bool) []string {
		node := agentNode("a", declared, backend, optIn.node)
		wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": node}}
		if optIn.flow != nil {
			optIn.flow(wf)
		}
		return model.NewProgramExecutor(wf).EffectiveToolNames(node, ultra)
	}
	var unsound []string
	appends := map[string]bool{}
	for _, optIn := range everyNodeOptIn(t) {
		for _, ultra := range []bool{false, true} {
			codexHolds := held(optIn, delegate.BackendCodex, ultra)
			for _, route := range routes {
				for _, name := range held(optIn, route, ultra) {
					if slices.Contains(declared, name) || slices.Contains(codexHolds, name) {
						continue
					}
					// A name codex never receives and the engine reads as
					// writing is refused by the held-name arm before the
					// route question is ever asked, so the union cannot
					// mis-charge it.
					if !toolIsWorkspaceSafe(name) {
						continue
					}
					appends[name] = true
					if !delegate.CodexDeclaredListIsReadOnly([]string{name}, true) {
						unsound = append(unsound, fmt.Sprintf("%s appends %q on the %s route, which the codex route does not receive, and codex sandboxes that name workspace-write", optIn.name, name, route))
					}
				}
			}
		}
	}
	sort.Strings(unsound)
	// One line per (field, name, route), not one per reading: the sweep asks
	// each shape twice, with and without the ultracode escalation.
	unsound = slices.Compact(unsound)
	if len(unsound) > 0 {
		t.Errorf("%d route append(s) break the condition the union-over-routes arm rests on — a codex node with such a `fallbacks:` route is refused a shared worktree for a tool its own route never holds:\n  %s",
			len(unsound), strings.Join(unsound, "\n  "))
	}
	names := make([]string, 0, len(appends))
	for n := range appends {
		names = append(names, n)
	}
	sort.Strings(names)
	// Frozen, not described: these three are the whole reach of the union's
	// extra charge today — `todo_write` on the claw route, `read_file` and
	// `glob` on it under `auto_memory:` — and each is read-only to codex.
	// A fourth name is a new charge on every codex node with such a route,
	// sound or not, and it is reviewed here rather than inferred.
	if want := []string{"glob", "read_file", "todo_write"}; !slices.Equal(names, want) {
		t.Errorf("a route appends %v that the codex route does not receive, the measured set is %v: the union charges a codex node for each of them, so a new one is read here one by one",
			names, want)
	}
}
