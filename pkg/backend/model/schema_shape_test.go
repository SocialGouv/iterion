package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every schema position is checked for its JSON kind: claw repairs a
// wrong-kind container on its way to the wire, so a node's declared output
// schema is refused before that repair could drop its shape.
func TestExplicitSchemaShapeError(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		`{"type":"object","properties":"not-an-object"}`: "/properties is a string",
		`["not","an","object"]`:                          "schema root is an array",
		`{"properties":{"a":"x"}}`:                       "/properties/a is a string",
		`{"properties":{"a/b":{"properties":[]}}}`:       "/properties/a~1b/properties is an array",
		`{"$defs":{"n":{"type":7}}}`:                     "/$defs/n/type is a number",
		`{"anyOf":{}}`:                                   "/anyOf is an object",
		`{"oneOf":[{"type":"string"},3]}`:                "/oneOf/1 is a number",
		`{"items":"x"}`:                                  "/items is a string",
		`{"items":[{"type":"string"},"x"]}`:              "/items/1 is a string",
		`{"additionalProperties":"no"}`:                  "/additionalProperties is a string",
		`{"required":"a"}`:                               "/required is a string",
		`{"required":["a",1]}`:                           "/required/1 is a number",
		`{"enum":"a"}`:                                   "/enum is a string",
		`{"type":["string",1]}`:                          "/type/1 is a number",
		`{"properties":{"p":{"items":{"properties":{"q":{"not":"x"}}}}}}`: "/properties/p/items/properties/q/not is a string",
	}
	for in, want := range refused {
		got := explicitSchemaShapeError(json.RawMessage(in))
		if !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want it to name %q", in, got, want)
		}
	}
	accepted := []string{
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		`{"type":"object","properties":null,"required":null}`,
		`{"type":["string","null"],"enum":["a",null]}`,
		`{"items":[{"type":"string"},true],"additionalItems":false}`,
		`{"anyOf":[true,{"type":"integer"}],"not":false,"$defs":{"n":true}}`,
		`{"type":"object","properties":{"properties":{"type":"string"},"required":{"type":"boolean"}}}`,
	}
	for _, in := range accepted {
		if got := explicitSchemaShapeError(json.RawMessage(in)); got != "" {
			t.Errorf("%s: refused %q, want accepted", in, got)
		}
	}
}
