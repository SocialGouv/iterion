package subbotcontracts_test

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/subbotcontracts"
)

// ProjectOutput builds the parent's subbot output from the child's contract:
// a `from: <node>.<field>` port takes that field, a whole-schema or file port
// (`from: <node>`) takes the node's whole output, a port whose producer — or
// field — the child never produced is an absent key (not a null), and a port
// name renames whatever it carries. The values are the child's own: a
// projection that fabricated a default, or keyed by the producer instead of
// the port, would lie to every reader of `outputs.<subbot>.*`.
func TestProjectOutputReadsTheContractPortsFromTheChildsNodes(t *testing.T) {
	contract := &ir.PublicContract{Name: "kid", Outputs: []*ir.PublicPort{
		{Name: "url", Type: "string", FromNode: "build", FromField: "url"},
		{Name: "passed", Type: "bool", FromNode: "verify", FromField: "passed"},
		{Name: "report", Type: "json", FromNode: "publish", FromField: ""},
		{Name: "bundle", Type: "json", File: &ir.PublicFile{MediaType: "application/zip"}, FromNode: "publish", FromField: ""},
		{Name: "later", Type: "string", FromNode: "skipped", FromField: "url"},
		{Name: "empty", Type: "string", FromNode: "verify", FromField: "no_such_field"},
	}}
	nodeOutputs := map[string]map[string]any{
		"build":   {"url": "https://forge/pr/1", "log": "…"},
		"verify":  {"passed": true},
		"publish": {"summary": "done", "bytes": 12},
		"skipped": nil, // a producer that ran and produced nothing
	}
	got := subbotcontracts.ProjectOutput(contract, nodeOutputs)
	want := map[string]any{
		"url":    "https://forge/pr/1",
		"passed": true,
		"report": map[string]any{"summary": "done", "bytes": 12},
		"bundle": map[string]any{"summary": "done", "bytes": 12},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projected =\n%v\nwant\n%v", got, want)
	}
	if _, ok := got["later"]; ok {
		t.Error("a port whose producer produced nothing must be an absent key, not a value")
	}
	if _, ok := got["empty"]; ok {
		t.Error("a port whose field the producer lacks must be an absent key, not a value")
	}
}

// An explicit null the child produced travels: the projection only omits
// what was never produced.
func TestProjectOutputCarriesAnExplicitNullThrough(t *testing.T) {
	contract := &ir.PublicContract{Name: "kid", Outputs: []*ir.PublicPort{
		{Name: "pr_url", Type: "string", Nullable: true, FromNode: "tail", FromField: "url"},
	}}
	got := subbotcontracts.ProjectOutput(contract, map[string]map[string]any{
		"tail": {"url": nil},
	})
	v, ok := got["pr_url"]
	if !ok {
		t.Fatal("a produced explicit null must travel as the port's value")
	}
	if v != nil {
		t.Fatalf("pr_url = %v, want nil", v)
	}
}
