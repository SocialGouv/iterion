package openai

import (
	"strings"
	"testing"

	api "github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// convertToolsToResponses must NOT emit `"required": null` for a tool whose
// input schema has only optional parameters (Required == nil). The OpenAI
// /responses validator rejects null with "None is not of type 'array'", which
// silently killed every such MCP tool (observed live with firecrawl's
// firecrawl_interact / firecrawl_monitor_create, both all-optional). A tool
// WITH required fields must still carry them.
func TestConvertToolsToResponses_OmitsNullRequired(t *testing.T) {
	tools := []api.Tool{
		{
			Name: "all_optional",
			InputSchema: api.InputSchema{
				Type: "object",
				Properties: map[string]api.Property{
					"pages": {Type: "array", Items: &api.Property{Type: "string"}},
					"goal":  {Type: "string"},
				},
				Required: nil, // the firecrawl_interact / monitor_create shape
			},
		},
		{
			Name: "has_required",
			InputSchema: api.InputSchema{
				Type:       "object",
				Properties: map[string]api.Property{"url": {Type: "string"}},
				Required:   []string{"url"},
			},
		},
	}
	out, err := convertToolsToResponses(tools)
	if err != nil {
		t.Fatalf("convertToolsToResponses: %v", err)
	}

	optional := string(out[0].Parameters)
	if strings.Contains(optional, "null") {
		t.Errorf("all_optional params contain null (OpenAI /responses rejects it): %s", optional)
	}
	if strings.Contains(optional, "\"required\"") {
		t.Errorf("all_optional must omit `required` entirely, got: %s", optional)
	}
	// array item schema must survive so OpenAI accepts the array property.
	if !strings.Contains(optional, "\"items\"") {
		t.Errorf("all_optional dropped array `items`: %s", optional)
	}

	required := string(out[1].Parameters)
	if !strings.Contains(required, "\"required\":[\"url\"]") {
		t.Errorf("has_required lost its required list: %s", required)
	}
}
