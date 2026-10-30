package facade_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/facade"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// fixturePkg is a two-operation connector: one curated read, one mutating
// long-tail operation.
func fixturePkg() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{ID: "probe", Version: "1.0.0"},
		Ops: []spec.OpsFile{{Operations: []spec.Operation{
			{
				ID: "probe.issue.list", Resource: "issue", Verb: "list",
				Summary: "List issues", Effect: spec.EffectRead, MCP: true,
				Params: []spec.Param{
					{Key: "owner", Name: "owner", In: "path", Type: "string", Required: true},
					{Key: "state", Name: "state", In: "query", Type: "string", Enum: []string{"open", "closed"}},
				},
			},
			{
				ID: "probe.issue.create", Resource: "issue", Verb: "create",
				Summary: "Create an issue", Effect: spec.EffectCreate,
				Params: []spec.Param{{Key: "title", Name: "title", In: "body", Type: "string", Required: true}},
			},
		}}},
	}
}

func TestFacadeListsTheCuratedSetPlusTheDiscoveryPair(t *testing.T) {
	f, err := facade.New(fixturePkg())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tools := f.Tools()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "probe_issue_list") {
		t.Errorf("the curated read is not listed: %v", names)
	}
	if !strings.Contains(joined, facade.SearchTool) || !strings.Contains(joined, facade.CallTool) {
		t.Errorf("the discovery pair is not listed: %v", names)
	}
	if strings.Contains(joined, "probe_issue_create") {
		t.Errorf("an uncurated operation must NOT be a named tool — 500 tools bury a context: %v", names)
	}
	// Every tool schema reserves the connection argument.
	for _, tool := range tools {
		if tool.Name == facade.SearchTool {
			continue
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props[facade.ConnArg]; !ok {
			t.Errorf("tool %s carries no %q argument — the model could not name what authenticates the call", tool.Name, facade.ConnArg)
		}
	}
}

func TestFacadeSearchFindsTheLongTail(t *testing.T) {
	f, err := facade.New(fixturePkg())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	hits, _ := f.SearchOperations("create", 0)
	if len(hits) != 1 || hits[0].ID != "probe.issue.create" {
		t.Fatalf("search(create) = %+v", hits)
	}
	// A blank query lists the curated set first.
	hits, _ = f.SearchOperations("", 0)
	if len(hits) != 2 || !hits[0].Curated {
		t.Errorf("search(\"\") = %+v, want curated first", hits)
	}
}

func TestFacadeCallDispatchesAndExtractsTheConnection(t *testing.T) {
	f, err := facade.New(fixturePkg())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var gotOp, gotAlias string
	var gotParams map[string]any
	execFn := func(_ context.Context, op, alias string, params map[string]any) (any, error) {
		gotOp, gotAlias, gotParams = op, alias, params
		return map[string]any{"ok": true}, nil
	}

	// A curated call by tool name.
	out, err := f.Call(context.Background(), "probe_issue_list",
		json.RawMessage(`{"connection":"main","owner":"me","state":"open"}`), execFn)
	if err != nil {
		t.Fatalf("curated call: %v", err)
	}
	if gotOp != "probe.issue.list" || gotAlias != "main" {
		t.Errorf("dispatch = %q via %q", gotOp, gotAlias)
	}
	if _, leaked := gotParams[facade.ConnArg]; leaked {
		t.Errorf("the connection alias leaked into the operation's params: %v", gotParams)
	}
	if out.(map[string]any)["ok"] != true {
		t.Errorf("result = %v", out)
	}

	// A curated operation reached THROUGH the long tail: the discovery pair
	// still dispatches what the operator curated, by id as well as by name.
	if _, err := f.Call(context.Background(), facade.CallTool,
		json.RawMessage(`{"operation":"probe.issue.list","connection":"main","arguments":{"owner":"me"}}`), execFn); err != nil {
		t.Fatalf("call_operation on a curated operation: %v", err)
	}
	if gotOp != "probe.issue.list" {
		t.Errorf("call_operation dispatched %q", gotOp)
	}

	// An UNCURATED operation is refused, even though search listed it: the
	// curated set is what the operator read, and an agent reaching past it is
	// an effect nobody granted. (probe.issue.create carries no `mcp: true`.)
	if _, err := f.Call(context.Background(), facade.CallTool,
		json.RawMessage(`{"operation":"probe.issue.create","connection":"main","arguments":{"title":"hi"}}`), execFn); err == nil {
		t.Errorf("call_operation served an uncurated operation — the curated list an operator reads was not the bound")
	} else if !strings.Contains(err.Error(), "curated set") {
		t.Errorf("the refusal must name the curation: %v", err)
	} else if gotOp == "probe.issue.create" {
		t.Errorf("the executor was reached for a refused operation")
	}

	// The discovery tool answers WITHOUT an executor call.
	out, err = f.Call(context.Background(), facade.SearchTool, json.RawMessage(`{"query":"issue"}`), execFn)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	ops := out.(map[string]any)["operations"].([]facade.OperationHit)
	if len(ops) != 2 {
		t.Errorf("search(issue) hit %d operations, want 2", len(ops))
	}
	if total := out.(map[string]any)["total"]; total != 2 {
		t.Errorf("search(issue) total = %v, want 2", total)
	}
}

// TestSearchNamesTheTruncation: the page is capped, and the cap must be
// visible — a model that cannot see it reads twenty hits as all of them and
// never narrows its query.
//
// Mutation that reddens this: drop the `total` field from the search tool's
// answer (or truncate without counting).
func TestSearchNamesTheTruncation(t *testing.T) {
	f, err := facade.New(fixturePkg())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	hits, total := f.SearchOperations("", 1)
	if len(hits) != 1 {
		t.Fatalf("search(\"\", 1) returned %d hits, want the capped 1", len(hits))
	}
	if total != 2 {
		t.Errorf("total = %d, want the UNTRUNCATED match count 2", total)
	}
	out, err := f.Call(context.Background(), facade.SearchTool, json.RawMessage(`{"limit":1}`), func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errors.New("search never reaches the executor")
	})
	if err != nil {
		t.Fatalf("search call: %v", err)
	}
	if total := out.(map[string]any)["total"]; total != 2 {
		t.Errorf("the tool answer's total = %v, want 2", total)
	}
}

func TestFacadeCallRefusesWhatItCannotServe(t *testing.T) {
	f, err := facade.New(fixturePkg())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	execFn := func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errors.New("must not be reached")
	}
	for _, tc := range []struct {
		name, tool, args, wantIn string
	}{
		{"an unknown tool", "probe_issue_nope", `{}`, "no tool"},
		{"an unknown operation", facade.CallTool, `{"operation":"probe.nope.x","connection":"main"}`, "no operation"},
		{"a non-string connection", "probe_issue_list", `{"connection":42}`, "string"},
		{"arguments that are not an object", facade.CallTool, `{"operation":"probe.issue.list","connection":"main","arguments":[1]}`, "object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Call(context.Background(), tc.tool, json.RawMessage(tc.args), execFn)
			if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %v, want %q", err, tc.wantIn)
			}
		})
	}
}

// TestFacadeOverTheBakedForgejoPackage pins the wiring the whole lot hangs
// on: the shipped package's overlay curates a set (mcp: true), and the facade
// reads the SAME package the deterministic node resolves.
func TestFacadeOverTheBakedForgejoPackage(t *testing.T) {
	pkg, _, err := connection.BakedCatalog().Package("forgejo")
	if err != nil {
		t.Fatalf("baked forgejo: %v", err)
	}
	f, err := facade.New(pkg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tools := f.Tools()
	if len(tools) < 5 {
		t.Fatalf("the baked forgejo facade lists %d tools — the overlay's curation did not reach it", len(tools))
	}
	// The ADR's reference operation must be reachable, curated or long tail.
	hits, total := f.SearchOperations("issue.comment", 5)
	if total < 1 || total < len(hits) {
		t.Errorf("total = %d for %d hits — the untruncated count", total, len(hits))
	}
	found := false
	for _, h := range hits {
		if h.ID == "forgejo.issue.comment" {
			found = true
		}
	}
	if !found {
		t.Errorf("forgejo.issue.comment is not discoverable: %+v", hits)
	}
}

// TestFacadeRefusesACollidingPackage: two operations mapping to one tool name
// would list a tool that calls the wrong operation.
func TestFacadeRefusesACollidingPackage(t *testing.T) {
	pkg := fixturePkg()
	dup := pkg.Ops[0].Operations[0]
	dup.ID = "probe_issue_list" // maps to the same tool name as probe.issue.list
	dup.MCP = true
	pkg.Ops[0].Operations = append(pkg.Ops[0].Operations, dup)
	if _, err := facade.New(pkg); err == nil {
		t.Fatal("a tool-name collision must refuse the facade, not serve a wrong tool")
	}
}
