package ast_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// A contract that uses every property, read from text.
const contractTransportFixture = `schema report:
  ok: bool

contract c:
  display_name: "Feature dev"
  responsibility: "Implements a feature and opens a PR"
  version: 2
  inputs:
    goal: string
      description: "What to build"
    depth: int
      required: false
      default: 3
    labels: string[]
      nullable: true
      min_items: 1
      max_items: 5
      default: null
    brief: string
      file:
        media_type: "text/markdown"
        min_bytes: 1
        schema: report
  outputs:
    pr_url: string
      from: open_pr.url
    artefact: string
      from: write
      file:
  criteria:
    goal_long_enough:
      kind: min_length
      port: input.goal
      params: {min: 2}
    declared_only:
  effects:
    opens_pr:
      description: "Opens a pull request"
      paid: true

workflow w:
  contract: c
  entry: a
  a -> done

agent a:
  description: "d"
`

// The three states of a default — absent, an explicit null, a value —
// and an explicit `required: false` are distinct in the text and stay
// distinct through the transport; the empty `file:` block survives as a
// block; the workflow keeps the contract it names.
func TestContractSurvivesTheTransport(t *testing.T) {
	pr := parser.Parse("x.bot", contractTransportFixture)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("fixture: %v", pr.Diagnostics)
	}
	raw, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"contracts"`, `"file_spec"`, `"contract": "c"`, `"default": null`, `"required": false`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("the document lacks %s:\n%s", key, raw)
		}
	}
	back, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ast.MarshalFile(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Fatalf("the document changed on a second round-trip:\n%s\n---\n%s", raw, again)
	}
	c := back.Contracts[0]
	if c.Name != "c" || c.DisplayName != "Feature dev" || c.Version == nil || *c.Version != 2 || len(c.Inputs) != 4 || len(c.Outputs) != 2 || len(c.Criteria) != 2 || len(c.Effects) != 1 {
		t.Fatalf("contract read back as %+v", c)
	}
	goal, depth, labels, brief := c.Inputs[0], c.Inputs[1], c.Inputs[2], c.Inputs[3]
	if goal.Default != nil {
		t.Errorf("an absent default came back as %s", goal.Default)
	}
	if depth.Required == nil || *depth.Required || string(depth.Default) != "3" {
		t.Errorf("depth read back as required=%v default=%s", depth.Required, depth.Default)
	}
	if string(labels.Default) != "null" || !labels.Nullable || labels.MinItems == nil || *labels.MinItems != 1 || labels.MaxItems == nil || *labels.MaxItems != 5 || labels.Type != "string[]" {
		t.Errorf("labels read back as %+v", labels)
	}
	if brief.FileSpec == nil || brief.FileSpec.MediaType != "text/markdown" || brief.FileSpec.MinBytes != 1 || brief.FileSpec.Schema != "report" {
		t.Errorf("brief's file block read back as %+v", brief.FileSpec)
	}
	if c.Outputs[0].From != "open_pr.url" || c.Outputs[1].From != "write" || c.Outputs[1].FileSpec == nil {
		t.Errorf("outputs read back as %+v, %+v", c.Outputs[0], c.Outputs[1])
	}
	if k := c.Criteria[0]; k.Kind != "min_length" || k.Port != "input.goal" || string(k.Params) != `{"min":2}` {
		t.Errorf("criterion read back as %+v", k)
	}
	if k := c.Criteria[1]; k.Name != "declared_only" || k.Kind != "" || k.Params != nil {
		t.Errorf("the bare criterion read back as %+v", k)
	}
	if e := c.Effects[0]; e.Name != "opens_pr" || !e.Paid || e.Description != "Opens a pull request" {
		t.Errorf("effect read back as %+v", e)
	}
	if back.Workflows[0].Contract != "c" {
		t.Errorf("the workflow's contract read back as %q", back.Workflows[0].Contract)
	}
	// A default the transport carries as JSON keeps its shape and its
	// precision: a large integer is not rounded through a float64.
	var v any
	d := json.NewDecoder(bytes.NewReader(depth.Default))
	d.UseNumber()
	if err := d.Decode(&v); err != nil || v != json.Number("3") {
		t.Errorf("default decoded as %#v, %v", v, err)
	}
}

// A JSON value from the transport is held in its one canonical form —
// compact, keys in order, numbers as written — and a value that is not one
// public JSON value is refused where it stands, naming the port.
func TestContractJSONFromTheTransportIsCanonicalOrRefused(t *testing.T) {
	doc := func(def string) []byte {
		return []byte(`{"contracts": [{"name": "c", "inputs": [{"name": "x", "type": "int", "default": ` + def + `}]}]}`)
	}
	f, err := ast.UnmarshalFile(doc("{ \"z\": 1e3,\n \"a\": [ ] }"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(f.Contracts[0].Inputs[0].Default); got != `{"a":[],"z":1e3}` {
		t.Errorf("the default is held as %s", got)
	}
	for _, def := range []string{`{"a": 1, "a": 2}`, `[{"k": 1, "k": 2}]`} {
		if _, err := ast.UnmarshalFile(doc(def)); err == nil || !strings.Contains(err.Error(), `input "x"`) {
			t.Errorf("%s: refusal %v does not name the port", def, err)
		}
	}
	// The same rule on a criterion's parameters.
	raw := []byte(`{"contracts": [{"name": "c", "criteria": [{"name": "k", "kind": "min_length", "params": {"min": 1, "min": 2}}]}]}`)
	if _, err := ast.UnmarshalFile(raw); err == nil || !strings.Contains(err.Error(), `criterion "k"`) {
		t.Errorf("duplicate parameters: refusal %v does not name the criterion", err)
	}
}

// A contract declared in a fragment carries the fragment as its
// provenance, on the way out and on the way back.
func TestContractCarriesItsProvenance(t *testing.T) {
	u := unit.LoadMap(map[string]string{
		"main.bot":         "dsl: 2\nimport \"lib/contract.bot\"\n\nworkflow w:\n  contract: c\n  entry: a\n  a -> done\n\nagent a:\n  description: \"d\"\n",
		"lib/contract.bot": "contract c:\n  inputs:\n    goal: string\n",
	}, "main.bot")
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	raw, err := ast.MarshalFileWithProvenance(u.Merged, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := carriers(t, raw)["contracts[0]"]; got != "lib/contract.bot" {
		t.Fatalf("contracts[0] carries %q", got)
	}
	f, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Contracts[0].Span.Start.File != "lib/contract.bot" || f.Contracts[0].Span.End.File != "lib/contract.bot" {
		t.Fatalf("the contract read back from %q", f.Contracts[0].Span.Start.File)
	}
}

// The outbound direction refuses what the inbound refuses: a document is
// never marshalled that UnmarshalFile would refuse, and the refusal names
// the port or the criterion — on the transport and on the provenance
// marshaller alike.
func TestMarshalFileRefusesAValueTheReaderWouldRefuse(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *ast.File
		want string
	}{
		"port default":     {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{{Name: "x", Type: "int", Default: json.RawMessage(`{"a":1,"a":2}`)}}}}}, `input "x"`},
		"criterion params": {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Criteria: []*ast.CriterionDecl{{Name: "k", Kind: "min_length", Params: json.RawMessage(`{"min":1,"min":2}`)}}}}}, `criterion "k"`},
		"not JSON":         {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Outputs: []*ast.PortDecl{{Name: "y", Type: "int", Default: json.RawMessage(`{`)}}}}}, `output "y"`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ast.MarshalFile(tc.f); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("MarshalFile: %v", err)
			}
			if _, err := ast.MarshalFileWithProvenance(tc.f, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("MarshalFileWithProvenance: %v", err)
			}
		})
	}
}

// An explicit version survives the transport, 0 included.
func TestContractVersionZeroSurvivesTheTransport(t *testing.T) {
	f, err := ast.UnmarshalFile([]byte(`{"contracts":[{"name":"c","version":0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Contracts[0].Version == nil || *f.Contracts[0].Version != 0 {
		t.Fatalf("version read as %v", f.Contracts[0].Version)
	}
	raw, err := ast.MarshalFile(f)
	if err != nil || !strings.Contains(string(raw), `"version": 0`) {
		t.Fatalf("version written as:\n%s (%v)", raw, err)
	}
}

// A nil element in a contract's lists is refused by the transport by index,
// on the way out as on the way in, never dereferenced.
func TestMarshalFileRefusesANilContractElementByIndex(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *ast.File
		want string
	}{
		"contract":  {&ast.File{Contracts: []*ast.ContractDecl{{Name: "a"}, nil}}, "contracts[1] is nil"},
		"input":     {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{nil}}}}, `contract "c" inputs[0] is nil`},
		"output":    {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Outputs: []*ast.PortDecl{{Name: "x", Type: "int"}, nil}}}}, `contract "c" outputs[1] is nil`},
		"criterion": {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Criteria: []*ast.CriterionDecl{nil}}}}, `contract "c" criteria[0] is nil`},
		"effect":    {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Effects: []*ast.PublicEffect{nil}}}}, `contract "c" effects[0] is nil`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ast.MarshalFile(tc.f); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MarshalFile: %v", err)
			}
		})
	}
}
