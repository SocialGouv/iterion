package spec

import (
	"encoding/json"
	"reflect"
	"testing"
)

// An enum|env property takes any string (a run-time value in quotes), so its
// fragment carries no `enum` — but its words travel as `examples`, an
// annotation an editor completes on and a validator ignores.
func TestAnEnumOrEnvFragmentKeepsItsWordsAsExamples(t *testing.T) {
	kinds := []Kind{
		{Name: "thing", Role: Node, Doc: "A synthetic node.", Properties: []Property{{Name: "effort", Form: EnumOrEnv, Values: []string{"low", "high"}, Doc: "d"}}},
		{Name: "workflow", Role: Declaration, Edges: true, Properties: []Property{prop("entry", Ident, "x")}},
	}
	raw, err := renderSchemaFor(kinds, 2)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	effort := doc["$defs"].(map[string]any)["node.thing"].(map[string]any)["properties"].(map[string]any)["effort"].(map[string]any)
	if effort["type"] != "string" {
		t.Errorf("the fragment's type is %v, want string", effort["type"])
	}
	if _, refuses := effort["enum"]; refuses {
		t.Errorf("the fragment carries an enum, which would refuse a run-time value: %v", effort)
	}
	if got := effort["examples"]; !reflect.DeepEqual(got, []any{"low", "high"}) {
		t.Errorf("the fragment's examples are %v, want the property's words", got)
	}
}
