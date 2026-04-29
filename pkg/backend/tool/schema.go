package tool

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// SchemaFingerprint computes a deterministic SHA-256 hex digest of a JSON schema.
// The input is unmarshalled and re-marshalled to produce canonical (sorted-key, compact)
// JSON before hashing. Returns an empty string for nil or empty input.
func SchemaFingerprint(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(schema, &v); err != nil {
		h := sha256.Sum256(schema)
		return fmt.Sprintf("%x", h)
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		h := sha256.Sum256(schema)
		return fmt.Sprintf("%x", h)
	}
	h := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", h)
}
