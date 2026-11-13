package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestResponseFormatEnvelopeR2(t *testing.T) {
	for name, raw := range map[string][]byte{"syntax": []byte(`{"type":`), "utf8": []byte("{\"description\":\"\xff\"}")} {
		t.Run(name, func(t *testing.T) {
			if name == "utf8" && !json.Valid(raw) {
				t.Fatal("witness must pass json.Valid")
			}
			f := &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseFormatJSONSchema{Name: "answer", Schema: raw}}
			var invalid *RequestValidationError
			if err := f.Validate(); !errors.As(err, &invalid) || invalid.Field != "ResponseFormat.JSONSchema.Schema" {
				t.Fatalf("want typed schema refusal, got %v", err)
			}
		})
	}
	if err := (&ResponseFormat{Type: "text"}).Validate(); err != nil {
		t.Errorf("text must be accepted: %v", err)
	}
	if err := (&ResponseFormat{Type: "text", JSONSchema: &ResponseFormatJSONSchema{Name: "x", Schema: []byte(`{}`)}}).Validate(); err == nil {
		t.Error("text descriptor must be refused")
	}
	for _, provider := range []string{"anthropic", "bedrock", "vertex"} {
		err := RefuseOpenAIOnlyRequestOptions(provider, CreateMessageRequest{ResponseFormat: &ResponseFormat{Type: "text"}})
		if err == nil || !strings.Contains(err.Error(), "no native response_format") || !strings.Contains(err.Error(), "does not translate") {
			t.Errorf("inexact refusal: %v", err)
		}
	}
}
