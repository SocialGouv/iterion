package model

import (
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// codemodePolicy resolves a node's codemode (ADR-165) from the rungs that may
// carry it — the node's own value, then the workflow's, then off — and
// refuses a DECLARED `on` on a backend that serves no codemode tool.
//
// THE REFUSAL IS TERMINAL, and the message is what the operator gets instead
// of a run. An adversarial round refuted the claim this comment used to make —
// that a `fallbacks:` route would carry the node past it. It does not: the
// PRIMARY task is built outside the chain (executor_build_task.go, where the
// build error returns straight to the caller), and the twin this function
// mirrors behaves the same way, which its own
// TestAmbientContextRefusesAFallbackThatCannotHonourIt asserts. In this
// codebase an ErrCapabilityUnsupported ends the node; being typed is what
// lets a reader tell it from a transport failure, not what routes it.
//
// So the message carries the whole way out, because nothing downstream will:
// the backends that do serve codemode, and the `codemode: off` that keeps a
// node off a workflow default.
//
// It refuses rather than degrades, and that is the whole decision this
// function encodes. A node whose author asked for codemode and silently got
// none is the "fallback silencieux" the repository's conventions forbid: the
// model keeps its whole loop in context, the bill is the one codemode exists
// to avoid, and nothing in the run says so. The DEFAULT is not refused,
// because a fleet of nodes inheriting nothing must not start failing the day
// a registry cell changes — and since the default is OFF, "declared" and "on"
// are the same question here. That is ambient's division
// (executor_ambient.go), and this function is deliberately its twin.
//
// The compiler refuses the same thing earlier (C339), for the routes the
// source decides. This is not a duplicate of that check but its other half:
// a node with no `backend:`, an `auto` route or a `{{vars.x}}` a launch
// overrides has no backend the compiler can read, and this is the first place
// the resolved one exists.
// The BOUNDS come back with the policy, from the same call, for the reason
// this file's whole shape encodes: a second place that walked the same ladder
// would be a second opinion, and the one that answered second would win on
// some path. They are resolved only where the capability is GRANTED — an off
// policy returns before the bounds ladder is walked, because a node that runs
// no program has no bound to carry, and a non-nil bound set beside a
// Codemode:false task would read as a budget nobody can spend.
func (e *ClawExecutor) codemodePolicy(f backendFields, backendName string) (ir.CodemodePolicy, *ir.CodemodeBounds, *ir.CodemodeSubagents, error) {
	// ONE guard. `policy.Declared()` stood here too and could never fire:
	// the only rung answering "not declared" is the default, and the default
	// is OFF. The compiler's half carries the same single guard and
	// ir.TestOnImpliesDeclared pins the invariant both rely on. The run rung
	// is nearest: an operator's launch choice decides before anything the
	// source wrote, in both directions.
	policy := ir.ResolveCodemode(e.codemodeOverride, f.codemode, e.wfCodemode)
	if !policy.On {
		return policy, nil, nil, nil
	}
	bounds := ir.ResolveCodemodeBounds(e.codemodeBoundsOverride, f.codemodeBounds, e.wfCodemodeBounds)
	// The reserve resolves on the same ladder — the run rung stays unfilled
	// for the reason the deferral's stays: no launch surface carries the
	// value yet. nil after the ladder means nobody declared subagents
	// tokens anywhere, and the agent tool stays withheld from this node's
	// programs.
	subagents := ir.ResolveCodemodeSubagents(nil, f.codemodeSubagents, e.wfCodemodeSubagents)
	// The POSITIVE question, not the compiler's. Here the backend name is
	// RESOLVED, and the only loop that installs the tool is the one whose
	// cell says shipped — so a name the registry does not carry (a custom
	// backend in an operator's own registry) is refused out loud rather than
	// handed a Codemode the loop it reaches will never read.
	if ir.CodemodeServedBy(backendName) {
		return policy, bounds, subagents, nil
	}
	return policy, nil, nil, &delegate.ErrCapabilityUnsupported{
		NodeID:     f.id,
		Backend:    backendName,
		Capability: policy.String(),
		Remedy:     ir.CodemodeRemedy(backendName),
	}
}

// codemodeDeferOn resolves the node's defer state (ir.ResolveCodemodeDefer)
// beside codemodePolicy, over the same node/workflow rungs. There is no run
// rung to read: no launch surface carries the value (no queue field, no CLI
// flag), so the run parameter is "" here as it is in the compiler — the
// resolver keeps the rung for the day a launch override exists.
//
// It is read only where the policy is ON — the build site answers
// policy.On && deferOn, because a deferral without the catalogue it defers
// to bounds nothing and demoting tools on a codemode-off node would strand
// them behind a program the model has no way to run.
func (e *ClawExecutor) codemodeDeferOn(f backendFields) bool {
	return ir.ResolveCodemodeDefer("", f.codemodeDefer, e.wfCodemodeDefer) == "on"
}
