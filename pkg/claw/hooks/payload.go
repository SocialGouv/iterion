package hooks

import "encoding/json"

// BuildPayload constructs the JSON payload sent to hook stdin.
// For PostToolUseFailure: uses "tool_error" key instead of "tool_output", forces tool_result_is_error=true.
// For others: uses "tool_output" key, includes tool_result_is_error flag.
// Always includes: hook_event_name, tool_name, tool_input (parsed JSON), tool_input_json (raw string).
func BuildPayload(event HookEvent, toolName, toolInput string, toolOutput *string, isError bool) []byte {
	payload := map[string]interface{}{
		"hook_event_name": string(event),
		"tool_name":       toolName,
		"tool_input_json": toolInput,
	}

	// Parse tool_input as JSON; fall back to {"raw": toolInput} if invalid,
	// matching Rust parse_tool_input() behavior.
	var parsed interface{}
	if err := json.Unmarshal([]byte(toolInput), &parsed); err != nil {
		payload["tool_input"] = map[string]interface{}{"raw": toolInput}
	} else {
		payload["tool_input"] = parsed
	}

	if event == PostToolUseFailure {
		if toolOutput != nil {
			payload["tool_error"] = *toolOutput
		} else {
			payload["tool_error"] = nil // Rust serializes None as JSON null
		}
		payload["tool_result_is_error"] = true
	} else {
		if toolOutput != nil {
			payload["tool_output"] = *toolOutput
		} else {
			payload["tool_output"] = nil // Rust serializes None as JSON null
		}
		payload["tool_result_is_error"] = isError
	}

	data, _ := json.Marshal(payload)
	return data
}
