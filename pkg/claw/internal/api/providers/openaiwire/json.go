package openaiwire

import (
	"bytes"
	"encoding/json"
)

// MarshalJSON avoids HTML escaping because request bodies are JSON, not HTML.
// This also keeps raw schema strings byte-for-byte, apart from whitespace.
func MarshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}
