package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
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
func buildRunInputs(wf *ir.Workflow, presetName string, vars map[string]string, allowUnknown bool) (map[string]any, error) {
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
	// A --var that names no var of the workflow is refused, not dropped:
	// the run would execute on defaults while the operator believes it was
	// parameterised (#1757). The same refusal lives at the cloud chokepoint
	// (pkg/runview) and again at the runner — one rule at every door, one
	// helper. allowUnknown is the operator's explicit opt-out
	// (--allow-unknown-inputs): the keys ride, said — forwarding an
	// undeclared payload key to a subbot through {{input.*}} is their
	// legitimate use (C149). The engine-injected names are carved out
	// (#2416): an env-shaped bot-var dial (a provision's ITERION_VIBE_*)
	// names no var and is none the less a real input — and this CLI is
	// also the detached runner's subprocess, so refusing here would kill a
	// launch the server already admitted.
	if unknown := ir.UnknownInputNames(wf, vars); len(unknown) > 0 && !allowUnknown {
		if _, refused := platformcfg.SplitEngineInjectedInputs(unknown); len(refused) > 0 {
			return nil, fmt.Errorf("--var %q, which names no var of the workflow (declared: %s)",
				strings.Join(refused, ", "), strings.Join(ir.DeclaredVarNames(wf), ", "))
		}
	}
	return inputs, nil
}
