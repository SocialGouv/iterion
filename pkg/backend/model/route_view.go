package model

import "github.com/SocialGouv/iterion/pkg/dsl/ir"

// RouteView is the one value every launch-time routing derivation reads —
// the workflow under its launch-time overrides and run fallback chain, plus
// the vars those readers resolve `{{vars.*}}` routing fields against. It
// replaces the (workflow, overrides, runFallbacks) argument triple on
// EffectiveProviders and the wire-route readers, and gives the pre-flights
// what dispatch has always had: the run's vars.
//
// Vars is resolved as dispatch resolves them; nil is the reading every
// caller made before the run's vars existed at launch time — every span
// stays as written, every route stays undecided — so the change is inert
// for a caller with no vars to give. LateVars names the vars whose value
// the READING process decided, not the run (an `${…}`-consulting default, a
// coercion failure, an untrusted launch value): a field resolved through a
// late name is treated exactly as a raw `${…}` — undecided, never
// env-funded — because the runner will expand it with a different
// environment (ADR-129 §4, rule 1).
type RouteView struct {
	Workflow     *ir.Workflow
	Overrides    ModelOverrides
	RunFallbacks []FallbackEntry
	Vars         map[string]any
	LateVars     map[string]bool
	// AllowedProviders is the tenant's provider allowlist (ADR-129 §5) the
	// walk enforces when non-empty: a route resolving to a provider outside
	// it is excluded from the resolution's Providers and recorded in
	// Forbidden (with its site), and a route the walk cannot read is
	// recorded in Unreadable — under an allowlist, readability is
	// compulsory. Nil/empty is exactly the pre-allowlist walk: every route
	// records, nothing is refused. The launch and resume threads it from
	// the run's FROZEN routing policy (store.RunLLMRoutePolicy.Providers),
	// never from a re-resolution.
	AllowedProviders []string
}

// RouteViewForLaunch is the publisher-side reading: the workflow's declared
// defaults under the launch's overrides, resolved the way the launch
// resolves them (ir.LaunchVarsView), plus ADR-129 §4 rule 2 — a launch
// override whose name the lane does NOT pin in trusted is late whoever
// wrote it, because the webhook lanes seed vars from the delivery payload
// and a gate that narrowed on a payload-derived route would certify the
// payload's choice. Until a lane pins its names, every launch value is
// late and only declared defaults narrow — the conservative half.
// trusted carries the operator-pinned names (runview.LaunchSpec.TrustedVarNames).
func RouteViewForLaunch[V any](wf *ir.Workflow, overrides ModelOverrides, runFallbacks []FallbackEntry, launchOverrides map[string]V, trusted []string) RouteView {
	if wf == nil {
		return RouteView{Overrides: overrides, RunFallbacks: runFallbacks}
	}
	vars, late := ir.LaunchVarsView(wf, launchOverrides)
	trustedSet := make(map[string]bool, len(trusted))
	for _, name := range trusted {
		trustedSet[name] = true
	}
	for name := range launchOverrides {
		if _, isTrusted := trustedSet[name]; isTrusted {
			continue
		}
		if decl, declared := wf.Vars[name]; declared && decl != nil {
			if late == nil {
				late = make(map[string]bool)
			}
			late[name] = true
		}
	}
	return RouteView{Workflow: wf, Overrides: overrides, RunFallbacks: runFallbacks, Vars: vars, LateVars: late}
}

// RouteViewForDispatch is the runner-side reading: the run's own inputs —
// what queue.RunMessage.Vars carries — under the declared defaults, through
// the SAME declared-only filter the executor is seeded with
// (ir.DeclaredVarsOnly, inside ir.LaunchVarsView's layering). No trusted
// set: the run spends what its inputs say, whoever wrote them — the trust
// question is a gate question (who may CERTIFY a route), not a dispatch
// question. A name whose text consults the environment is late all the
// same, because the reading process may not be the one that expands it
// (the settings overlay repoints while a run waits).
func RouteViewForDispatch[V any](wf *ir.Workflow, overrides ModelOverrides, runFallbacks []FallbackEntry, msgVars map[string]V) RouteView {
	if wf == nil {
		return RouteView{Overrides: overrides, RunFallbacks: runFallbacks}
	}
	return RouteViewForLaunch(wf, overrides, runFallbacks, msgVars, ir.DeclaredVarNames(wf))
}

// field resolves a routing field's `{{vars.*}}` spans against the view —
// the reading every launch-time derivation shares. A late name stays as
// written, and so does every other namespace; the `${…}` pass is the
// reader's own, because the walk's provenance answers (envDeferred, the
// route-backend fromEnv bit) live on the raw spelling.
func (v RouteView) field(raw string) string {
	return resolveRoutingVars(raw, v.Vars, v.LateVars)
}

// gateVars returns the vars the `when:` gate may read: the view's, minus
// every late name — a gate over a late name is a gate the view cannot
// answer, and FallbackWhenActive keeps the route active there.
func (v RouteView) gateVars() map[string]any {
	if len(v.LateVars) == 0 || len(v.Vars) == 0 {
		return v.Vars
	}
	out := make(map[string]any, len(v.Vars))
	for name, val := range v.Vars {
		if !v.LateVars[name] {
			out[name] = val
		}
	}
	return out
}

// DecidedVars is the vars the view's reading decided — Vars without its
// late names, the map the `when:` gate reads (gateVars). A caller beside
// the fold that resolves a routing value against the view resolves it
// against THIS map: a late name's value is the reading process's own
// environment answer, and the run expands it with a different one
// (ADR-129 §4, rule 1) — a pin resolved over it parks, or spares, the run
// on this process's env.
func (v RouteView) DecidedVars() map[string]any { return v.gateVars() }
