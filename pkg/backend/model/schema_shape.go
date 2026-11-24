package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Keywords whose value is a map from names to subschemas.
var schemaMapKeywords = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}

// Keywords whose value is a list of subschemas.
var schemaListKeywords = []string{"allOf", "anyOf", "oneOf", "prefixItems"}

// Keywords whose value is one subschema (an object or a boolean).
var schemaSingleKeywords = []string{
	"additionalProperties", "not", "if", "then", "else", "contains",
	"propertyNames", "unevaluatedItems", "unevaluatedProperties", "additionalItems",
}

// explicitSchemaShapeError reports the first keyword of an explicit output
// schema whose value has the wrong JSON kind, or "" when every schema
// position holds what JSON Schema allows there. claw repairs a received
// schema before it reaches the wire (a wrong-kind container is dropped), so
// a node's declared output schema is checked here, where dropping a
// `properties` that is a string would silently remove the output's shape.
// A keyword set to null counts as absent, as claw drops null keywords.
// Paths are JSON pointers; keys are visited in sorted order, so the report
// is the same on every run.
func explicitSchemaShapeError(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return err.Error()
	}
	if _, ok := root.(map[string]any); !ok {
		return fmt.Sprintf("the schema root is %s, not an object", jsonKind(root))
	}
	return schemaShapeAt(root, "")
}

func schemaShapeAt(node any, path string) string {
	obj, ok := node.(map[string]any)
	if !ok {
		if _, isBool := node.(bool); isBool {
			return ""
		}
		return fmt.Sprintf("%s is %s, not a schema (object or boolean)", pointerOrRoot(path), jsonKind(node))
	}
	for _, kw := range schemaMapKeywords {
		v, present := obj[kw]
		if !present || v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s/%s is %s, not an object of schemas", path, kw, jsonKind(v))
		}
		for _, name := range sortedSchemaKeys(m) {
			if msg := schemaShapeAt(m[name], path+"/"+kw+"/"+escapePointer(name)); msg != "" {
				return msg
			}
		}
	}
	for _, kw := range schemaListKeywords {
		v, present := obj[kw]
		if !present || v == nil {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			return fmt.Sprintf("%s/%s is %s, not an array of schemas", path, kw, jsonKind(v))
		}
		for i, item := range list {
			if msg := schemaShapeAt(item, fmt.Sprintf("%s/%s/%d", path, kw, i)); msg != "" {
				return msg
			}
		}
	}
	for _, kw := range schemaSingleKeywords {
		if v, present := obj[kw]; present && v != nil {
			if msg := schemaShapeAt(v, path+"/"+kw); msg != "" {
				return msg
			}
		}
	}
	if v, present := obj["items"]; present && v != nil {
		// A draft-04 tuple (an array of schemas) is still a readable form.
		if list, isList := v.([]any); isList {
			for i, item := range list {
				if msg := schemaShapeAt(item, fmt.Sprintf("%s/items/%d", path, i)); msg != "" {
					return msg
				}
			}
		} else if msg := schemaShapeAt(v, path+"/items"); msg != "" {
			return msg
		}
	}
	if v, present := obj["required"]; present && v != nil {
		list, ok := v.([]any)
		if !ok {
			return fmt.Sprintf("%s/required is %s, not an array of names", path, jsonKind(v))
		}
		for i, item := range list {
			if _, isString := item.(string); !isString {
				return fmt.Sprintf("%s/required/%d is %s, not a name", path, i, jsonKind(item))
			}
		}
	}
	if v, present := obj["enum"]; present && v != nil {
		if _, ok := v.([]any); !ok {
			return fmt.Sprintf("%s/enum is %s, not an array", path, jsonKind(v))
		}
	}
	if v, present := obj["type"]; present && v != nil {
		switch t := v.(type) {
		case string:
		case []any:
			for i, item := range t {
				if _, isString := item.(string); !isString {
					return fmt.Sprintf("%s/type/%d is %s, not a type name", path, i, jsonKind(item))
				}
			}
		default:
			return fmt.Sprintf("%s/type is %s, not a type name or a list of them", path, jsonKind(v))
		}
	}
	return ""
}

func sortedSchemaKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func escapePointer(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '~':
			b.WriteString("~0")
		case '/':
			b.WriteString("~1")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func pointerOrRoot(path string) string {
	if path == "" {
		return "the schema root"
	}
	return path
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}
