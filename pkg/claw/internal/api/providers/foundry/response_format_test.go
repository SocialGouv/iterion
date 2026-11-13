package foundry

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestNativeResponseFormat(t *testing.T) {
	for _, format := range []string{
		`{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":{"additionalProperties":false,"properties":{"n":{"minimum":0,"type":"integer"}},"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"result","strict":false,"schema":{}}}`,
		`{"type":"json_object"}`, `{"type":"text"}`, `null`,
	} {
		t.Run(format, func(t *testing.T) {
			var req api.CreateMessageRequest
			if err := json.Unmarshal([]byte(`{"response_format":`+format+`}`), &req); err != nil {
				t.Fatal(err)
			}
			body, err := (&Client{}).buildRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if format == "null" {
				if len(got["response_format"]) != 0 {
					t.Fatalf("default must omit response_format: %s", body)
				}
			} else if string(got["response_format"]) != format {
				t.Fatalf("response_format lost: got %s, want %s", got["response_format"], format)
			}
		})
	}
}

func TestResponseFormatCleanup(t *testing.T) {
	f := &api.ResponseFormat{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`{"required":null,"minimum":2,"exclusiveMinimum":true}`)}}
	body, err := (&Client{}).buildRequest(api.CreateMessageRequest{ResponseFormat: f})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Format struct {
			JSONSchema struct {
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err = json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	s := env.Format.JSONSchema.Schema
	if _, ok := s["required"]; ok {
		t.Fatalf("required:null reached wire: %s", body)
	}
	if s["exclusiveMinimum"] != float64(2) {
		t.Fatalf("shared schema conversion missing: %s", body)
	}
}
