package runview

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrForkInputsRefused wraps every refusal of an operator-supplied value, so
// the HTTP surface answers 400 for what the operator typed instead of the 500
// its default arm gives a genuine fault.
var ErrForkInputsRefused = errors.New("fork: the values supplied in new_inputs were refused")

// gateForkInputs applies the launch-time var constraints — `[enum: …]` and
// `[matching: "<re>"]` — to the values an operator supplies to a fork.
//
// It exists because `fork` is the one operator surface that writes var values
// into a run without crossing Engine.Run: the child is parked `cancelled` and
// executed by Resume, and Resume deliberately does not re-judge stored values
// (tightening a declaration must never make a run unresumable). So a
// declaration read as enforced was inert on this path.
//
// It returns the CHANGED map — the delta the caller records on the child as
// store.Run.ForkSuppliedInputs, so the child's first resume judges exactly
// these keys against the workflow it compiles — and a NOTE for the one case
// it admits without a verdict:
//
//   - Only the values the operator CHANGED are judged. The studio's ForkDialog
//     pre-fills its editor with the parent's whole input map, so an unmodified
//     submit re-sends every value the parent was already admitted with; judging
//     those again would re-run the launch gate on a payload nobody typed, and
//     would refuse a fork whose declaration was tightened after the parent ran.
//     An unchanged map compiles nothing and cannot fail.
//   - The source judged is the one the parent EXECUTED, recorded on its own
//     document, never whatever the path holds now.
//   - An environment-dependent value is refused rather than guessed. The
//     expansion of `${…}` is a property of the PROCESS: this gate runs in the
//     server, the child runs in a runner pod with another environment, and
//     ${PROJECT_DIR} names a worktree that does not exist yet. Measured on the
//     engine's own gate: the same declaration admits or refuses `${MODE}`
//     depending on the environment of whoever evaluates it. A verdict on such a
//     value would be a guess, and a guess in a blocking gate is worse than the
//     hole it closes.
//   - A parent whose recorded source cannot be read back (over the 1 MiB
//     record cap, pre-2026-08-04, cleared by a forced cloud resume, or no
//     longer compiling) is ADMITTED with a note, not refused: the child's
//     first resume judges the recorded delta, so nothing reaches execution
//     unchecked and the cap stops costing forkability. Every path that would
//     execute the values crosses that floor; a run whose source is so far
//     gone that no engine can compile it cannot resume at all, so its values
//     never execute either.
func (s *Service) gateForkInputs(parent *store.Run, newInputs map[string]any) (changed map[string]any, note string, err error) {
	changed = changedForkInputs(parent.Inputs, newInputs)
	if len(changed) == 0 {
		return changed, "", nil
	}
	wf, werr := recordedWorkflow(parent)
	if werr != nil {
		note = fmt.Sprintf("the parent's recorded workflow source could not be read back (%v), so these values were admitted without a pre-check — the child's first resume judges them against the var constraints the workflow it compiles declares, and refuses it if they violate one", werr)
		return changed, note, nil
	}
	var violations []string
	literal := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(changed)) {
		// An UNDECLARED key is not a violation, because the launch gate does
		// not treat it as one: `{{input.X}}` resolves from run inputs the
		// workflow never declared, and surfaces like the forge gate relaunch
		// write such keys deliberately. Refusing here would make the fork
		// stricter than the launch it is recovering from — and it is
		// all-or-nothing, so one unknown key would kill every valid change in
		// the same submit.
		decl, declared := wf.Vars[k]
		if !declared {
			continue
		}
		if decl.Type != ir.VarString || (len(decl.EnumValues) == 0 && decl.Matching == "") {
			continue
		}
		text, isText := changed[k].(string)
		if isText && environmentDependent(text, decl.Type) {
			violations = append(violations, fmt.Sprintf(
				"var %q: value %q is resolved from the environment, and the process that runs the child is not this one — a constrained var takes a literal here",
				k, text))
			continue
		}
		literal[k] = changed[k]
	}
	if len(violations) > 0 {
		return changed, "", fmt.Errorf("runview: %w: %s", ErrForkInputsRefused, strings.Join(violations, "; "))
	}
	// Every remaining value reads the same in every environment, so the
	// expander can never be consulted — proven one line above, per value.
	if verr := runtime.ValidateVarConstraints(wf.Vars, literal, noEnv); verr != nil {
		return changed, "", fmt.Errorf("runview: %w: %v", ErrForkInputsRefused, verr)
	}
	return changed, "", nil
}

func noEnv(string) string { return "" }

// unjudgedForkKeys is the child's ForkSuppliedInputs: the keys the operator
// moved in this fork, plus the keys the PARENT itself inherited unjudged and
// that this fork re-sends unchanged (their values are not judged here — an
// unchanged value is never judged, the property that keeps the studio's
// unmodified ForkDialog submits working — so they stay armed for the child's
// first resume). Sorted and deduplicated: the field is a record, not a
// multiset, and both stores round-trip it verbatim.
func unjudgedForkKeys(parent *store.Run, changed map[string]any) []string {
	keys := make(map[string]bool, len(changed)+len(parent.ForkSuppliedInputs))
	for k := range changed {
		keys[k] = true
	}
	for _, k := range parent.ForkSuppliedInputs {
		keys[k] = true
	}
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// changedForkInputs keeps the keys whose value the operator actually moved.
func changedForkInputs(parentInputs, newInputs map[string]any) map[string]any {
	changed := map[string]any{}
	for k, v := range newInputs {
		if old, had := parentInputs[k]; had && reflect.DeepEqual(old, v) {
			continue
		}
		changed[k] = v
	}
	return changed
}

// environmentDependent reports whether a var value reads differently under two
// different environments. It probes the REAL reading (ir.ResolveVarText) with
// two distinct answers rather than looking for `${` in the text: the set of
// spellings that reach the environment is the compiler's business, not a
// pattern this file should try to keep up with.
func environmentDependent(text string, vt ir.VarType) bool {
	// The EMPTY name is answered identically by both probes: os.Getenv("") is
	// "" on every machine, so `${}` is a literal and refusing it would be a
	// false positive.
	probe := func(answer string) func(string) string {
		return func(name string) string {
			if name == "" {
				return ""
			}
			return answer
		}
	}
	a, errA := ir.ResolveVarText(text, vt, probe("\x00probe-a"))
	b, errB := ir.ResolveVarText(text, vt, probe("\x00probe-b"))
	if errA != nil || errB != nil {
		return true
	}
	return !reflect.DeepEqual(a, b)
}

// recordedWorkflow compiles the unit a run EXECUTED, from the files pinned on
// its own document. Nothing is read from disk: the path a cloud run names does
// not exist in the server pod, and the file a local path names may have moved
// on since.
func recordedWorkflow(run *store.Run) (*ir.Workflow, error) {
	var merged *unit.Unit
	switch {
	case len(run.WorkflowSources) > 0:
		files := make(map[string]string, len(run.WorkflowSources))
		for _, f := range run.WorkflowSources {
			files[f.Path] = f.Text
		}
		merged = unit.LoadMap(files, recordedMain(run))
	case strings.TrimSpace(run.WorkflowSource) != "":
		merged = unit.LoadMap(map[string]string{"<recorded>": run.WorkflowSource}, "<recorded>")
	default:
		return nil, errors.New("this run recorded no workflow source")
	}
	for _, d := range merged.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("parse: %s", d.Error())
		}
	}
	if merged.Merged == nil || len(merged.Merged.Workflows) == 0 {
		return nil, errors.New("the recorded source declares no workflow")
	}
	cr := ir.Compile(merged.Merged)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			return nil, fmt.Errorf("compile: %s", d.Error())
		}
	}
	if cr.Workflow == nil {
		return nil, errors.New("the recorded source compiled to no workflow")
	}
	return cr.Workflow, nil
}

// recordedMain names the unit's main file. The recorded list is written
// main-first, but a position in a slice is not a fact: the run's own FilePath
// is matched first and the order is only the fallback.
func recordedMain(run *store.Run) string {
	base := filepath.Base(strings.TrimSpace(run.FilePath))
	if base != "" && base != "." && base != string(filepath.Separator) {
		for _, f := range run.WorkflowSources {
			if filepath.Base(f.Path) == base {
				return f.Path
			}
		}
	}
	return run.WorkflowSources[0].Path
}
