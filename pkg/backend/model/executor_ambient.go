package model

import "github.com/SocialGouv/iterion/pkg/backend/ambient"

// ambientContextPolicy resolves a node's ambient-context policy (ADR-119) from
// the precedence chain: run override > node DSL > workflow DSL >
// ITERION_AMBIENT_CONTEXT > workspace. The policy is a property of the node,
// not of the backend, so every element of a fallback chain receives the same
// value and translates it with its own mechanism.
//
// A backend that translates no policy (opencode, kimi, grok) keeps its own
// conventions. The compiler warns when it can see that (C185); when the
// backend is only known at dispatch — a fallback element, a backend chosen by
// the environment — and the policy was chosen explicitly rather than
// defaulted, the run says so here instead of ignoring it in silence.
func (e *ClawExecutor) ambientContextPolicy(f backendFields, backendName string) ambient.Policy {
	p, source := ambient.ResolveSourced(e.ambientContextOverride, f.ambientContext, e.wfAmbientContext, e.ambientContextEnvDefault)
	if source != "default" && !ambient.Enforces(backendName) && e.logger != nil {
		e.logger.Warn("[%s/ambient_context] %s (from %s) is not enforced on backend %q: it keeps its own instruction-file conventions (ADR-119)",
			f.id, p, source, backendName)
	}
	return p
}
