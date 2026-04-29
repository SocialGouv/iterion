package model

import "encoding/json"

// ContextOverflowError indicates the prompt exceeded the model's context window.
type ContextOverflowError struct {
	Message string
}

func (e *ContextOverflowError) Error() string {
	return e.Message
}

// APIError represents a non-overflow API error.
type APIError struct {
	Message     string
	StatusCode  int
	IsRetryable bool
}

func (e *APIError) Error() string {
	return e.Message
}

// ClassifyStreamError parses a stream error event and returns the appropriate
// typed error (*ContextOverflowError or *APIError), or nil if the data is not
// a recognized error event.
func ClassifyStreamError(body []byte) error {
	var obj struct {
		Type  string `json:"type"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	if obj.Type != "error" {
		return nil
	}

	switch obj.Error.Code {
	case "context_length_exceeded":
		return &ContextOverflowError{Message: "Input exceeds context window of this model"}
	case "insufficient_quota":
		return &APIError{Message: "Quota exceeded. Check your plan and billing details."}
	case "usage_not_included":
		return &APIError{Message: "To use Codex with your ChatGPT plan, upgrade to Plus."}
	case "invalid_prompt":
		msg := "Invalid prompt."
		if obj.Error.Message != "" {
			msg = obj.Error.Message
		}
		return &APIError{Message: msg}
	}

	return nil
}
