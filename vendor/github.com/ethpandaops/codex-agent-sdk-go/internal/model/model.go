// Package model defines types for Codex CLI model discovery.
package model

import (
	"encoding/json"
	"fmt"
)

// Info describes a model available from the Codex CLI.
type Info struct {
	ID                        string                  `json:"id"`
	Model                     string                  `json:"model"`
	DisplayName               string                  `json:"displayName"`
	Description               string                  `json:"description"`
	IsDefault                 bool                    `json:"isDefault"`
	Hidden                    bool                    `json:"hidden"`
	DefaultReasoningEffort    string                  `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []ReasoningEffortOption `json:"supportedReasoningEfforts"`
	InputModalities           []string                `json:"inputModalities"`
	SupportsPersonality       bool                    `json:"supportsPersonality"`
	Metadata                  map[string]any          `json:"metadata,omitempty"`
}

// ReasoningEffortOption describes a selectable reasoning effort level.
type ReasoningEffortOption struct {
	Value string `json:"-"`
	Label string `json:"-"`
}

// ListResponse is the response payload from the model/list RPC method.
type ListResponse struct {
	Models   []Info         `json:"models"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// UnmarshalJSON preserves provider-specific model fields under Metadata.
func (i *Info) UnmarshalJSON(data []byte) error {
	type infoAlias Info

	var decoded infoAlias

	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	metadata := decoded.Metadata
	if len(metadata) == 0 {
		metadata = nil
	}

	for key, value := range raw {
		switch key {
		case "id", "model", "displayName", "description", "isDefault", "hidden",
			"defaultReasoningEffort", "supportedReasoningEfforts", "inputModalities",
			"supportsPersonality", "metadata":
			continue
		}

		if metadata == nil {
			metadata = make(map[string]any)
		}

		var decodedValue any
		if err := json.Unmarshal(value, &decodedValue); err != nil {
			return err
		}

		if decodedValue == nil {
			continue
		}

		metadata[key] = decodedValue
	}

	decoded.Metadata = metadata
	*i = Info(decoded)

	return nil
}

// UnmarshalJSON preserves response-level model/list fields under Metadata.
func (r *ListResponse) UnmarshalJSON(data []byte) error {
	type listResponseAlias ListResponse

	var decoded listResponseAlias

	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	metadata := decoded.Metadata
	if len(metadata) == 0 {
		metadata = nil
	}

	for key, value := range raw {
		switch key {
		case "models", "metadata":
			continue
		}

		if metadata == nil {
			metadata = make(map[string]any)
		}

		var decodedValue any
		if err := json.Unmarshal(value, &decodedValue); err != nil {
			return err
		}

		if decodedValue == nil {
			continue
		}

		metadata[key] = decodedValue
	}

	decoded.Metadata = metadata
	*r = ListResponse(decoded)

	return nil
}

// UnmarshalJSON accepts the current app-server payload shape.
func (o *ReasoningEffortOption) UnmarshalJSON(data []byte) error {
	var raw struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Description     string `json:"description"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if raw.ReasoningEffort == "" {
		return fmt.Errorf("reasoning effort option: missing reasoningEffort")
	}

	o.Value = raw.ReasoningEffort

	if raw.Description == "" {
		return fmt.Errorf("reasoning effort option: missing description")
	}

	o.Label = raw.Description

	return nil
}

// MarshalJSON emits the current app-server field names so printed payloads match
// what Codex actually returns.
func (o ReasoningEffortOption) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Description     string `json:"description"`
	}{
		ReasoningEffort: o.Value,
		Description:     o.Label,
	})
}
