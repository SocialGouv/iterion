package runview

import (
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// ErrLaunchVarsRefused wraps every launch refused at admission because a var
// value is outside what its var declares: the operator's error, answered on
// the request.
var ErrLaunchVarsRefused = errors.New("var validation")

// GateLaunchVars applies the var constraints — `[enum: …]` and
// `[matching: "<re>"]` — to the values a launch starts with, at admission:
// the operator who typed a value outside them gets the refusal on the
// request, while their hands are on the keyboard, instead of a run that
// fails once a runner has claimed it.
//
// It is the EARLIER of two refusals, never a replacement: the engine's gate
// (Engine.Run → validateVarConstraints) is the one every launch surface
// funnels into, and this one must never refuse a value that gate admits. So
// it judges only what reads the same in every environment:
//
//   - inputs are the values the run starts with, every server-side
//     derivation applied — the credential-derived topology vars included,
//     which may replace a value the operator sent;
//   - a value whose reading depends on the environment — `${MODE:-fast}`,
//     `${PROJECT_DIR}/x` — is passed on unjudged: its expansion belongs to
//     the process that runs the engine, and the engine's gate judges it
//     there.
//
// The refusal is the engine's, in the engine's words: each violation names
// the var, the value and the constraint it failed.
func GateLaunchVars(wf *ir.Workflow, inputs map[string]any) error {
	literal := make(map[string]any, len(inputs))
	for k, v := range inputs {
		if decl, declared := wf.Vars[k]; declared {
			if text, isText := v.(string); isText && environmentDependent(text, decl.Type) {
				continue
			}
		}
		literal[k] = v
	}
	// Every remaining value reads the same in every environment, so the
	// expander can never be consulted.
	if err := runtime.ValidateVarConstraints(wf.Vars, literal, noEnv); err != nil {
		return fmt.Errorf("%w: %v", ErrLaunchVarsRefused, err)
	}
	return nil
}
