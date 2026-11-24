package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func r3WireSchemas(t *testing.T, wire string, body []byte) (tool, response json.RawMessage) {
	t.Helper()
	var env struct {
		Tools []struct {
			Parameters json.RawMessage `json:"parameters"`
			Function   struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ResponseFormat struct {
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Text struct {
			Format struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Tools) != 1 {
		t.Fatalf("want one tool: %s", body)
	}
	if wire == api.OpenAIWireResponses {
		return env.Tools[0].Parameters, env.Text.Format.Schema
	}
	return env.Tools[0].Function.Parameters, env.ResponseFormat.JSONSchema.Schema
}

func TestPrefixItemsHaveItemsOnWire(t *testing.T) {
	for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
		for _, strict := range []bool{false, true} {
			for _, keyword := range []string{"items", "prefixItems"} {
				t.Run(fmt.Sprintf("%s/strict=%v/%s", wire, strict, keyword), func(t *testing.T) {
					raw := []byte(fmt.Sprintf(`{"type":"object","properties":{"pair":{"type":"array","minItems":2,"maxItems":2,%q:[{"type":"string"},{"type":"number"}]}},"required":["pair"],"additionalProperties":false}`, keyword))
					var s api.InputSchema
					if err := json.Unmarshal(raw, &s); err != nil {
						t.Fatal(err)
					}
					body := captureSchemaRequest(t, wire, api.CreateMessageRequest{
						Tools:          []api.Tool{{Name: "tuple", InputSchema: s}},
						ResponseFormat: &api.ResponseFormat{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "tuple", Strict: strict, Schema: raw}},
					})
					tool, response := r3WireSchemas(t, wire, body)
					for name, schema := range map[string]json.RawMessage{"tool": tool, "response": response} {
						var root struct {
							Properties map[string]map[string]json.RawMessage `json:"properties"`
						}
						if err := json.Unmarshal(schema, &root); err != nil {
							t.Fatal(err)
						}
						pair := root.Properties["pair"]
						if string(pair["items"]) != "{}" {
							t.Errorf("%s: array schema missing items:{}: %s", name, schema)
						}
						if !bytes.Equal(pair["prefixItems"], []byte(`[{"type":"string"},{"type":"number"}]`)) {
							t.Errorf("%s: tuple constraints lost: %s", name, schema)
						}
					}
				})
			}
		}
	}
}

func TestNormalizedSchemaKeyOrderOnWire(t *testing.T) {
	for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
		for _, fields := range []string{
			`"reasoning":{"type":"string"},"answer":{"type":"string"}`,
			`"reasoning":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}},"answer":{"items":[{"properties":{"z":{"type":"string"},"a":{"type":"string"}}}],"additionalItems":false}`,
		} {
			t.Run(wire+"/"+fields, func(t *testing.T) {
				raw := []byte(`{"type":"object","properties":{` + fields + `},"required":["reasoning","answer"],"additionalProperties":false,"$schema":"http://json-schema.org/draft-07/schema#"}`)
				format := &api.ResponseFormat{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "answer", Strict: true, Schema: raw}}
				normalized, err := format.Normalized()
				if err != nil {
					t.Fatal(err)
				}
				var s api.InputSchema
				if err := json.Unmarshal(raw, &s); err != nil {
					t.Fatal(err)
				}
				body := captureSchemaRequest(t, wire, api.CreateMessageRequest{Tools: []api.Tool{{Name: "answer", InputSchema: s}}, ResponseFormat: format})
				tool, response := r3WireSchemas(t, wire, body)
				for name, schema := range map[string]json.RawMessage{"normalized": normalized.JSONSchema.Schema, "tool": tool, "response": response} {
					r, a := bytes.Index(schema, []byte(`"reasoning":`)), bytes.Index(schema, []byte(`"answer":`))
					if r < 0 || a < r {
						t.Errorf("%s: reasoning must precede answer: %s", name, schema)
					}
					if bytes.Contains(raw, []byte(`"z":`)) && bytes.Index(schema, []byte(`"z":`)) > bytes.Index(schema, []byte(`"a":`)) {
						t.Errorf("%s: nested object order changed: %s", name, schema)
					}
					if !bytes.Contains(schema, []byte(`"$schema":"https://json-schema.org/draft/2020-12/schema"`)) {
						t.Errorf("%s: dialect was not converted: %s", name, schema)
					}
				}
				if !bytes.Equal(format.JSONSchema.Schema, raw) {
					t.Error("caller schema mutated")
				}
			})
		}
	}
}
