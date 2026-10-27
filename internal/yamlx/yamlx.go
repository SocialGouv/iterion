// Package yamlx centralizes the repository's yaml.v3 entry points so the
// canonical style lives in one place: strict unmarshaling (unknown fields
// rejected) and two-space marshaling, matching the style of shipped
// manifests.
package yamlx

import (
	"bytes"
	"io"

	"gopkg.in/yaml.v3"
)

// UnmarshalStrict decodes YAML data into out and rejects unknown fields.
// Use it wherever a document may not carry fields the target does not
// declare: the schema drift becomes a caller-visible error instead of a
// silently dropped value. An empty or comment-only document decodes to the
// zero value — the caller's defaults apply — matching what v2's
// UnmarshalStrict returned for the same bytes.
func UnmarshalStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	return nil
}

// Unmarshal decodes YAML data into out, ignoring unknown fields. Use it for
// documents that legitimately carry extension fields.
func Unmarshal(data []byte, out any) error {
	return yaml.Unmarshal(data, out)
}

// Marshal encodes v as YAML with the canonical two-space indent.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
