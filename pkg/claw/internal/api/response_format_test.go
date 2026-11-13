package api

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestResponseFormatIsRefusedOnAnthropicWire(t *testing.T) {
	req := CreateMessageRequest{Model: "claude-sonnet-4-20250514", MaxTokens: 1024}
	_, err := marshalAnthropicRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []*ResponseFormat{
		{Type: "json_object"},
		{Type: "json_schema", JSONSchema: &ResponseFormatJSONSchema{
			Name: "result", Strict: true, Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		}},
	} {
		req.ResponseFormat = format
		_, err := marshalAnthropicRequest(req)
		var invalid interface {
			error
			InvalidField() string
		}
		if !errors.As(err, &invalid) {
			t.Fatalf("want typed refusal, got %T: %v", err, err)
		}
	}
}
