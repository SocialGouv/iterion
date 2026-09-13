package unparse_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

const portsDocument = `dsl: 2

contract Assemble:
  display_name: "Assembler le produit"
  responsibility: "Produire le rapport à partir de la recherche et du brief"
  version: 2
  inputs:
    research: Dossier[]
      min_items: 0
      max_items: 20
    brief: Brief
    optional: string
      required: false
    explicit_null: string
      required: false
      nullable: true
      default: null
    empty: string[]
      required: false
      default: []
    structured: json
      required: false
      default: {"id": 9007199254740993, "nested": [true, null, {"path": "a\\b", "label": "une \"citation\""}]}
  outputs:
    report: file
      description: "Rapport livré au projet"
      file:
        media_type: "application/json"
        min_bytes: 2
        schema: Report
  criteria:
    readable:
      kind: min_length
      port: input.brief
      params: {"min": 1}
  effects:
    publish:
      description: "Publication du rapport"
      paid: true

port_policy isolated:
  max_map_items: 20
  effects:
    publish:
      resource: publication
      recovery: verify
      verifier: "check_publication"

compute produce:
  expr:
    result: "1"

workflow product:
  runtime_semantics: "ports-v1"
  contract: Assemble
  graph:
    nodes:
      compose:
        implementation: produce
        contract: Assemble
        policy: isolated
    bindings:
      input.research -> compose.research
      input.brief -> compose.brief
    exports:
      report: compose.report
    products: ["report"]
  budget:
    max_parallel_branches: 2
`

func parseContractDocument(t *testing.T, source string) *ast.File {
	t.Helper()
	r := parser.Parse("ports.bot", source)
	if len(r.Diagnostics) != 0 {
		t.Fatalf("parse: %v\n%s", r.Diagnostics, source)
	}
	return r.File
}

func contractWire(t *testing.T, f *ast.File) []byte {
	t.Helper()
	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPublicContractsSurviveSourceAndEditorRoundTrips(t *testing.T) {
	original := parseContractDocument(t, portsDocument)
	wire := contractWire(t, original)
	edited, err := ast.UnmarshalFile(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []int{0, 2} {
		edited.Profile = profile
		expected := contractWire(t, edited)
		source := unparse.Unparse(edited)
		restored := contractWire(t, parseContractDocument(t, source))
		if !bytes.Equal(expected, restored) {
			t.Fatalf("profile %d changed the complete document\nwant %s\ngot %s\nsource:\n%s", profile, expected, restored, source)
		}
	}
	ports := edited.Contracts[0].Inputs
	if ports[2].Default != nil || string(ports[3].Default) != "null" || string(ports[4].Default) != "[]" {
		t.Fatalf("missing, null and [] were conflated: %#v", ports)
	}
	if !strings.Contains(string(ports[5].Default), "9007199254740993") {
		t.Fatal("structured default lost integer precision")
	}
	if edited.Workflows[0].Graph.Nodes[0].Policy != "isolated" || len(edited.PortPolicies[0].Effects) != 1 {
		t.Fatal("public editing dropped technical policy references")
	}
}

func TestPublicContractSourceDiagnosticsRetainFileAndLine(t *testing.T) {
	tests := []struct {
		name, source string
		code         parser.DiagCode
	}{
		{"unknown port property", "contract c:\n  inputs:\n    item: string\n      requried: false\n", parser.DiagUnknownProperty},
		{"duplicate requiredness", "contract c:\n  inputs:\n    item: string\n      required: false\n      required: true\n", parser.DiagDuplicateBlock},
		{"duplicate JSON key", "contract c:\n  inputs:\n    item: json\n      default: {\"a\": 1, \"a\": 2}\n", parser.DiagDuplicateBlock},
		{"duplicate graph", "workflow w:\n  graph:\n\n  graph:\n", parser.DiagDuplicateBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := parser.Parse("diagnostic.bot", tt.source)
			for _, d := range r.Diagnostics {
				if d.Code == tt.code && d.File == "diagnostic.bot" && d.Line >= 3 {
					return
				}
			}
			t.Fatalf("missing source-located %s: %v", tt.code, r.Diagnostics)
		})
	}
}

func TestPublicContractJSONRejectsNilDeclarations(t *testing.T) {
	for _, raw := range []string{
		`{"contracts":[null]}`,
		`{"contracts":[{"name":"C","inputs":[null]}]}`,
		`{"port_policies":[{"name":"P","effects":[null]}]}`,
		`{"workflows":[{"name":"W","graph":{"nodes":[null]}}]}`,
	} {
		if _, err := ast.UnmarshalFile([]byte(raw)); err == nil {
			t.Fatalf("accepted nil declaration: %s", raw)
		}
	}
	// Explicit null is legal data, including inside an array default.
	raw := `{"contracts":[{"name":"C","inputs":[{"name":"value","type":"json","default":[null]}]}]}`
	f, err := ast.UnmarshalFile([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var value []any
	if err := json.Unmarshal(f.Contracts[0].Inputs[0].Default, &value); err != nil || len(value) != 1 || value[0] != nil {
		t.Fatalf("data null was treated as a missing declaration: %v %v", value, err)
	}
}
