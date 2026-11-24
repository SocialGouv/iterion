package permission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// CanonicalApprovalInput normalizes object ordering and whitespace without
// changing numeric text or accepting duplicate keys at any depth.
func CanonicalApprovalInput(raw json.RawMessage) (json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("approval input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readApprovalValue(dec)
	if err != nil {
		return nil, fmt.Errorf("decode approval input: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("approval input must be a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("approval input must contain exactly one JSON object")
	}
	return json.Marshal(v)
}

func readApprovalValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		obj := make(map[string]any)
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("expected approval input object key")
			}
			if _, exists := obj[name]; exists {
				return nil, fmt.Errorf("duplicate key in approval input")
			}
			obj[name], err = readApprovalValue(dec)
			if err != nil {
				return nil, err
			}
		}
		_, err = dec.Token()
		return obj, err
	case json.Delim('['):
		items := make([]any, 0)
		for dec.More() {
			item, err := readApprovalValue(dec)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		_, err = dec.Token()
		return items, err
	default:
		return token, nil
	}
}
