package openaiwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// An MCP server built on zod v4 (firecrawl-mcp's monitor tools) describes a
// z.record as `"propertyNames": {"type": "string"}`. vLLM's xgrammar refuses
// the keyword when claw's first agentic turn forces a tool call
// (`tool_choice: "required"`), failing every request of the node on
// mistral-small and qwen gateways. A propertyNames that admits every key
// constrains nothing — an object key is always a string — so the wire never
// carries one, at any depth; one that does constrain the keys stays.
func TestConvertTools_DropsAPropertyNamesThatConstrainsNothing(t *testing.T) {
	params := convertedParameters(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"id": {"type": "string"},
			"body": {"type": "object", "propertyNames": {"type": "string"}, "additionalProperties": {}},
			"tags": {"type": "object", "propertyNames": {}, "additionalProperties": {"type": "string"}},
			"meta": {"type": "object", "propertyNames": true},
			"labels": {"type": "object", "propertyNames": {"type": ["string", "null"], "description": "a label"}},
			"nested": {"type": "array", "items": {"type": "object", "propertyNames": {"type": "string"}}}
		},
		"required": ["id", "body"],
		"additionalProperties": false
	}`)
	if strings.Contains(params, "propertyNames") {
		t.Errorf("a vacuous propertyNames reached the wire: %s", params)
	}
	if !strings.Contains(params, `"body":{"type":"object","additionalProperties":{}}`) {
		t.Errorf("body lost more than its propertyNames: %s", params)
	}
}

func TestConvertTools_KeepsAPropertyNamesThatConstrainsKeys(t *testing.T) {
	for name, keys := range map[string]string{
		"pattern": `{"type": "string", "pattern": "^[a-z]+$"}`,
		"enum":    `{"enum": ["a", "b"]}`,
		"no key":  `false`,
		"integer": `{"type": "integer"}`,
	} {
		t.Run(name, func(t *testing.T) {
			params := convertedParameters(t, `{"type": "object", "properties": {"m": {"type": "object", "propertyNames": `+keys+`}}}`)
			if !strings.Contains(params, `"propertyNames"`) {
				t.Errorf("a propertyNames that constrains keys was dropped: %s", params)
			}
		})
	}
}

func convertedParameters(t *testing.T, raw string) string {
	t.Helper()
	var s api.InputSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, err := ConvertTools("openai", []api.Tool{{Name: "monitor_update", Description: "test", InputSchema: s}})
	if err != nil {
		t.Fatalf("ConvertTools: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, tools[0].Function.Parameters); err != nil {
		t.Fatal(err)
	}
	return compact.String()
}
