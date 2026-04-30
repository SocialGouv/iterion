package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
)

func StructuredOutputTool() api.Tool {
	return api.Tool{
		Name: "structured_output",
		Description: "Return structured data as the final output. " +
			"Pass your structured object under the \"payload\" key " +
			"(e.g. {\"payload\": {\"key\": \"value\"}}); the input is " +
			"echoed back as-is.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"payload": {
					Type:        "object",
					Description: "The structured output object to echo back. Any keys you include will be returned to the caller verbatim.",
				},
			},
			Required: []string{"payload"},
		},
	}
}

func ExecuteStructuredOutput(input map[string]any) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("structured_output: payload must not be empty")
	}
	result := map[string]any{
		"data":              "Structured output provided successfully",
		"structured_output": input,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}
