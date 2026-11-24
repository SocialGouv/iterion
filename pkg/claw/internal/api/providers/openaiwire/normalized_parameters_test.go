package openaiwire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	api "github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// NormalizedParameters is the single normaliser both OpenAI tool converters
// (chat + responses) route through. It must never emit the three null shapes
// OpenAI's function validator rejects.
func TestNormalizedParameters(t *testing.T) {
	t.Run("nil required is omitted, not null", func(t *testing.T) {
		p, err := NormalizedParameters(api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{"pages": {Type: "array", Items: &api.Property{Type: "string"}}},
			Required:   nil,
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(p), "null") {
			t.Errorf("emitted null: %s", p)
		}
		if strings.Contains(string(p), "\"required\"") {
			t.Errorf("required must be omitted when empty: %s", p)
		}
		if !strings.Contains(string(p), "\"items\"") {
			t.Errorf("array items dropped: %s", p)
		}
	})

	t.Run("empty type defaults to object, nil properties to {}", func(t *testing.T) {
		p, err := NormalizedParameters(api.InputSchema{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(p), "\"type\":\"object\"") {
			t.Errorf("type not defaulted to object: %s", p)
		}
		if !strings.Contains(string(p), "\"properties\":{}") {
			t.Errorf("properties not defaulted to {}: %s", p)
		}
	})

	t.Run("present required survives", func(t *testing.T) {
		p, err := NormalizedParameters(api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{"url": {Type: "string"}},
			Required:   []string{"url"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(p), "\"required\":[\"url\"]") {
			t.Errorf("required lost: %s", p)
		}
	})
}

// Conforming received schemas preserve spelling and explicit empty collections.
// Incomplete roots are normalized without discarding untyped keywords.
func TestNormalizedParameters_ReceivedSchemaIsVerbatim(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{}`, `{"properties":{},"type":"object"}`},
		{`{"properties":{},"required":[]}`, `{"properties":{},"required":[],"type":"object"}`},
		{`{"type":"object","additionalProperties":false}`, `{"additionalProperties":false,"properties":{},"type":"object"}`},
		{`{"type":"object", "properties":{}, "const":9007199254740993}`, `{"type":"object", "properties":{}, "const":9007199254740993}`},
	} {
		var schema api.InputSchema
		if err := json.Unmarshal([]byte(tc.input), &schema); err != nil {
			t.Fatal(err)
		}
		got, err := NormalizedParameters(schema)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
	for _, raw := range []string{`false`, `true`, `[]`, `42`, `"scalar"`} {
		_, err := NormalizedParameters(api.InputSchema{Raw: json.RawMessage(raw)})
		var invalid interface {
			error
			InvalidField() string
		}
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "object") {
			t.Errorf("%s: want typed root-object error, got %T: %v", raw, err, err)
		}
	}
}
