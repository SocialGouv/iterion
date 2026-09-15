package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// validateModelOverrides admits a launch's model_overrides array.
//
// Model names are host state that this process cannot enumerate (a valid spec
// may be one the curated list has never heard of). Backend and effort names,
// however, are closed engine enums. Reject typos before they reach the first
// node of a run the operator has already paid to start.
func validateModelOverrides(entries []runview.ModelOverrideEntry) error {
	for _, e := range entries {
		if strings.TrimSpace(e.Selector) == "" {
			return fmt.Errorf("model_overrides: an entry has an empty selector")
		}
		if e.Backend != "" && !validModelBackendNames[e.Backend] {
			return fmt.Errorf(
				"model_overrides: %q is not a backend (valid: %s)",
				e.Backend, strings.Join(validBackendNames(), ", "),
			)
		}
		if e.Effort == "" {
			continue
		}
		if !ir.ValidReasoningEfforts[e.Effort] {
			return fmt.Errorf(
				"model_overrides: %q is not a reasoning effort (valid: %s)",
				e.Effort, strings.Join(validEffortNames(), ", "),
			)
		}
	}
	return nil
}

var validModelBackendNames = map[string]bool{
	delegate.BackendClaw:       true,
	delegate.BackendClaudeCode: true,
	delegate.BackendCodex:      true,
	delegate.BackendKimi:       true,
	delegate.BackendGrok:       true,
	delegate.BackendPi:         true,
}

func validBackendNames() []string {
	out := make([]string, 0, len(validModelBackendNames))
	for k := range validModelBackendNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func validEffortNames() []string {
	out := make([]string, 0, len(ir.ValidReasoningEfforts))
	for k := range ir.ValidReasoningEfforts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
