package model

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// publicPortsToJSONSchema projects the resolved contract through the existing
// structured-output seam. Requiredness and nested types cannot be recovered
// from the legacy shallow field list.
func publicPortsToJSONSchema(ports []ir.PublicPort) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(ports))
	for _, port := range ports {
		property := publicTypeToJSONSchema(port.Type)
		if !port.Nullable && port.Type.ArrayDepth == 0 && port.Type.Name == "json" && port.Type.Schema == nil {
			property["type"] = []string{"object", "array", "string", "number", "boolean"}
		}
		if port.Description != "" {
			property["description"] = port.Description
		}
		if port.MinItems != nil {
			property["minItems"] = *port.MinItems
		}
		if port.MaxItems != nil {
			property["maxItems"] = *port.MaxItems
		}
		if port.Nullable {
			// anyOf retains object/array constraints when null is admitted.
			property = map[string]any{"anyOf": []any{property, map[string]any{"type": "null"}}}
		}
		properties[port.Name] = property
		if port.Required {
			required = append(required, port.Name)
		}
	}
	return map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	}
}

func publicTypeToJSONSchema(t ir.PortType) map[string]any {
	if element, array := t.Element(); array {
		return map[string]any{"type": "array", "items": publicTypeToJSONSchema(element)}
	}
	if t.Schema != nil {
		properties := map[string]any{}
		required := make([]string, 0, len(t.Schema.Fields))
		for _, field := range t.Schema.Fields {
			property := fieldToJSONSchema(field)
			if field.Type == ir.FieldTypeJSON {
				property["type"] = []string{"object", "array", "string", "number", "boolean"}
			}
			properties[field.Name] = property
			required = append(required, field.Name)
		}
		return map[string]any{
			"type": "object", "properties": properties, "required": required, "additionalProperties": false,
		}
	}
	switch t.Name {
	case "string":
		return map[string]any{"type": "string"}
	case "bool":
		return map[string]any{"type": "boolean"}
	case "int":
		return map[string]any{"type": "integer"}
	case "float":
		return map[string]any{"type": "number"}
	case "file":
		return map[string]any{"anyOf": []any{
			map[string]any{"type": "string", "minLength": 1},
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string", "minLength": 1}},
				"required":   []string{"path"}, "additionalProperties": false,
			},
		}}
	default:
		return map[string]any{"type": []string{"object", "array", "string", "number", "boolean", "null"}}
	}
}
