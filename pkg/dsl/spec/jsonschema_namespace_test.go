package spec

import (
	"encoding/json"
	"testing"
)

// A declaration written with its header (a group) and a block property
// whose body names the same kind render as TWO definitions: the header's
// shape under `header.<kind>`, the body's under `<kind>`. On one shared
// name, whichever rendered first would serve both, and a block body would
// silently get the header's shape.
func TestHeaderAndBodyDefinitionsDoNotShareAName(t *testing.T) {
	kinds := []Kind{
		{Name: "group", Role: Declaration, Doc: "A synthetic group.", Header: &Header{Syntax: "group <name>:"}, Properties: []Property{prop("note", String, "d")}},
		{Name: "thing", Role: Node, Doc: "A synthetic node.", Properties: []Property{Property{Name: "inner", Form: Block, Body: "group", Doc: "d"}}},
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
	defs := doc["$defs"].(map[string]any)
	header, ok := defs["header.group"].(map[string]any)
	if !ok {
		t.Fatalf("no header.group definition among %v", keys(defs))
	}
	body, ok := defs["group"].(map[string]any)
	if !ok {
		t.Fatalf("no group body definition among %v", keys(defs))
	}
	if _, named := header["properties"].(map[string]any)["group"]; !named {
		t.Errorf("the header definition does not carry the declaration's name: %v", header)
	}
	if _, named := body["properties"].(map[string]any)["group"]; named {
		t.Errorf("the body definition carries the header's name key: %v", body)
	}
	if _, note := body["properties"].(map[string]any)["note"]; !note {
		t.Errorf("the body definition lacks the kind's own property: %v", body)
	}
	inner := defs["node.thing"].(map[string]any)["properties"].(map[string]any)["inner"].(map[string]any)
	if ref, _ := inner["$ref"].(string); ref != "#/$defs/group" {
		t.Errorf("the block property refers to %q, want the body definition #/$defs/group", ref)
	}
}
