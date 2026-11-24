package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Record object order alongside the decoded tree. The cleaner's path routes
// carry this order with schemas moved into prefixItems, dependentSchemas or
// allOf. Instance data and extension objects retain their order too.
// The input must first pass decodeSchemaJSON's syntax and depth checks.
func decodeOrderedSchema(d *json.Decoder, path *schemaPath) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key must be a string")
			}
			if _, exists := m[key]; !exists {
				path.order = append(path.order, key)
			}
			// Match encoding/json's last-value policy for duplicate keys.
			child := &schemaPath{}
			if path.input == nil {
				path.input = map[string]*schemaPath{}
			}
			path.input[key] = child
			v, err := decodeOrderedSchema(d, child)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		_, err := d.Token()
		return m, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decodeOrderedSchema(d, path.inputChild(strconv.Itoa(len(a))))
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := d.Token()
		return a, err
	default:
		return token, nil
	}
}

// Retain original keys in order; append newly synthesized keys deterministically.
func (p *schemaPath) orderedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, key := range p.order {
		if _, exists := m[key]; exists {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	start := len(keys)
	for key := range m {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys[start:])
	return keys
}

func marshalOrderedSchema(value any, path *schemaPath) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	var write func(any, *schemaPath) error
	writeScalar := func(value any) error {
		if err := encoder.Encode(value); err != nil {
			return err
		}
		buf.Truncate(buf.Len() - 1) // Encoder's trailing newline.
		return nil
	}
	// Write into one buffer, rather than re-encoding every descendant subtree.
	write = func(value any, path *schemaPath) error {
		switch v := value.(type) {
		case map[string]any:
			buf.WriteByte('{')
			for i, key := range path.orderedKeys(v) {
				if i > 0 {
					buf.WriteByte(',')
				}
				if err := writeScalar(key); err != nil {
					return err
				}
				buf.WriteByte(':')
				if err := write(v[key], path.child(key)); err != nil {
					return err
				}
			}
			buf.WriteByte('}')
		case []any:
			buf.WriteByte('[')
			for i, child := range v {
				if i > 0 {
					buf.WriteByte(',')
				}
				if err := write(child, path.child(strconv.Itoa(i))); err != nil {
					return err
				}
			}
			buf.WriteByte(']')
		default:
			return writeScalar(v)
		}
		return nil
	}
	if err := write(value, path); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
