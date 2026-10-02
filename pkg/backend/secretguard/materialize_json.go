package secretguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaterializeJSON materializes the placeholders inside the string values of a
// JSON document, never in its text: a secret value carrying a quote stays the
// one string it replaces, instead of rewriting the document around it — a
// placeholder in one field of an agent's tool input must never set another.
// Keys are left as they are, numbers keep their text. Anything that is not
// exactly one JSON document — a parse error, or a second value after the
// first — is refused, never materialized as text.
func MaterializeJSON(raw []byte, materialize func(string) string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("secretguard: materialize a tool input that is not JSON: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("secretguard: materialize a tool input that carries more than one JSON document (%v)", err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(MaterializeValue(doc, materialize)); err != nil {
		return nil, fmt.Errorf("secretguard: re-encode the materialized tool input: %w", err)
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// MaterializeValue is MaterializeJSON over a decoded value: every string leaf
// materialized, the structure untouched. Never mutates v.
func MaterializeValue(v any, materialize func(string) string) any {
	switch t := v.(type) {
	case string:
		return materialize(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = MaterializeValue(vv, materialize)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = MaterializeValue(vv, materialize)
		}
		return out
	default:
		return v
	}
}
