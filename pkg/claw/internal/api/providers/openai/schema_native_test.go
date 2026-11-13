package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

const losslessToolSchema = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"score": {"type": "integer", "minimum": 0, "maximum": 100},
		"payload": {"type": "object", "additionalProperties": false,
			"properties": {"label": {"type": "string", "pattern": "^[A-Z]+$", "minLength": 1}},
			"required": ["label"]},
		"rows": {"type": "array", "minItems": 1, "items": {
			"type": "object", "additionalProperties": false,
			"properties": {"value": {"type": ["number", "null"], "minimum": -1.20e+3}},
			"required": ["value"]}},
		"id": {"const": 9007199254740993}
	},
	"required": ["score", "payload", "rows"],
	"$defs": {"tag": {"enum": ["A", "B"]}},
	"allOf": [{"if": {"required": ["id"]}, "then": {"properties": {"score": {"maximum": 50}}}}],
	"x-extension": {"keep": true, "text": "<tag>&\u0041"}
}`

// Capture the serialized HTTP body so a conversion-only test cannot hide a
// later loss in the request envelope. No endpoint or credentials are needed.
func captureSchemaRequest(t *testing.T, wire string, req api.CreateMessageRequest) []byte {
	t.Helper()
	var body []byte
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "test", BaseURL: "https://gateway.example", Model: "scaleway/deepseek-v4.1-flash",
		OpenAIWireAPI: wire, OpenAIModelVerbatim: true, NoAmbientHeaders: true,
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			sse := "data: [DONE]\n\n"
			if wire == api.OpenAIWireResponses {
				sse = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(sse))}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, req)
	if len(body) == 0 {
		t.Fatal("no HTTP request captured")
	}
	return body
}

func compactSchemaJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestToolSchemaVerbatimOnWire(t *testing.T) {
	var schema api.InputSchema
	if err := json.Unmarshal([]byte(losslessToolSchema), &schema); err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
		t.Run(wire, func(t *testing.T) {
			body := captureSchemaRequest(t, wire, api.CreateMessageRequest{
				Tools:      []api.Tool{{Name: "structured_output", InputSchema: schema}},
				ToolChoice: &api.ToolChoice{Type: "tool", Name: "structured_output"},
			})
			var got struct {
				Tools []struct {
					Parameters json.RawMessage `json:"parameters"`
					Function   struct {
						Parameters json.RawMessage `json:"parameters"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Tools) != 1 {
				t.Fatalf("tools = %s, want one tool", body)
			}
			params := got.Tools[0].Function.Parameters
			if wire == api.OpenAIWireResponses {
				params = got.Tools[0].Parameters
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(params, &root); err != nil {
				t.Fatal(err)
			}
			if string(root["additionalProperties"]) != "false" {
				t.Fatalf("emitted schema lost additionalProperties:false: %s", params)
			}
			if !bytes.Equal(compactSchemaJSON(t, params), compactSchemaJSON(t, []byte(losslessToolSchema))) {
				t.Fatalf("schema changed on the wire:\ngot  %s\nwant %s", params, compactSchemaJSON(t, []byte(losslessToolSchema)))
			}
		})
	}
}

func TestNativeResponseFormatOnWire(t *testing.T) {
	for i, format := range []string{
		`{"type":"json_schema","json_schema":{"name":"analysis","strict":true,"schema":` + losslessToolSchema + `}}`,
		`{"type":"json_schema","json_schema":{"name":"analysis","strict":false,"schema":` + losslessToolSchema + `}}`,
		`{"type":"json_object"}`,
	} {
		t.Run([]string{"strict", "non-strict", "legacy"}[i], func(t *testing.T) {
			var req api.CreateMessageRequest
			if err := json.Unmarshal([]byte(`{"model":"scaleway/deepseek-v4.1-flash","response_format":`+format+`}`), &req); err != nil {
				t.Fatal(err)
			}
			body := captureSchemaRequest(t, api.OpenAIWireChat, req)
			var got map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if len(got["response_format"]) == 0 {
				t.Fatalf("serialized request is missing response_format: %s", body)
			}
			if !bytes.Equal(compactSchemaJSON(t, got["response_format"]), compactSchemaJSON(t, []byte(format))) {
				t.Fatalf("response_format changed: got %s, want %s", got["response_format"], format)
			}
			if _, ok := got["tools"]; ok {
				t.Fatalf("native output unexpectedly synthesizes tools: %s", body)
			}
		})
	}
}

func TestNativeResponseFormatOnResponsesWire(t *testing.T) {
	for i, format := range []string{
		`{"type":"json_schema","json_schema":{"name":"analysis","strict":true,"schema":` + losslessToolSchema + `}}`,
		`{"type":"json_schema","json_schema":{"name":"analysis","strict":false,"schema":` + losslessToolSchema + `}}`,
		`{"type":"json_object"}`,
	} {
		t.Run([]string{"strict", "non-strict", "legacy"}[i], func(t *testing.T) {
			var req api.CreateMessageRequest
			if err := json.Unmarshal([]byte(`{"response_format":`+format+`}`), &req); err != nil {
				t.Fatal(err)
			}
			body := captureSchemaRequest(t, api.OpenAIWireResponses, req)
			var got struct {
				Text struct {
					Format map[string]json.RawMessage `json:"format"`
				} `json:"text"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Text.Format) == 0 {
				t.Fatalf("serialized Responses request is missing text.format: %s", body)
			}
			if i == 2 {
				if len(got.Text.Format) != 1 || string(got.Text.Format["type"]) != `"json_object"` {
					t.Fatalf("legacy format = %s", body)
				}
				return
			}
			f := got.Text.Format
			if string(f["type"]) != `"json_schema"` || string(f["name"]) != `"analysis"` || string(f["strict"]) != []string{"true", "false"}[i] {
				t.Fatalf("schema format metadata lost: %s", body)
			}
			if !bytes.Equal(compactSchemaJSON(t, f["schema"]), compactSchemaJSON(t, []byte(losslessToolSchema))) {
				t.Fatalf("native Responses schema changed: %s", body)
			}
		})
	}
}

func TestNativeResponseFormatOptIn(t *testing.T) {
	for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
		t.Run(wire, func(t *testing.T) {
			req := api.CreateMessageRequest{
				Tools:      []api.Tool{{Name: "structured_output", InputSchema: api.InputSchema{Type: "object"}}},
				ToolChoice: &api.ToolChoice{Type: "tool", Name: "structured_output"},
			}
			body := captureSchemaRequest(t, wire, req)
			var got map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if len(got["response_format"]) != 0 || len(got["text"]) != 0 {
				t.Fatalf("native response format must be opt-in: %s", body)
			}
			if len(got["tools"]) == 0 || len(got["tool_choice"]) == 0 {
				t.Fatalf("existing tool path changed: %s", body)
			}
		})
	}
}
