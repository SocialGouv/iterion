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

	properties := make(map[string]interface{})
	required := make([]string, 0, len(schema.Fields))

	for _, f := range schema.Fields {
		properties[f.Name] = fieldToJSONSchema(f)
		required = append(required, f.Name)
	}

	obj := map[string]interface{}{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}

	return json.Marshal(obj)
}

func fieldToJSONSchema(f *ir.SchemaField) map[string]interface{} {
	prop := make(map[string]interface{})

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
		// JSON fields accept any value.
		prop["type"] = "object"
	case ir.FieldTypeStringArray:
		prop["type"] = "array"
		items := map[string]interface{}{"type": "string"}
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
