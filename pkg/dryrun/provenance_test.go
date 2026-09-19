package dryrun

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// A collection finding carries its surface from the failing site: a
// foreach's finding is said "foreach <name> (collection)", an over's plain
// "collection" — a foreach and a node may share a name, so the label is
// carried, never guessed from it. The mutation: guess from the name (the
// pre-fix rule) — the over's finding is then pinned on the foreach.
func TestACollectionFindingCarriesItsSurface(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "surface",
		Entry: "gen",
		Nodes: map[string]ir.Node{
			"gen": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gen"}, SchemaFields: ir.SchemaFields{OutputSchema: "payload_out"}},
		},
		Schemas: map[string]*ir.Schema{
			"payload_out": {Name: "payload_out", Fields: []*ir.SchemaField{
				{Name: "payload", Type: ir.FieldTypeJSON},
			}},
		},
		Foreaches: map[string]*ir.Foreach{
			"scan": {Name: "scan", CollectionRaw: "{{outputs.gen.payload}}", CollectionRefs: []*ir.Ref{
				{Kind: ir.RefOutputs, Path: []string{"gen", "payload"}},
			}},
		},
	}
	for _, tc := range map[string]struct {
		collection string
		want       string
	}{
		"over":    {collection: "over", want: "collection"},
		"foreach": {collection: "foreach", want: "foreach scan (collection)"},
	} {
		x := NewExecutor(wf, true, nil, nil)
		standIn, ok := x.Inconclusive(runtime.ExpressionFailure{
			NodeID:     "scan",
			Source:     "{{outputs.gen.payload}}",
			Refs:       []expr.Ref{{Namespace: "outputs", Path: []string{"gen", "payload"}}},
			Collection: tc.collection,
			Err:        errors.New("coercion failed"),
		})
		if !ok {
			t.Fatalf("%s: the shaped collection was not read as invented", tc.collection)
		}
		if _, isList := standIn.([]any); !isList {
			t.Fatalf("%s: the stand-in is not the simulated list: %#v", tc.collection, standIn)
		}
		findings := x.Findings()
		if len(findings) != 1 || findings[0].Where != tc.want {
			t.Fatalf("%s: the finding reads %+v, want where %q", tc.collection, findings, tc.want)
		}
	}
}
