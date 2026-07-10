package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// buildRunInputs assembles the input map for [runtime.Engine.Run]
// according to the documented precedence:
//
//  1. vars: defaults (applied by the engine, not visible here)
//  2. --preset <name>: in-source named preset values
//  3. --var key=value: CLI overrides
//
// Recipe presets are applied earlier in resolveWorkflow and are not
// in scope here. An unknown preset name returns a user-readable error
// listing the available names, since this is a CLI argument mistake
// the operator can correct.
func buildRunInputs(wf *ir.Workflow, presetName string, vars map[string]string) (map[string]any, error) {
	inputs := make(map[string]any)
	if presetName != "" {
		preset, ok := wf.Presets[presetName]
		if !ok {
			available := make([]string, 0, len(wf.Presets))
			for name := range wf.Presets {
				available = append(available, name)
			}
			sort.Strings(available)
			if len(available) == 0 {
				return nil, fmt.Errorf("--preset %q: workflow has no presets declared", presetName)
			}
			return nil, fmt.Errorf("--preset %q: unknown preset (available: %s)", presetName, strings.Join(available, ", "))
		}
		for k, v := range preset.Values {
			inputs[k] = v
		}
	}
	for k, v := range vars {
		inputs[k] = v
	}
	return inputs, nil
}
