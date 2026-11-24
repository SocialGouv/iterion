package model

import (
	"slices"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// SiteKind is how a model-calling site of a workflow spends: the shape of the
// call dispatch makes there.
type SiteKind string

const (
	// SiteNode is an agent or judge node's own dispatch on its backend,
	// over the chain its `provider:` field names (Legs).
	SiteNode SiteKind = "node"
	// SiteLLMRouter is a router in `llm` mode: dispatched like an agent
	// node, on its backend, over its chain, on the model its dispatch sends
	// (ClawExecutor.executeLLMRouterUnified, chainReading.routerModel).
	SiteLLMRouter SiteKind = "llm_router"
	// SiteHumanLLM is a human node's `interaction: llm|llm_or_human`
	// generation: a DIRECT claw generation on its model, in this process
	// (executeHumanLLMWithSpend) — under llm_or_human, one the node survives
	// (Degrades).
	SiteHumanLLM SiteKind = "human_llm"
	// SiteSelfRepair is a Verified Action's rung 3: a DIRECT claw generation
	// on the recovery model, in this process (selfRepair).
	SiteSelfRepair SiteKind = "self_repair"
	// SiteAgentRecovery is a Verified Action's rung 4: an agent dispatch on
	// the synthetic `<tool>__recover` node (agentRecovery).
	SiteAgentRecovery SiteKind = "agent_recovery"
	// SiteFallback is an authored `fallbacks:` route of an agent or judge
	// node: dispatched only when the node's own chain fails.
	SiteFallback SiteKind = "fallback"
	// SiteInteraction is an agent or judge node's interaction generation on
	// its `interaction_model:` (ExecuteHumanLLMForInteraction): a DIRECT claw
	// generation, made only when the node's backend asks a question.
	SiteInteraction SiteKind = "interaction"
	// SiteReviewCompanion is a review gate's companion (ExecuteReviewCompanion):
	// a DIRECT claw generation on the gate's model, whose failure pauses for
	// the human.
	SiteReviewCompanion SiteKind = "review_companion"
	// SiteSupervisor is a declared supervisor's evaluation: claw, in this
	// process, for the whole run.
	SiteSupervisor SiteKind = "supervisor"
	// SiteUnread is a node that calls models this workflow does not say how:
	// a subbot's child `.bot` (judged at its own start), or a model-calling
	// kind no arm of CallSites reads (ir.NodeUsesLLM's conservative answer).
	SiteUnread SiteKind = "unread"
)

// CallSite is one place a workflow's execution calls a model, read the way
// its dispatch makes the call (ADR-161 decisions 2 and 4): the backend it runs
// on, the chain it walks (Legs), the spec a leg that pins none sends, who
// calls (Role) and where (Host).
type CallSite struct {
	// ID names the site: the node's id for its own dispatch,
	// `<node>#self_repair` and `<node>#agent_recovery` for a Verified
	// Action's rungs, `<node>#interaction`, `<node>#review_companion`,
	// `<node>#<fallback>` (fallbackSite) and `supervisor:<name>`.
	ID string
	// NodeID is the workflow node the site belongs to; "" for a supervisor.
	NodeID string
	Kind   SiteKind
	// Node is the node dispatch runs: the workflow's own, or rung 4's
	// synthetic agent. Nil for a direct generation and a supervisor.
	Node ir.Node
	// Backend is the backend the call runs on, lower-cased: the reader's
	// resolution of the node's for a dispatch ("" when it resolves at
	// dispatch), claw for a direct generation and a supervisor.
	Backend string
	// Legs is the chain the call walks, in order: model.Legs for a dispatch
	// (empty where the allowlist dropped every leg), one leg for a fallback
	// route, one hint-free leg for a direct generation and a supervisor.
	Legs []credroute.Leg
	// Model is the spec a leg that pins none sends: Baseline for a dispatch
	// (an llm router's default included), the route's or the node's for a
	// fallback, the generation's own model. Empty where an agent or judge
	// node names none — a claw dispatch then sends the detector's pick
	// (Base).
	Model string
	// Role is who calls: the node's own dispatch, an auxiliary generation of
	// its execution, a supervisor.
	Role credroute.Role
	// Host: the call is made in this process, on the host, whatever the
	// run's sandbox — a direct generation, a supervisor.
	Host bool
	// Rescue: the call is made only on another's failure or a question —
	// a `fallbacks:` route, an interaction generation, the review companion
	// (whose failure pauses for the human), a supervisor. The launch warns
	// on a rescue no credential funds and the pre-flight keys none but the
	// supervisor; every other site is a call the node makes when it runs.
	Rescue bool
	// Degrades: a call the node makes whenever it runs, whose failure the
	// node survives — an `interaction: llm_or_human` LLM half falls back to
	// the human pause, whatever failed it (no credential included). The
	// launch warns on one no credential funds, as on a rescue, since its
	// dispatch never fails the node; the pre-flight keys it, as every call
	// the node makes when it runs.
	Degrades bool
	// Name is the supervisor's or the fallback route's own name.
	Name string
}

// Base is the spec a leg of s that pins none sends, as the dispatch builds it
// (buildTask): the site's model, and for a claw dispatch that names none the
// detector's pick — detected, ClawExecutor.DetectedClawSpec for the executor
// that will dispatch, DetectedClawSpecUnder for a reader that holds none. Its
// refusal under the run's allowlist is the whole site's: the dispatch fails
// the node before its first leg. Nil detected leaves the spec empty.
func (s CallSite) Base(detected func(nodeID string) (string, error)) (string, error) {
	return siteBase(s.Model, s.Backend, s.Host, s.NodeID, detected)
}

func siteBase(mdl, backend string, host bool, nodeID string, detected func(nodeID string) (string, error)) (string, error) {
	if mdl != "" || host || backend != delegate.BackendClaw || detected == nil {
		return mdl, nil
	}
	return detected(nodeID)
}

// LegSpec is the spec leg i sends: its own model, else base (Base).
func (s CallSite) LegSpec(i int, base string) string {
	return legSpec(s.Legs, i, base)
}

func legSpec(legs []credroute.Leg, i int, base string) string {
	if i >= 0 && i < len(legs) && legs[i].Model != "" {
		return legs[i].Model
	}
	return base
}

// Call is leg i's question to the resolver for spec: placed where the call is
// made — this process for a Host site, placement (the run's sandbox) for a
// dispatch.
func (s CallSite) Call(i int, spec string, placement credroute.Sandbox) credroute.Call {
	return siteCall(s.Backend, s.Legs, s.Role, s.Host, i, spec, placement)
}

func siteCall(backend string, legs []credroute.Leg, role credroute.Role, host bool, i int, spec string, placement credroute.Sandbox) credroute.Call {
	if role == "" {
		role = credroute.RoleNode
	}
	call := credroute.Call{Backend: backend, Spec: spec, Role: role, Sandbox: placement}
	if i >= 0 && i < len(legs) {
		call.Leg = legs[i]
	}
	if host {
		call.Sandbox = credroute.Sandbox{}
	}
	return call
}

// CallSites is the ONE enumeration of wf's model-calling sites under v
// (ADR-161 decision 4): every node kind and role dispatch can turn into a
// model call, each with its legs. The launch's admission and the usage-cap
// pre-flight iterate it and nothing else, so a site one reader judges is a
// site the other keys. backendOf is the reader's resolution of a dispatched
// node's backend — the executor's own, launch override included, for the
// admission; nil reads the view's (routeBackend: an override, the node's
// field, the workflow default, "" when it resolves at dispatch).
//
// A subagent's spec is the `agent` tool's argument, chosen by the model at
// dispatch; the one it inherits when the model names none is the node's own,
// which the node's site already carries. Structured-output recovery
// (extractStructuredViaClaw) is no site: it fires on an answer that already
// failed its schema, on the detector's pick, and is skipped — never the
// node's error — where nothing funds it. Its readers are the dispatch, the
// ledger and the plan, through the Spend its own event carries
// (credparity.RecoveryRows).
//
// Sorted by ID, so a reader that names its sites names them in one order on
// every start.
func CallSites(v RouteView, backendOf func(ir.Node) string) []CallSite {
	wf := v.Workflow
	if wf == nil {
		return nil
	}
	if backendOf == nil {
		backendOf = func(n ir.Node) string { return v.dispatchBackend(n) }
	}
	var out []CallSite
	for _, n := range wf.Nodes {
		out = append(out, nodeCallSites(v, n, backendOf)...)
	}
	for i, sup := range wf.Supervisors {
		if sup == nil {
			continue
		}
		out = append(out, CallSite{
			ID: SupervisorSiteID(sup.Name, i), Kind: SiteSupervisor, Name: sup.Name,
			Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: strings.TrimSpace(ir.ExpandEnvWithDefault(v.field(v.Overrides.EffectiveSupervisorModel(sup)))),
			Role: credroute.RoleSupervisor, Host: true, Rescue: true,
		})
	}
	slices.SortFunc(out, func(a, b CallSite) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// SupervisorSiteID names the site of the supervisor declared i-th:
// `supervisor:<name>`, its index for one the author named none. A DSL node id
// never holds the colon, so the id never names a node.
func SupervisorSiteID(name string, i int) string {
	if name != "" {
		return "supervisor:" + name
	}
	return "supervisor:#" + strconv.Itoa(i)
}

// nodeCallSites is n's sites, by the call dispatch turns each into.
func nodeCallSites(v RouteView, n ir.Node, backendOf func(ir.Node) string) []CallSite {
	id := n.NodeID()
	switch node := n.(type) {
	case *ir.AgentNode, *ir.JudgeNode:
		site := v.dispatchSite(id, id, SiteNode, n, backendOf(n))
		out := []CallSite{site}
		inf := n.(interface{ GetInteractionFields() *ir.InteractionFields }).GetInteractionFields()
		if mdl := strings.TrimSpace(v.field(inf.InteractionModel)); interactsWithModel(inf.Interaction) && mdl != "" {
			out = append(out, directSite(id+interactionSiteSuffix, id, SiteInteraction, mdl, true))
		}
		return append(out, v.fallbackSites(n, site)...)
	case *ir.RouterNode:
		if node.RouterMode != ir.RouterLLM {
			return nil // the deterministic modes call no model
		}
		return []CallSite{v.dispatchSite(id, id, SiteLLMRouter, n, backendOf(n))}
	case *ir.HumanNode:
		switch {
		case node.Interaction == ir.InteractionReview:
			return []CallSite{directSite(id+reviewCompanionSiteSuffix, id, SiteReviewCompanion, v.field(node.Model), true)}
		case interactsWithModel(node.Interaction):
			site := directSite(id, id, SiteHumanLLM, v.field(node.Model), false)
			site.Degrades = node.Interaction == ir.InteractionLLMOrHuman
			return []CallSite{site}
		}
		return nil
	case *ir.ToolNode:
		spec, selfRepair, agentRung := recoveryRungsOf(n)
		if !selfRepair && !agentRung {
			return nil
		}
		spec = recoverySpecOf(v, spec)
		var out []CallSite
		if selfRepair {
			out = append(out, directSite(id+"#self_repair", id, SiteSelfRepair, spec, false))
		}
		if agentRung {
			syn := recoveryAgentNode(id, spec, nil)
			out = append(out, v.dispatchSite(id+agentRecoverySiteSuffix, id, SiteAgentRecovery, syn, backendOf(syn)))
		}
		return out
	}
	if ir.NodeUsesLLM(n) {
		// A subbot's child, or a kind no arm above reads: it calls models
		// this workflow does not say how.
		return []CallSite{{ID: id, NodeID: id, Kind: SiteUnread, Node: n, Role: credroute.RoleNode}}
	}
	return nil
}

// reviewCompanionSiteSuffix is the site of a review gate's companion.
const reviewCompanionSiteSuffix = "#review_companion"

// interactsWithModel reports whether an interaction mode answers with a
// model rather than by parking for a human.
func interactsWithModel(m ir.InteractionMode) bool {
	return m == ir.InteractionLLM || m == ir.InteractionLLMOrHuman
}

// dispatchSite is the site of a node dispatch: n on backend, over the chain
// the dispatch walks and the model a leg that pins none sends.
func (v RouteView) dispatchSite(id, nodeID string, kind SiteKind, n ir.Node, backend string) CallSite {
	return CallSite{
		ID: id, NodeID: nodeID, Kind: kind, Node: n, Backend: strings.ToLower(strings.TrimSpace(backend)),
		Legs: Legs(v, n), Model: strings.TrimSpace(Baseline(v, n)), Role: credroute.RoleNode,
	}
}

// directSite is the site of a direct claw generation on mdl: one hint-free
// leg — no dispatch stamps a hint on the ctx these generations resolve
// under — in this process, an auxiliary of the node's execution.
func directSite(id, nodeID string, kind SiteKind, mdl string, rescue bool) CallSite {
	return CallSite{
		ID: id, NodeID: nodeID, Kind: kind, Backend: delegate.BackendClaw,
		Legs: []credroute.Leg{{}}, Model: strings.TrimSpace(ir.ExpandEnvWithDefault(mdl)),
		Role: credroute.RoleAuxiliary, Host: true, Rescue: rescue,
	}
}

// fallbackSites is n's active `fallbacks:` routes: each on its own backend
// (the node's where it names none), its own hint, its own model (the node's
// where it names none). A `skip` route dispatches nothing, and a route the
// run's own vars deactivate is no path the run takes.
func (v RouteView) fallbackSites(n ir.Node, node CallSite) []CallSite {
	gf, ok := n.(interface{ GetFallbacks() []ir.Fallback })
	if !ok {
		return nil
	}
	var out []CallSite
	for i, fb := range gf.GetFallbacks() {
		fb = v.Overrides.EffectiveFallback(n.NodeID(), i, fb)
		if fb.Action == ir.FallbackActionSkip || !FallbackWhenActive(v.DecidedVars(), n.NodeID(), fb, nil) {
			continue
		}
		backend := strings.ToLower(v.field(fb.Backend))
		if backend == "" {
			backend = node.Backend
		}
		leg := credroute.Leg{Provider: strings.ToLower(v.field(fb.Provider))}
		if leg.Provider == "auto" {
			leg.Provider = ""
		}
		mdl := v.field(fb.Model)
		if mdl == "" {
			mdl = node.Model
		}
		out = append(out, CallSite{
			ID: fallbackSite(n.NodeID(), i, fb.Name), NodeID: n.NodeID(), Kind: SiteFallback, Node: n, Name: fb.Name,
			Backend: backend, Legs: []credroute.Leg{leg}, Model: mdl, Role: credroute.RoleNode, Rescue: true,
		})
	}
	return out
}

// DetectedClawSpecUnder is ClawExecutor.DetectedClawSpec for a reader that
// holds no executor (the usage-cap pre-flight): the detector's suggestion
// over this process's environment — detect.Providers, the report the
// executor's own detector caches — vetted against allowed, the run's
// provider allowlist, by the predicate the dispatch applies
// (allowlistedPick).
func DetectedClawSpecUnder(allowed []string) func(nodeID string) (string, error) {
	set := AllowedProviderSet(allowed)
	return func(nodeID string) (string, error) {
		return allowlistedPick(set, nodeID, detect.SuggestedModel(detect.BackendClaw, detect.Providers()))
	}
}

// dispatchBackend is the view's reading of the backend n's dispatch runs on:
// a launch override, the node's `backend:`, the workflow's default — "" where
// none names one, or the first that does resolves at dispatch.
func (v RouteView) dispatchBackend(n ir.Node) string {
	ov := v.Overrides.ForNode(n.NodeID(), n.NodeKind())
	var own string
	if fields, ok := llmFieldsOf(n); ok {
		own = fields.Backend
	}
	var wfDefault string
	if v.Workflow != nil {
		wfDefault = v.Workflow.DefaultBackend
	}
	return routeBackend(v.field(ov.Backend), v.field(own), v.field(wfDefault))
}
