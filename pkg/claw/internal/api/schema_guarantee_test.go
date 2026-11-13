package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil/schematest"
)

func TestReceivedSchemaGuarantee(t *testing.T) {
	schematest.Run(t, func(raw []byte) ([]byte, error) {
		var s InputSchema
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		req := CreateMessageRequest{Tools: []Tool{{Name: "guarantee", InputSchema: s}}}
		return schematest.ToolSchema(marshalAnthropicRequest(req))
	})
}

func FuzzReceivedSchemaGuarantee(f *testing.F) {
	for _, raw := range []string{`{}`, `{"items":[null],"additionalItems":false}`, `{"dependencies":{"x":{"required":null}}}`, `{"$defs":{"x":null},"allOf":{}}`, `{"exclusiveMinimum":true,"minimum":1e-400}`} {
		f.Add(raw)
	}
	meta := schematest.MetaSchema(f)
	f.Fuzz(func(t *testing.T, raw string) {
		var s InputSchema
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return
		}
		out, err := s.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		// The oracle accepts json.Number, preserving exact decimal arithmetic.
		d := json.NewDecoder(bytes.NewReader(out))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		if err := meta.Validate(v); err != nil {
			t.Fatalf("%s -> %s violates 2020-12: %v", raw, out, err)
		}
	})
}
