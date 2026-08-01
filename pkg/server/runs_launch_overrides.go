package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// validateModelOverrides admits a launch's model_overrides array.
//
// Only the effort dimension is checked against a closed set: model and backend
// names are host state that this process cannot enumerate (a valid spec may be
// one the curated list has never heard of), whereas reasoning_effort is a fixed
// enum that reaches the provider verbatim. An unknown level would otherwise
// surface as an API error on the first node of a run the operator has already
// paid to start.
func validateModelOverrides(entries []runview.ModelOverrideEntry) error {
	for _, e := range entries {
		if strings.TrimSpace(e.Selector) == "" {
			return fmt.Errorf("model_overrides: an entry has an empty selector")
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

func validEffortNames() []string {
	out := make([]string, 0, len(ir.ValidReasoningEfforts))
	for k := range ir.ValidReasoningEfforts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
