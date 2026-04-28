package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"time"
)

const maxSleepDurationMs = 300000 // 5 minutes max (Rust parity)

func SleepTool() api.Tool {
	return api.Tool{
		Name:        "sleep",
		Description: "Pause execution for a specified duration in milliseconds. Maximum 300000ms (5 minutes).",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"duration_ms": {Type: "integer", Description: "Duration to sleep in milliseconds (0 to 300000)."},
			},
			Required: []string{"duration_ms"},
		},
	}
}

func ExecuteSleep(input map[string]any) (string, error) {
	rawMs, ok := input["duration_ms"]
	if !ok {
		return "", fmt.Errorf("sleep: 'duration_ms' is required")
	}
	ms, ok := toInt64(rawMs)
	if !ok || ms < 0 {
		return "", fmt.Errorf("sleep: 'duration_ms' must be a non-negative integer")
	}
	if ms > maxSleepDurationMs {
		return "", fmt.Errorf("sleep: duration_ms %d exceeds maximum of %d ms", ms, maxSleepDurationMs)
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	result := map[string]any{
		"duration_ms": ms,
		"message":     fmt.Sprintf("Slept for %d ms", ms),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// toInt64 converts a JSON number to int64.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}
