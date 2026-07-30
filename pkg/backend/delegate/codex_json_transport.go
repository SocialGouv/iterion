package delegate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const codexJSONTransportDescription = "Transport as a JSON-encoded string: the string contents must be exactly one valid JSON value, including quotes around a JSON string. Examples: {\"key\":\"value\"}, [1,2], \"text\", 42, true, null."

// codexTransportSchema adapts only the canonical schema passed to Codex.
//
// Iterion compiles a DSL FieldTypeJSON as the exact union of all six JSON
// kinds. OpenAI Structured Outputs rejects that union when it contains an
// unconstrained object variant: every object must enumerate its properties and
// set additionalProperties:false, which is incompatible with FieldTypeJSON's
// intentionally unknown shape. For the Codex wire format only, encode those
// fields as strings containing JSON and return their names for post-formatting
// decoding. The Task is passed to CodexBackend.Execute by value, so callers and
// all other backends retain the canonical schema.
func codexTransportSchema(schema json.RawMessage) (json.RawMessage, []string, error) {
	if len(schema) == 0 {
		return nil, nil, nil
	}

	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, nil, fmt.Errorf("parse output schema: %w", err)
	}

	propertiesValue, ok := root["properties"]
	if !ok {
		return append(json.RawMessage(nil), schema...), nil, nil
	}
	properties, ok := propertiesValue.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("output schema properties: expected object, got %T", propertiesValue)
	}

	var jsonFields []string
	for name, propertyValue := range properties {
		property, ok := propertyValue.(map[string]any)
		if !ok || !isAnyJSONTypeUnion(property["type"]) {
			continue
		}

		description := codexJSONTransportDescription
		if existing, ok := property["description"].(string); ok && strings.TrimSpace(existing) != "" {
			description = strings.TrimSpace(existing) + " " + description
		}
		property["type"] = "string"
		property["description"] = description
		jsonFields = append(jsonFields, name)
	}

	if len(jsonFields) == 0 {
		return append(json.RawMessage(nil), schema...), nil, nil
	}
	sort.Strings(jsonFields)

	transport, err := json.Marshal(root)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal codex transport schema: %w", err)
	}
	return transport, jsonFields, nil
}

// isAnyJSONTypeUnion identifies the exact schema emitted for DSL
// FieldTypeJSON. Order is irrelevant, but duplicates, omissions, additions,
// and non-string members prevent a match.
func isAnyJSONTypeUnion(value any) bool {
	types, ok := value.([]any)
	if !ok || len(types) != 6 {
		return false
	}

	wanted := map[string]bool{
		"object":  true,
		"array":   true,
		"string":  true,
		"number":  true,
		"boolean": true,
		"null":    true,
	}
	seen := make(map[string]bool, len(wanted))
	for _, value := range types {
		typeName, ok := value.(string)
		if !ok || !wanted[typeName] || seen[typeName] {
			return false
		}
		seen[typeName] = true
	}
	return len(seen) == len(wanted)
}

// decodeCodexJSONTransport reverses codexTransportSchema after the formatting
// pass. Decoding is atomic: malformed transport in one field leaves every
// field untouched so a retry never receives a half-normalized result.
func decodeCodexJSONTransport(output map[string]any, jsonFields []string) error {
	decoded := make(map[string]any, len(jsonFields))
	for _, name := range jsonFields {
		transportValue, ok := output[name]
		if !ok {
			// Missing required fields remain the model validator's concern.
			continue
		}
		encoded, ok := transportValue.(string)
		if !ok {
			return fmt.Errorf("field %q: expected JSON-encoded string, got %T", name, transportValue)
		}

		var value any
		if err := json.Unmarshal([]byte(encoded), &value); err != nil {
			return fmt.Errorf("field %q: invalid encoded JSON: %w", name, err)
		}
		decoded[name] = value
	}

	for name, value := range decoded {
		output[name] = value
	}
	return nil
}
