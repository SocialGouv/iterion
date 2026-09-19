package dryrun

import (
	"errors"
	"strings"
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
		// gen produced on this pass — the scenario is a produced shape the
		// coercion refused, not an absent producer.
		x.markProduced("gen")
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

// `loop.<name>.previous_output` is the snapshot of the crossing BEFORE the
// latest one, taken from the edge the pass SELECTED — whatever the
// workflow's edge list holds. Two crossings of one name from two different
// sources: the previous snapshot is worker_a's (the crossing before the
// last), and worker_b — present in the edge list, never a snapshot — must
// not veto the answer. The mutation: read the LATEST crossing's source (the
// wrong crossing — worker_b's, which never left a previous snapshot).
func TestAPreviousOutputIsTheCrossingBeforeTheLatest(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "ambig",
		Entry: "tally",
		Nodes: map[string]ir.Node{
			"tally":    &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "tally"}, SchemaFields: ir.SchemaFields{OutputSchema: "total"}},
			"worker_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker_a"}, SchemaFields: ir.SchemaFields{OutputSchema: "countA"}},
			"worker_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker_b"}, SchemaFields: ir.SchemaFields{OutputSchema: "countB"}},
		},
		Schemas: map[string]*ir.Schema{
			"countA": {Name: "countA", Fields: []*ir.SchemaField{{Name: "count", Type: ir.FieldTypeJSON}}},
			"countB": {Name: "countB", Fields: []*ir.SchemaField{{Name: "count", Type: ir.FieldTypeInt}}},
			"total":  {Name: "total", Fields: []*ir.SchemaField{{Name: "n", Type: ir.FieldTypeJSON}}},
		},
		Edges: []*ir.Edge{
			{From: "tally", To: "worker_a"},
			{From: "tally", To: "worker_b"},
			{From: "worker_a", To: "tally", LoopName: "again"},
			{From: "worker_b", To: "tally", LoopName: "again"},
		},
		Loops: map[string]*ir.Loop{
			"again": {
				Name:          "again",
				MaxIterations: 2,
				// Compiled shape: the body and the entries the compiler
				// derives — the loop edges' targets ARE the entries, which
				// is why no reset may live on the crossing path.
				Body:    map[string]bool{"tally": true, "worker_a": true, "worker_b": true},
				Entries: map[string]bool{"tally": true},
			},
		},
	}
	x := NewExecutor(wf, true, nil, nil)
	x.markProduced("worker_a")
	// The pass crossed `again` from worker_a, then from worker_b: the
	// previous snapshot is worker_a's.
	x.recordLoopCrossing("again", "worker_a", "tally")
	x.recordLoopCrossing("again", "worker_b", "tally")
	_, ok := x.Inconclusive(runtime.ExpressionFailure{
		NodeID: "tally",
		Field:  "n",
		Source: "loop.again.previous_output.count + 1",
		Refs:   []expr.Ref{{Namespace: "loop", Path: []string{"again", "previous_output", "count"}}},
		Err:    errors.New("cannot apply + to map and int64"),
	})
	if !ok {
		t.Fatal("the previous crossing's snapshot was not read as invented")
	}
	findings := x.Findings()
	if len(findings) != 1 {
		t.Fatalf("one finding expected, got %+v", findings)
	}
	if !strings.Contains(findings[0].Detail, "worker_a") {
		t.Fatalf("the consult did not answer on the previous crossing's source: %s", findings[0].Detail)
	}
	if !strings.Contains(findings[0].Detail, "a json field the dry run shaped") {
		t.Fatalf("the consult did not name the shaped field: %s", findings[0].Detail)
	}
}
