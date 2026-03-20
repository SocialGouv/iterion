package goai

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// schemaMarshalFunc is swappable for testing the panic path.
var schemaMarshalFunc = json.Marshal

// SchemaFrom generates a JSON Schema from a Go type using reflection.
// The schema is compatible with OpenAI strict mode:
//   - All properties are required (pointer types become nullable)
//   - additionalProperties: false on all objects
//
// Supports struct tags:
//   - json:"name" for field naming, json:"-" to skip
//   - jsonschema:"description=...,enum=a|b|c" for descriptions and enums
func SchemaFrom[T any]() json.RawMessage {
	t := reflect.TypeFor[T]()
	// Unwrap pointer types.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	schema := typeToSchema(t)
	// typeToSchema only produces JSON-safe types (map[string]any with string/bool/int/slice values),
	// so json.Marshal cannot fail here. Panic on impossible error to surface bugs in typeToSchema.
	data, err := schemaMarshalFunc(schema)
	if err != nil {
		panic(fmt.Sprintf("goai: SchemaFrom marshal failed (bug in typeToSchema): %v", err))
	}
	return data
}

func typeToSchema(t reflect.Type) map[string]any {
	// Unwrap pointer: nullable type.
	if t.Kind() == reflect.Ptr {
		inner := typeToSchema(t.Elem())
		// Make nullable: type becomes array ["<type>", "null"].
		if baseType, ok := inner["type"].(string); ok {
			inner["type"] = []string{baseType, "null"}
		}
		return inner
	}

	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": typeToSchema(t.Elem())}
	case reflect.Map:
		if t.Key().Kind() == reflect.String {
			return map[string]any{"type": "object", "additionalProperties": typeToSchema(t.Elem())}
		}
		return map[string]any{"type": "object"}
	case reflect.Struct:
		return structToSchema(t)
	default:
		return map[string]any{}
	}
}

func structToSchema(t reflect.Type) map[string]any {
	properties := make(map[string]any)
	var required []string

	collectFields(t, properties, &required)

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// collectFields recursively processes struct fields, flattening embedded structs.
func collectFields(t reflect.Type, properties map[string]any, required *[]string) {
	for i := range t.NumField() {
		field := t.Field(i)

		// Flatten embedded (anonymous) structs.
		if field.Anonymous {
			ft := field.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectFields(ft, properties, required)
				continue
			}
		}

		if !field.IsExported() {
			continue
		}

		name := field.Name

		// Parse json tag.
		if tag := field.Tag.Get("json"); tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
		}

		prop := typeToSchema(field.Type)

		// Parse jsonschema tag.
		if tag := field.Tag.Get("jsonschema"); tag != "" {
			parseSchemaTag(tag, prop)
		}

		properties[name] = prop

		// All properties are required (OpenAI strict mode).
		// Pointer types are already nullable via type: ["<base>", "null"].
		*required = append(*required, name)
	}
}

func parseSchemaTag(tag string, prop map[string]any) {
	for part := range strings.SplitSeq(tag, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key, value := strings.TrimSpace(k), strings.TrimSpace(v)
		switch key {
		case "description":
			prop["description"] = value
		case "enum":
			prop["enum"] = strings.Split(value, "|")
		}
	}
}
