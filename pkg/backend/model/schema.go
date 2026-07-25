package model

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// SchemaToJSON converts an IR Schema to a JSON Schema (json.RawMessage)
// suitable for use with sdk.WithExplicitSchema.
func SchemaToJSON(schema *ir.Schema) (json.RawMessage, error) {
	if schema == nil {
		return nil, fmt.Errorf("model: nil schema")
	}

	properties := make(map[string]any)
	required := make([]string, 0, len(schema.Fields))

	for _, f := range schema.Fields {
		properties[f.Name] = fieldToJSONSchema(f)
		required = append(required, f.Name)
	}

	obj := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}

	return json.Marshal(obj)
}

func fieldToJSONSchema(f *ir.SchemaField) map[string]any {
	prop := make(map[string]any)

	switch f.Type {
	case ir.FieldTypeString:
		prop["type"] = "string"
	case ir.FieldTypeBool:
		prop["type"] = "boolean"
	case ir.FieldTypeInt:
		prop["type"] = "integer"
	case ir.FieldTypeFloat:
		prop["type"] = "number"
	case ir.FieldTypeJSON:
		// JSON fields accept any value. JSON Schema's canonical "any" is
		// the empty schema {}, but that has no "type" key — and some
		// structured-output providers (OpenAI/codex's formatting pass)
		// reject a type-less property with `invalid_json_schema: schema
		// must have a 'type' key`, failing the whole node. Emitting a
		// single "type" (e.g. "object") is the opposite failure: it
		// silently rejects the other JSON shapes (the earlier live bug
		// stripped an array-valued `ecosystems: json` to nothing).
		// A "type" *union* over every JSON kind satisfies the type-key
		// requirement while still accepting arrays/objects/strings/
		// numbers/booleans/nulls — the true "any" contract of FieldTypeJSON.
		prop["type"] = []string{"object", "array", "string", "number", "boolean", "null"}
		return prop
	case ir.FieldTypeStringArray:
		prop["type"] = "array"
		items := map[string]any{"type": "string"}
		if len(f.EnumValues) > 0 {
			items["enum"] = f.EnumValues
		}
		prop["items"] = items
		// Early return: for string arrays, enum constraints belong on the items
		// schema, not the array itself. The general enum block below would
		// incorrectly place enum on the array type.
		return prop
	}

	if len(f.EnumValues) > 0 {
		prop["enum"] = f.EnumValues
	}

	return prop
}
