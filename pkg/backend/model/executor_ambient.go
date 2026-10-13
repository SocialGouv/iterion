package model

import (
	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// ambientContextPolicy resolves a node's ambient-context policy (ADR-119) from
// the precedence chain: run override > node DSL > workflow DSL >
// ITERION_AMBIENT_CONTEXT > workspace, and reports the level it came from. The
// policy is a property of the node, not of the backend, so every element of a
// fallback chain receives the same value and translates it with its own
// mechanism.
//
// A declared value (anything but the default) meets the backend's verdict
// here, at the one point every chain element passes through:
//
//   - a refused policy fails the element with a
//     *delegate.ErrCapabilityUnsupported: the backend loads, whatever iterion
//     passes it, instruction files the policy excludes. The compiler refuses
//     the same (C187) when it can see the backend; this catches a fallback
//     element, a run override and the environment, which it cannot.
//   - an unmeasured one is logged, the backend keeping its own conventions
//     (C185 at compile time).
//
// The default is never refused or logged here: each backend applies it as far
// as it can, as documented.
func (e *ClawExecutor) ambientContextPolicy(f backendFields, backendName string) (ambient.Policy, string, error) {
	p, source := ambient.ResolveSourced(e.ambientContextOverride, f.ambientContext, e.wfAmbientContext, e.ambientContextEnvDefault)
	if !ambient.Declared(source) {
		return p, source, nil
	}
	switch support, reason := ambient.SupportOf(backendName, p); support {
	case ambient.Refused:
		return p, source, &delegate.ErrCapabilityUnsupported{
			NodeID:     f.id,
			Backend:    backendName,
			Capability: ambient.Capability(p, source),
			Remedy:     ambient.Remedy(backendName, p, reason),
		}
	case ambient.Unmeasured:
		if e.logger != nil {
			e.logger.Warn("[%s/ambient_context] %s (from %s) is not enforced on backend %q: what it loads has not been measured, so it keeps its own instruction-file conventions (ADR-119)",
				f.id, p, source, backendName)
		}
	}
	return p, source, nil
}
