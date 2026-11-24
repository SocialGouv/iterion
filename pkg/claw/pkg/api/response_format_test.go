package api_test

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func TestPublicResponseFormat(t *testing.T) {
	req := api.CreateMessageRequest{ResponseFormat: &api.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &api.ResponseFormatJSONSchema{
			Name: "result", Strict: false, Schema: json.RawMessage(`{"additionalProperties":false,"type":"object"}`),
		},
	}}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	const want = `{"type":"json_schema","json_schema":{"name":"result","strict":false,"schema":{"additionalProperties":false,"type":"object"}}}`
	if string(got["response_format"]) != want {
		t.Fatalf("public format = %s, want %s", got["response_format"], want)
	}
}
