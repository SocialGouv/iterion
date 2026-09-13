package assistantmission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const MaxProposalActions = 8

type Proposal struct {
	ID     string         `json:"id"`
	Intent string         `json:"intent,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
}

// ParseProposals accepts the generic assistant_actions artifact contract. A
// list is all-or-nothing: one malformed entry rejects the entire artifact.
func ParseProposals(value any) ([]Proposal, error) {
	if raw, ok := value.(string); ok {
		dec := json.NewDecoder(bytes.NewBufferString(raw))
		dec.UseNumber()
		var decoded any
		if err := dec.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("assistant_actions string is not JSON: %w", err)
		}
		value = decoded
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode assistant_actions: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var proposals []Proposal
	if err := dec.Decode(&proposals); err != nil {
		return nil, fmt.Errorf("assistant_actions must be an array of action objects: %w", err)
	}
	if len(proposals) == 0 {
		return nil, nil
	}
	if len(proposals) > MaxProposalActions {
		return nil, fmt.Errorf("assistant_actions contains %d entries; maximum is %d", len(proposals), MaxProposalActions)
	}
	for i := range proposals {
		p := &proposals[i]
		if p.ID == "" {
			return nil, fmt.Errorf("assistant_actions[%d].id is required", i)
		}
		if p.Intent != "" && p.Intent != "suggested" && p.Intent != "explicit" {
			return nil, fmt.Errorf("assistant_actions[%d].intent must be suggested or explicit", i)
		}
		if p.Args == nil {
			p.Args = map[string]any{}
		}
	}
	return proposals, nil
}

func ValidateSingleSupported(proposals []Proposal, allowed map[string]bool) (Proposal, error) {
	if len(proposals) != 1 {
		return Proposal{}, fmt.Errorf("expected exactly one assistant action, got %d", len(proposals))
	}
	if !allowed[proposals[0].ID] {
		return Proposal{}, fmt.Errorf("assistant action %q is outside the mission allowlist", proposals[0].ID)
	}
	if proposals[0].ID != ActionResume && proposals[0].ID != ActionRewind {
		return Proposal{}, errors.New("only run.resume and run.rewind are supported by the server authority")
	}
	return proposals[0], nil
}
