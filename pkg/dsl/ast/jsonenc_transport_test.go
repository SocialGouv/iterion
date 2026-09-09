package ast_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The JSON codec is the cloud transport (the publisher marshals the parsed
// AST onto the queue, the runner unmarshals and compiles it) and the studio's
// save path. The program that compiles on the runner must be the program the
// author wrote: for every construct, parse → compile and parse → JSON → parse
// → compile must yield the same compiled workflow and the same diagnostics.

// compileBothWays compiles src directly and through the JSON transport.
func compileBothWays(t *testing.T, name, src string) (direct, viaJSON *ir.CompileResult) {
	t.Helper()
	pr := parser.Parse(name, src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("%s: parse error: %s", name, d.Error())
		}
	}
	direct = ir.Compile(pr.File)

	raw, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	restored, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	viaJSON = ir.Compile(restored)
	return direct, viaJSON
}

// The four constructs the codec used to drop: a group and its two `use`
// instantiations (with params), an `as foreach` back-edge, a named
// resource pool beside a counting resource.
const transportFixture = `schema pout:
  id: string
  ok: bool

schema lout:
  items: string[]
  done: bool

group gate_block(label):
  tool gate:
    command: ` + "`printf '{\"id\":\"%s\",\"ok\":true}' \"{{params.label}}\"`" + `
    output: pout
    needs: slot

use gate_block as r1 with { label: "A" }
use gate_block as r2 with { label: "B" }

tool plan:
  command: ` + "`printf '{\"items\":[\"a\",\"b\"],\"done\":false}'`" + `
  output: lout

tool step:
  command: ` + "`printf '{\"items\":[],\"done\":true}'`" + `
  output: lout

workflow transport:
  entry: plan
  resources:
    slot: ["slot-a", "slot-b"]
    cpu: 2
  plan -> r1.gate
  r1.gate -> r2.gate
  r2.gate -> step
  step -> step as foreach scan(item in "{{outputs.plan.items}}")
  step -> done when done
`

func TestTransportCarriesGroupsUsesForeachAndPools(t *testing.T) {
	direct, viaJSON := compileBothWays(t, "transport.bot", transportFixture)
	if direct.HasErrors() {
		t.Fatalf("the fixture must compile directly; got %v", direct.Diagnostics)
	}
	dsltest.AssertSameProgram(t, "transport.bot", direct, viaJSON)

	// And the AST itself keeps the declarations, not only their effect.
	pr := parser.Parse("transport.bot", transportFixture)
	raw, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Groups) != 1 || len(restored.Uses) != 2 {
		t.Errorf("groups/uses did not survive: %d groups, %d uses", len(restored.Groups), len(restored.Uses))
	}
	if g := restored.Groups[0]; len(g.Params) != 1 || g.Params[0] != "label" || len(g.Tools) != 1 {
		t.Errorf("group body did not survive: %+v", g)
	}
	if u := restored.Uses[1]; u.Prefix != "r2" || len(u.With) != 1 || u.With[0].Value != "B" {
		t.Errorf("use binding did not survive: %+v", u)
	}
	var foreach *ast.ForeachClause
	for _, e := range restored.Workflows[0].Edges {
		if e.Foreach != nil {
			foreach = e.Foreach
		}
	}
	if foreach == nil || foreach.Name != "scan" || foreach.Item != "item" || foreach.Collection != "{{outputs.plan.items}}" {
		t.Errorf("foreach clause did not survive: %+v", foreach)
	}
	res := restored.Workflows[0].Resources
	if res == nil || res.Capacities["slot"] != 2 || res.Capacities["cpu"] != 2 || !reflect.DeepEqual(res.Members["slot"], []string{"slot-a", "slot-b"}) || res.Members["cpu"] != nil {
		t.Errorf("resources did not survive: %+v", res)
	}
}

// Every workflow in the repository must compile identically through the
// transport — the guarantee the cloud launch and the studio save rely on.
// Files with a prompt `{{include}}` are excluded: the include resolves from
// the prompt's source path, which the JSON does not carry (#1013).
func TestCorpusSurvivesTheJSONTransport(t *testing.T) {
	checked := 0
	for _, path := range dsltest.CorpusFiles(t, filepath.Join("..", "..", "..")) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), `{{include "`) {
			continue
		}
		pr := parser.Parse(path, string(src))
		skip := false
		for _, pd := range pr.Diagnostics {
			if pd.Severity == parser.SeverityError {
				skip = true // a deliberately broken fixture; not the transport's business
			}
		}
		if skip || pr.File == nil {
			continue
		}
		direct := ir.Compile(pr.File)
		raw, err := ast.MarshalFile(pr.File)
		if err != nil {
			t.Errorf("%s: marshal: %v", path, err)
			continue
		}
		restored, err := ast.UnmarshalFile(raw)
		if err != nil {
			t.Errorf("%s: unmarshal: %v", path, err)
			continue
		}
		dsltest.AssertSameProgram(t, path, direct, ir.Compile(restored))
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d workflows checked — the corpus walk is broken", checked)
	}
	t.Logf("checked %d workflows through the JSON transport", checked)
}
