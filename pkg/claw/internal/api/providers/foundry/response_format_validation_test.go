package foundry

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestResponseFormatValidation(t *testing.T) {
	formats := []*api.ResponseFormat{
		{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "bad", Schema: []byte(`{"type":`)}},
		{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "bad", Schema: []byte("{\"description\":\"\xff\"}")}},
		{}, {Type: "xml"}, {Type: "json_schema"},
		{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Schema: []byte(`{}`)}},
		{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "answer"}},
		{Type: "json_schema", JSONSchema: &api.ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`null`)}},
		{Type: "json_object", JSONSchema: &api.ResponseFormatJSONSchema{Name: "answer", Schema: []byte(`{}`)}},
	}
	for _, mode := range []string{"default"} {
		_ = mode
		for _, format := range formats {
			req := api.CreateMessageRequest{ResponseFormat: format}
			var err error
			_, err = (&Client{}).buildRequest(req)
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
