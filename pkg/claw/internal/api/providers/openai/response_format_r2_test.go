package openai

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"testing"
)

func TestResponseFormatCleanupOnWire(t *testing.T) {
	for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
		for _, typ := range []string{"text", "json_schema"} {
			t.Run(wire+"/"+typ, func(t *testing.T) {
				format := &api.ResponseFormat{Type: typ}
				if typ == "json_schema" {
					format.JSONSchema = &api.ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`{"type":"object","required":null,"properties":{"n":{"minimum":0,"exclusiveMinimum":true}}}`)}
				}
				body := captureSchemaRequest(t, wire, api.CreateMessageRequest{ResponseFormat: format})
				var env map[string]any
				if err := json.Unmarshal(body, &env); err != nil {
					t.Fatal(err)
				}
				f, _ := env["response_format"].(map[string]any)
				if wire == api.OpenAIWireResponses {
					f = env["text"].(map[string]any)["format"].(map[string]any)
				}
				if f["type"] != typ {
					t.Fatalf("format lost: %s", body)
				}
				if typ == "text" {
					return
				}
				descriptor := f
				if wire == api.OpenAIWireChat {
					descriptor = f["json_schema"].(map[string]any)
				}
				s := descriptor["schema"].(map[string]any)
				if _, ok := s["required"]; ok {
					t.Errorf("required:null reached wire: %s", body)
				}
				n := s["properties"].(map[string]any)["n"].(map[string]any)
				if n["exclusiveMinimum"] != float64(0) {
					t.Errorf("shared normalization missing: %s", body)
				}
				if string(format.JSONSchema.Schema) != `{"type":"object","required":null,"properties":{"n":{"minimum":0,"exclusiveMinimum":true}}}` {
					t.Error("caller mutated")
				}
			})
		}
	}
}
