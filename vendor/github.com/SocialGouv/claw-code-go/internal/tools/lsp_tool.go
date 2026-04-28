package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/lsp"
)

func LspTool() api.Tool {
	return api.Tool{
		Name:        "lsp",
		Description: "Dispatch an LSP action (diagnostics, hover, definition, references, completion, symbols, format).",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"action":    {Type: "string", Description: "The LSP action to perform."},
				"path":      {Type: "string", Description: "File path for the action."},
				"line":      {Type: "number", Description: "Line number (0-based)."},
				"character": {Type: "number", Description: "Character offset (0-based)."},
				"query":     {Type: "string", Description: "Query string for search actions."},
			},
			Required: []string{"action"},
		},
	}
}

func ExecuteLSP(input map[string]any, registry *lsp.Registry) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("lsp: registry not available")
	}

	action, ok := input["action"].(string)
	if !ok || action == "" {
		return "", fmt.Errorf("lsp: 'action' is required")
	}

	var path *string
	if p, ok := input["path"].(string); ok {
		path = &p
	}

	var line *uint32
	if v, ok := input["line"].(float64); ok {
		l := uint32(v)
		line = &l
	}

	var character *uint32
	if v, ok := input["character"].(float64); ok {
		c := uint32(v)
		character = &c
	}

	var query *string
	if q, ok := input["query"].(string); ok {
		query = &q
	}

	result, err := registry.Dispatch(action, path, line, character, query)
	if err != nil {
		// Rust returns a structured error JSON, not an error.
		errResult := map[string]any{
			"action": action,
			"error":  err.Error(),
			"status": "error",
		}
		out, _ := json.MarshalIndent(errResult, "", "  ")
		return string(out), nil
	}

	// Pretty-print the raw JSON from Dispatch.
	var parsed any
	if json.Unmarshal(result, &parsed) == nil {
		out, _ := json.MarshalIndent(parsed, "", "  ")
		return string(out), nil
	}
	return string(result), nil
}
