// Package schema provides JSON Schema normalization helpers for the
// Codex Agent SDK.
package schema

import "sort"

// EnforceStrictMode recursively normalizes a JSON Schema map so it
// satisfies OpenAI's strict structured-output requirements:
//
//  1. Every "type": "object" node must have "additionalProperties": false.
//  2. Every "type": "object" node with "properties" must have a "required"
//     array that lists every property key.
//
// Existing values are preserved — if the caller already set
// additionalProperties or required, the function will not overwrite them.
//
// Ref: https://platform.openai.com/docs/guides/structured-outputs
func EnforceStrictMode(m map[string]any) {
	nodeType, _ := m["type"].(string)
	if nodeType == "object" {
		if _, has := m["additionalProperties"]; !has {
			m["additionalProperties"] = false
		}

		enforceRequiredAll(m)
	}

	recurseMapValues(m, "properties")
	recurseMapValues(m, "$defs")

	if items, ok := m["items"].(map[string]any); ok {
		EnforceStrictMode(items)
	}

	recurseSliceValues(m, "anyOf")
}

// EnforceAdditionalProperties is an alias kept for backward
// compatibility; it calls EnforceStrictMode.
func EnforceAdditionalProperties(m map[string]any) {
	EnforceStrictMode(m)
}

// enforceRequiredAll ensures the "required" array on an object node
// lists every key present in "properties". Missing keys are appended
// in sorted order for deterministic output.
func enforceRequiredAll(m map[string]any) {
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return
	}

	existing := toStringSet(m["required"])

	var missing []string

	for key := range props {
		if !existing[key] {
			missing = append(missing, key)
		}
	}

	if len(missing) == 0 {
		return
	}

	sort.Strings(missing)

	req := toStringSlice(m["required"])
	for _, key := range missing {
		req = append(req, key)
	}

	m["required"] = req
}

// recurseMapValues applies EnforceStrictMode to each map value under
// the given key.
func recurseMapValues(m map[string]any, key string) {
	container, ok := m[key].(map[string]any)
	if !ok {
		return
	}

	for _, v := range container {
		if sub, isMap := v.(map[string]any); isMap {
			EnforceStrictMode(sub)
		}
	}
}

// recurseSliceValues applies EnforceStrictMode to each map element in
// a slice under the given key.
func recurseSliceValues(m map[string]any, key string) {
	arr, ok := m[key].([]any)
	if !ok {
		return
	}

	for _, item := range arr {
		if sub, isMap := item.(map[string]any); isMap {
			EnforceStrictMode(sub)
		}
	}
}

// toStringSet converts a required field value ([]any or []string) into
// a set for fast lookup.
func toStringSet(v any) map[string]bool {
	set := make(map[string]bool, 8)

	switch arr := v.(type) {
	case []any:
		for _, item := range arr {
			if s, ok := item.(string); ok {
				set[s] = true
			}
		}
	case []string:
		for _, s := range arr {
			set[s] = true
		}
	}

	return set
}

// toStringSlice converts a required field value into a []any slice,
// which is the type produced by JSON unmarshalling.
func toStringSlice(v any) []any {
	switch arr := v.(type) {
	case []any:
		return arr
	case []string:
		out := make([]any, len(arr))
		for i, s := range arr {
			out[i] = s
		}

		return out
	default:
		return nil
	}
}
