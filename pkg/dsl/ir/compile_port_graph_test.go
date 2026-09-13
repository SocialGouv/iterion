package ir

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const nativeMapDocument = `dsl: 2
contract Batch:
  display_name: "Render a batch"
  responsibility: "Publish all rendered values"
  inputs:
    items: string[]
      min_items: 0
      max_items: 4
    prefix: string
      required: false
      default: "item-"
  outputs:
    results: string[]
contract Render:
  display_name: "Render an item"
  responsibility: "Prefix one item"
  inputs:
    item: string
    prefix: string
  outputs:
    text: string
contract Collect:
  display_name: "Collect results"
  responsibility: "Receive the complete batch"
  inputs:
    texts: string[]
    note: string
      required: false
  outputs:
    texts: string[]
port_policy limited:
  max_map_items: 4
compute render_impl:
  expr:
    text: "input.prefix + input.item"
compute collect_impl:
  expr:
    texts: "input.texts"
workflow batch:
  runtime_semantics: "ports-v1"
  contract: Batch
  port_policy: limited
  graph:
    nodes:
      render:
        implementation: render_impl
        contract: Render
      collect:
        implementation: collect_impl
        contract: Collect
    bindings:
      input.items -> render.item
      input.prefix -> render.prefix
      render.text -> collect.texts
    exports:
      results: collect.texts
    products: ["results"]
`

func compileNativeTest(t *testing.T, text string) (*ast.File, *CompileResult) {
	t.Helper()
	parsed := parser.Parse("native.bot", text)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse: %v", parsed.Diagnostics)
	}
	return parsed.File, Compile(parsed.File)
}

func TestPortGraphCompilesMapBroadcastAndWholeArrayCollection(t *testing.T) {
	file, result := compileNativeTest(t, nativeMapDocument)
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	w := result.Workflow
	if w.RuntimeSemantics != RuntimeSemanticsPortsV1 || w.Entry != "" || len(w.Edges) != 0 {
		t.Fatal("native semantics were translated into legacy control flow")
	}
	if len(w.Nodes) != 2 || w.Nodes["render"].NodeID() != "render" || w.Nodes["render_impl"] != nil {
		t.Fatal("graph instances were not separated from technical declarations")
	}
	if w.Ports.Nodes["render"].MapInput != "item" || w.Ports.Nodes["collect"].MapInput != "" {
		t.Fatal("map inference did not distinguish T[] -> T from T[] -> T[]")
	}
	if w.Ports.Nodes["render"].OutputTypes["text"].String() != "string[]" ||
		w.Ports.Nodes["render"].Policy.MaxMapItems != 4 {
		t.Fatal("map output lifting or workflow policy inheritance was lost")
	}
	if got := w.Ports.Nodes["collect"].Dependencies; len(got) != 1 || got[0] != "render" {
		t.Fatalf("collector dependencies: %v", got)
	}
	if _, connected := w.Ports.Nodes["collect"].Inputs["note"]; connected {
		t.Fatal("unconnected optional input acquired an implicit supplier")
	}
	if len(w.Ports.Products) != 1 || w.Ports.Products[0] != "results" || w.PublicContract.Identity == "" || w.Ports.Identity == "" {
		t.Fatal("public products or stable compiled identities are missing")
	}
	before, err := ast.MarshalFile(file)
	if err != nil {
		t.Fatal(err)
	}
	second := Compile(file)
	after, err := ast.MarshalFile(file)
	if err != nil || !bytes.Equal(before, after) || second.Workflow.Ports.Identity != w.Ports.Identity {
		t.Fatal("compilation mutated source declarations or changed identity on a repeated compile")
	}
}

func TestPortGraphRejectsInvalidBindingsAndExportsWithPositions(t *testing.T) {
	tests := []struct {
		name, source string
		code         DiagCode
	}{
		{"missing supplier", strings.Replace(nativeMapDocument, "      input.items -> render.item\n", "", 1), DiagPortSupplier},
		{"duplicate supplier", strings.Replace(nativeMapDocument, "      input.prefix -> render.prefix", "      input.prefix -> render.prefix\n      input.prefix -> render.prefix", 1), DiagPortSupplier},
		{"unknown consumer", strings.Replace(nativeMapDocument, "render.item", "render.missing", 1), DiagPortReference},
		{"unknown producer", strings.Replace(nativeMapDocument, "input.items ->", "missing.items ->", 1), DiagPortReference},
		{"incompatible type", strings.Replace(nativeMapDocument, "    item: string", "    item: int", 1), DiagPortCompatibility},
		{"ambiguous map", strings.Replace(nativeMapDocument, "    prefix: string\n      required: false\n      default: \"item-\"", "    prefix: string[]", 1), DiagPortMapAxes},
		{"missing export", strings.Replace(nativeMapDocument, "    exports:\n      results: collect.texts\n", "", 1), DiagPortExport},
		{"unexported product", strings.Replace(nativeMapDocument, `products: ["results"]`, `products: ["missing"]`, 1), DiagPortExport},
		{"duplicate instance", strings.Replace(nativeMapDocument, "      collect:\n", "      render:\n", 1), DiagPortReference},
		{"unknown implementation", strings.Replace(nativeMapDocument, "implementation: render_impl", "implementation: missing", 1), DiagPortImplementation},
		{"missing opt in", strings.Replace(nativeMapDocument, "  runtime_semantics: \"ports-v1\"\n", "", 1), DiagRuntimeSemantics},
		{"unknown semantics", strings.Replace(nativeMapDocument, "ports-v1", "ports-v99", 1), DiagRuntimeSemantics},
		{"legacy control edge", strings.Replace(nativeMapDocument, "  contract: Batch", "  entry: render_impl\n  render_impl -> done\n  contract: Batch", 1), DiagPortControl},
		{"hidden input", strings.Replace(nativeMapDocument, "input.prefix + input.item", "input.hidden", 1), DiagPortHiddenInput},
		{"hidden predecessor output", strings.Replace(nativeMapDocument, "input.prefix + input.item", "outputs.collect.texts", 1), DiagPortHiddenInput},
		{"invalid default", strings.Replace(nativeMapDocument, `default: "item-"`, "default: null", 1), DiagPublicPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, result := compileNativeTest(t, tt.source)
			for _, d := range result.Diagnostics {
				if d.Code == tt.code && d.Severity == SeverityError && d.File == "native.bot" && d.Line > 0 && d.Hint != "" {
					return
				}
			}
			t.Fatalf("missing located %s: %v", tt.code, result.Diagnostics)
		})
	}
}

func TestPortGraphJSONCannotBypassPublicDeclarationValidation(t *testing.T) {
	file, _ := compileNativeTest(t, nativeMapDocument)
	file.Contracts[1].Inputs[0].Name = "_run_id"
	file.Contracts = append(file.Contracts, file.Contracts[1])
	b, err := ast.MarshalFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := ast.UnmarshalFile(b)
	if err != nil {
		t.Fatal(err)
	}
	result := Compile(fromJSON)
	codes := map[DiagCode]bool{}
	for _, diagnostic := range result.Diagnostics {
		codes[diagnostic.Code] = true
	}
	if !codes[DiagPublicPort] || !codes[DiagPublicContract] {
		t.Fatalf("JSON bypassed identifier or duplicate checks: %v", result.Diagnostics)
	}
}

func TestPortGraphReusesAnAxisWhenOneArrayFeedsTwoScalarPorts(t *testing.T) {
	source := strings.Replace(nativeMapDocument, "      input.prefix -> render.prefix", "      input.items -> render.prefix", 1)
	_, result := compileNativeTest(t, source)
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	node := result.Workflow.Ports.Nodes["render"]
	if len(node.MapInputs) != 2 || node.Inputs[node.MapInputs[0]] != node.Inputs[node.MapInputs[1]] {
		t.Fatalf("one array source became two independent dimensions: %#v", node)
	}
}

func TestPortGraphDetectsCyclesBeforeExecution(t *testing.T) {
	source := strings.Replace(nativeMapDocument, "      input.items -> render.item", "      collect.texts -> render.item", 1)
	_, result := compileNativeTest(t, source)
	for _, d := range result.Diagnostics {
		if d.Code == DiagPortCycle {
			return
		}
	}
	t.Fatal(result.Diagnostics)
}

func TestLegacyWorkflowKeepsControlSemantics(t *testing.T) {
	_, result := compileNativeTest(t, "compute first:\n  expr:\n    value: \"1\"\nworkflow legacy:\n  entry: first\n  first -> done\n")
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	w := result.Workflow
	if w.RuntimeSemantics != "" || w.Ports != nil || w.PublicContract != nil || w.Entry != "first" || len(w.Edges) != 1 || len(w.Nodes) != 3 {
		t.Fatalf("legacy control IR changed: %#v", w)
	}
}

func TestPortGraphCompilesCrossedDependenciesWithoutControlJoins(t *testing.T) {
	source := `contract Root:
  display_name: "Combine"
  responsibility: "Combine a crossed dependency graph"
  inputs:
    seed: string
  outputs:
    result: string
contract Unary:
  display_name: "Copy"
  responsibility: "Copy a value"
  inputs:
    seed: string
  outputs:
    value: string
contract Binary:
  display_name: "Combine two inputs"
  responsibility: "Concatenate both values"
  inputs:
    left: string
    right: string
  outputs:
    value: string
compute copy_impl:
  expr:
    value: "input.seed"
compute combine_impl:
  expr:
    value: "input.left + input.right"
workflow crossed:
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      a:
        implementation: copy_impl
        contract: Unary
      b:
        implementation: copy_impl
        contract: Unary
      c:
        implementation: combine_impl
        contract: Binary
      d:
        implementation: combine_impl
        contract: Binary
      e:
        implementation: combine_impl
        contract: Binary
      f:
        implementation: combine_impl
        contract: Binary
    bindings:
      input.seed -> a.seed
      input.seed -> b.seed
      a.value -> c.left
      b.value -> c.right
      a.value -> d.left
      c.value -> d.right
      b.value -> e.left
      c.value -> e.right
      d.value -> f.left
      e.value -> f.right
    exports:
      result: f.value
`
	_, result := compileNativeTest(t, source)
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	g := result.Workflow.Ports
	expected := map[string]string{"a": "", "b": "", "c": "a,b", "d": "a,c", "e": "b,c", "f": "d,e"}
	for id, deps := range expected {
		if strings.Join(g.Nodes[id].Dependencies, ",") != deps {
			t.Fatalf("%s dependencies = %v, want %s", id, g.Nodes[id].Dependencies, deps)
		}
	}
	if len(result.Workflow.Edges) != 0 || len(g.Order) != 6 {
		t.Fatal("crossed DAG was flattened into control edges or lost instances")
	}
	if result.Workflow.Nodes["a"] == result.Workflow.Nodes["b"] {
		t.Fatal("two instances reused the same mutable node identity")
	}
}

func TestPortGraphProductCanAlsoFeedAConsumer(t *testing.T) {
	source := `contract Root:
  display_name: "Deliver a report"
  responsibility: "Deliver the report used by the next step"
  inputs:
    value: string
  outputs:
    report: file
contract Produce:
  display_name: "Write report"
  responsibility: "Produce a file"
  inputs:
    value: string
  outputs:
    report: file
      file:
        min_bytes: 1
contract Consume:
  display_name: "Read report"
  responsibility: "Use a produced report"
  inputs:
    report: file
  outputs:
    result: string
tool produce_impl:
  command: "fixture-producer"
tool consume_impl:
  command: "fixture-consumer"
workflow deliver:
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      produce:
        implementation: produce_impl
        contract: Produce
      consume:
        implementation: consume_impl
        contract: Consume
    bindings:
      input.value -> produce.value
      produce.report -> consume.report
    exports:
      report: produce.report
    products: ["report"]
`
	_, result := compileNativeTest(t, source)
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	g := result.Workflow.Ports
	if g.Exports["report"] != g.Nodes["consume"].Inputs["report"] {
		t.Fatal("product export and consumer do not point to the same output")
	}
	if len(g.Products) != 1 || g.Products[0] != "report" {
		t.Fatal("unconsumed consume.result became an implicit product")
	}
}
