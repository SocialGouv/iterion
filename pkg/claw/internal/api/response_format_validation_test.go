package api

import (
	"errors"
	"strings"
	"testing"
)

func TestResponseFormatValidation(t *testing.T) {
	formats := []*ResponseFormat{
		{}, {Type: "xml"}, {Type: "json_schema"},
		{Type: "json_schema", JSONSchema: &ResponseFormatJSONSchema{Schema: []byte(`{}`)}},
		{Type: "json_schema", JSONSchema: &ResponseFormatJSONSchema{Name: "answer"}},
		{Type: "json_schema", JSONSchema: &ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`null`)}},
		{Type: "json_object", JSONSchema: &ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`{}`)}},
	}
	formats = append(formats, &ResponseFormat{Type: "json_object"}, &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`{}`)}})
	for _, mode := range []string{"default"} {
		_ = mode
		for _, format := range formats {
			req := CreateMessageRequest{ResponseFormat: format}
			var err error
			_, err = marshalAnthropicRequest(req)
			var invalid interface {
				error
				InvalidField() string
			}
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "ResponseFormat") {
				t.Errorf("%s format %+v: want typed ResponseFormat refusal, got %T: %v", mode, format, err, err)
			}
		}
	}
}
