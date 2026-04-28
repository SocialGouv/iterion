package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"encoding/json"
	"fmt"
)

func ConfigTool() api.Tool {
	return api.Tool{
		Name:        "config",
		Description: "Read configuration values. Returns the value for the specified key.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"key": {Type: "string", Description: "The configuration key to read."},
			},
			Required: []string{"key"},
		},
	}
}

// ExecuteConfig reads a config value. The configMap is typically populated from
// ConversationLoop.Config fields exposed as a flat map.
func ExecuteConfig(input map[string]any, configMap map[string]any) (string, error) {
	key, ok := input["key"].(string)
	if !ok || key == "" {
		return "", fmt.Errorf("config: 'key' is required and must be a string")
	}
	if configMap == nil {
		return "", fmt.Errorf("config: no configuration available")
	}
	val, found := configMap[key]
	result := map[string]any{
		"key":   key,
		"found": found,
		"value": val,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}
