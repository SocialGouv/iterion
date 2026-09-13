package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DecodePublicJSON accepts one JSON value, preserving numbers and rejecting
// duplicate object members. The editor JSON and .bot transports must not
// assign different meanings to the same public default or criterion.
func DecodePublicJSON(raw []byte) (any, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid public JSON value")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return decodePublicJSONValue(d)
}

func decodePublicJSONValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("public JSON object requires a string key")
			}
			if _, present := object[key]; present {
				return nil, fmt.Errorf("duplicate public JSON member %q", key)
			}
			value, err := decodePublicJSONValue(d)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		_, err = d.Token()
		return object, err
	case json.Delim('['):
		array := []any{}
		for d.More() {
			value, err := decodePublicJSONValue(d)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err = d.Token()
		return array, err
	default:
		return token, nil
	}
}
