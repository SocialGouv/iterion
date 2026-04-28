package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"encoding/json"
	"fmt"
)

func StructuredOutputTool() api.Tool {
	return api.Tool{
		Name:        "structured_output",
		Description: "Return structured data as the final output. The input is echoed back as-is. Payload must not be empty.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
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
