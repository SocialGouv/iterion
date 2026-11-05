package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sandbox_credential_admission.go is the launch's credential admission
// (#1768, ADR-161 decision 4): a run the launch judges — an untrusted
// (fork-lane) run, and every run stamped to its own credentials (ADR-131 §7)
// — is refused before it starts when a node of it has no leg any credential
// funds. The alternative is the run dying mid-run on the backend's opaque
// "not logged in", or on the stamp's refusal, hours in, after the review's
// expensive half. The launch refuses instead, typed, naming every such node
// and the ways out.
//
// The answer is the resolver's (credroute.Resolve), the one the dispatch
// spends on: every leg of every call site of the run (model.CallSites — the
// one enumeration the usage-cap pre-flight keys too; model.Legs, the chain
// each dispatch walks), placed where each site calls its model, with what
// the sandbox binds of the operator's home. A backend whose arm has not
// landed (ErrNoArm) keeps today's per-backend probe in a fork-lane sandbox;
// the cases nothing can read are a WARNING (sandbox_no_credential_warning),
// never a refusal on a guess — an unknown backend, a `fallbacks:` route the
// resolver refuses, a supervisor's own spec.
//
// It runs at ONE site every start passes, once its sandbox is known:
// resolveAndStartSandbox after the mode, the driver and the host-state mount
// are resolved and before the host exits (`none`, a degraded `auto`) and any
// pull, mount or start — and startSandbox's adopt branch for a subbot child
// under a sandboxed parent, on the child's own nodes.
//
// The host_state channel counts while it is mounted: nothing on main floors
// host_state for an untrusted run yet, so a local file-based login still
// reaches a docker sandbox — the day a floor withdraws it, this refusal is
// what tells the operator WHY the run cannot launch.

// sandboxCredentialHome resolves what this sandbox will actually bind of
// the operator's home: the resolved host_state, the driver's bind
// capability, and the planted-home refusal — the same inputs
// applyHostStateMounts will apply below, so the probe and the mount plan
// cannot disagree.
func sandboxCredentialHome(p SandboxParams, caps sandbox.Capabilities) credroute.SandboxHomeState {
	resolved, _ := pickHostState(workflowHostState(p.Workflow), p.HostStateOverride, p.HostStateDefault)
	if !sandbox.HostState(resolved).Active() || !caps.SupportsHostBindMounts {
		return credroute.SandboxHomeState{}
	}
	homeDir, planted := resolveHostHomeDir()
	if planted || homeDir == "" {
		return credroute.SandboxHomeState{}
	}
	return credroute.SandboxHomeState{Mounted: true, HomeDir: homeDir}
}

// credentialRouteViewer is the executor that reports the view its dispatch
// reads a workflow's chains under (model.ClawExecutor.CredentialRouteView).
type credentialRouteViewer interface {
	CredentialRouteView(wf *ir.Workflow) model.RouteView
}

// credentialRouteView is the view the engine's executor dispatches the
// workflow's chains under, for a run launched with inputs: the executor's own
// overrides, vars and allowlist, and over its vars the ones the run pushes
// it before its first node (pushExecutorVars: resolveVars over the same
// inputs — the declared defaults, the launch's values, a subbot child's
// `with:` bindings). Read before the sandbox starts, it is the reading the
// dispatch's legs take once the run is under way. An executor that reports
// no view applies no overrides or allowlist of its own.
func (e *Engine) credentialRouteView(inputs map[string]any) model.RouteView {
	v := model.RouteView{Workflow: e.workflow}
	if viewer, ok := e.executor.(credentialRouteViewer); ok {
		v = viewer.CredentialRouteView(e.workflow)
	}
	vars := maps.Clone(v.Vars)
	if vars == nil {
		vars = map[string]any{}
	}
	// Quiet: runInitState resolves the same vars and says what it coerced.
	maps.Copy(vars, e.resolveVarsLogging(inputs, iterlog.Nop()))
	v.Vars = vars
	return v
}

// clawSpecDetector is the executor whose claw dispatch sends the detector's
// suggestion for a node that names no model (model.ClawExecutor).
type clawSpecDetector interface {
	DetectedClawSpec(nodeID string) (string, error)
}

// The claw executor answers both readings the admission takes of its
// dispatch.
var (
	_ credentialRouteViewer = (*model.ClawExecutor)(nil)
	_ clawSpecDetector      = (*model.ClawExecutor)(nil)
)

// detectedClawSpec is the executor's pick for a claw node naming no model,
// nil for an executor that picks none.
func (e *Engine) detectedClawSpec() func(nodeID string) (string, error) {
	if d, ok := e.executor.(clawSpecDetector); ok {
		return d.DetectedClawSpec
	}
	return nil
}

// admitAdoptedCredentials is the admission of a subbot child adopting its
// parent's live sandbox: its own nodes, sandboxed, under its own trust, their
// legs read under the vars its launch inputs — the `with:` its parent binds —
// give them.
func (e *Engine) admitAdoptedCredentials(ctx context.Context, runID string, inputs map[string]any, emitEvent func(store.EventType, map[string]any) error) error {
	if e.simulation != (Simulation{}) || e.workflow == nil {
		return nil
	}
	return credentialAdmission{
		wf: e.workflow, routes: e.credentialRouteView(inputs), detected: e.detectedClawSpec(),
		placement: credroute.Sandbox{Contained: true, Home: e.sharedSandbox.CredentialHome},
		trust:     e.trust, resolver: e.backendResolver(), runID: runID,
		adopted: true,
	}.admit(ctx, emitEvent, e.logger)
}

// credentialAdmission is one run's admission inputs.
type credentialAdmission struct {
	wf *ir.Workflow
	// routes is the view the dispatch reads the run's chains under.
	routes model.RouteView
	// detected is the dispatch's pick for a claw node naming no model; nil
	// leaves such a node's spec empty.
	detected func(nodeID string) (string, error)
	// placement is where the run's nodes call their models.
	placement credroute.Sandbox
	trust     store.RunTrust
	resolver  effectiveBackendResolver
	runID     string
	// adopted: a subbot child judged in its parent's sandbox. No start of
	// its own ever ran a per-backend probe, so a backend without an arm
	// keeps that path — unjudged — and is warned.
	adopted bool
}

// nodeVerdict is a call site's admission: the spend its first funded leg
// lands on, or — none funded — the refusal its chain ends on.
type nodeVerdict struct {
	// node names the site (model.CallSite.ID): the node's id for its own
	// dispatch, `<tool>#self_repair` for a recovery rung.
	node string
	// nodeID is the workflow node the site belongs to.
	nodeID  string
	backend string
	spend   credroute.Spend
	// refusal is the last leg's refusal: the one the dispatch meets when its
	// chain runs out. Nil for an admitted site.
	refusal *credroute.Refusal
}

// admitSandboxCredentials is the resolved start's admission, its nodes
// placed where the start resolved them: the run's own sandbox, with the home
// mount it binds, or the host.
func admitSandboxCredentials(ctx context.Context, p SandboxParams, placement credroute.Sandbox, emitEvent func(store.EventType, map[string]any) error, logger *iterlog.Logger) error {
	if p.Simulation || p.Workflow == nil {
		return nil
	}
	routes := p.Routes
	routes.Workflow = p.Workflow
	return credentialAdmission{
		wf: p.Workflow, routes: routes, detected: p.DetectedClawSpec, placement: placement,
		trust: p.Trust, resolver: p.EffectiveBackend, runID: p.RunID,
	}.admit(ctx, emitEvent, logger)
}

// admit refuses the run when a call site has no funded leg, warning on what
// it cannot read and on the rescues nothing funds. Nil for a run the launch
// does not judge: a trusted run its credentials do not stamp keeps its
// refusals for the dispatch.
//
// The sites are model.CallSites — the one enumeration of the run's
// model-calling sites the usage-cap pre-flight keys too: every agent and
// judge node, an LLM router, a human node's LLM half, a Verified Action's
// recovery rungs, each node's `fallbacks:` routes and interaction
// generation, a review gate's companion and the declared supervisors. A site
// the node makes when it runs is refused unfunded; a rescue (a `fallbacks:`
// route, an interaction generation, the review companion, a supervisor) is
// warned — the site it rescues decides. A subbot's child is judged at its own
// start (the adopt branch, or its own resolved start).
func (a credentialAdmission) admit(ctx context.Context, emitEvent func(store.EventType, map[string]any) error, logger *iterlog.Logger) error {
	creds, _ := secrets.CredentialsFromContext(ctx)
	stamped := creds.AmbientRefused
	fork := !a.trust.Trusted()
	if !fork && !stamped {
		return nil
	}
	var refused []nodeVerdict
	var undecidable, warned []string
	backendOf := func(n ir.Node) string { return dispatchedBackendName(n, a.wf, a.resolver) }
	// Sorted by site id (CallSites), so the same run's refusal words its list
	// identically on every start, and its code is the first refused site's.
	for _, site := range model.CallSites(a.routes, backendOf) {
		switch {
		case site.Kind == model.SiteUnread:
			if _, child := site.Node.(*ir.SubbotNode); !child {
				undecidable = append(undecidable, fmt.Sprintf("%s (no reading of its model calls)", site.ID))
			}
			continue
		case site.Rescue:
			if w := a.judgeRescue(ctx, site); w != "" {
				warned = append(warned, w)
			}
			continue
		}
		verdict, err := a.judgeSite(ctx, site)
		switch {
		case errors.Is(err, credroute.ErrNoArm):
			// No arm decides this backend yet: a fork-lane sandbox keeps
			// today's per-backend probe; anything else is the warning's
			// territory — never a refusal on a guess.
			if !fork || !a.placement.Contained || a.adopted {
				undecidable = append(undecidable, fmt.Sprintf("%s (%s)", site.ID, site.Backend))
				break
			}
			if decided, ok := a.probeWithoutArm(ctx, site.Backend, siteFields(site.Node)); !decided {
				undecidable = append(undecidable, fmt.Sprintf("%s (%s)", site.ID, site.Backend))
			} else if !ok {
				refused = append(refused, nodeVerdict{node: site.ID, nodeID: site.NodeID, backend: site.Backend, refusal: &credroute.Refusal{
					Backend: site.Backend, Reason: credroute.ReasonNoCredential,
				}})
			}
		case verdict.refusal != nil:
			refused = append(refused, verdict)
		case verdict.spend.Channel == credroute.ChannelOwnConfig:
			// The backend reads its own configuration, never a run
			// credential (ADR-161 decision 5): admitted, and warned as a
			// site the launch cannot read.
			undecidable = append(undecidable, fmt.Sprintf("%s (%s)", site.ID, site.Backend))
		case logger != nil:
			logger.Info("runtime: node %s (%s): its credential is the %s%s", site.ID, site.Backend, verdict.spend.Channel, slotSuffix(verdict.spend))
		}
	}
	if len(undecidable) > 0 {
		reason := fmt.Sprintf("no credential reading for %s — the launch cannot tell whether a credential will reach those nodes", strings.Join(undecidable, ", "))
		warnCredential(emitEvent, logger, a.runID, reason, undecidable)
	}
	if len(warned) > 0 {
		reason := fmt.Sprintf("no credential funds %s — they cannot serve this run", strings.Join(warned, ", "))
		warnCredential(emitEvent, logger, a.runID, reason, warned)
	}
	if len(refused) == 0 {
		return nil
	}
	return a.refusal(refused, fork)
}

// siteFields is the LLM fields the per-backend probe reads of a dispatched
// node; nil for a site with none.
func siteFields(n ir.Node) *ir.LLMFields {
	switch node := n.(type) {
	case *ir.AgentNode:
		return &node.LLMFields
	case *ir.JudgeNode:
		return &node.LLMFields
	case *ir.RouterNode:
		return &node.LLMFields
	}
	return nil
}

// judgeSite resolves every leg of site's chain until one is funded, each
// where the site calls its model (model.CallSite.Call). ErrNoArm reports a
// backend no arm decides.
func (a credentialAdmission) judgeSite(ctx context.Context, site model.CallSite) (nodeVerdict, error) {
	verdict := nodeVerdict{node: site.ID, nodeID: site.NodeID, backend: site.Backend}
	if len(site.Legs) == 0 {
		// The allowlist dropped every route: the dispatch refuses the same
		// way (ErrProviderOutsideAllowlist).
		verdict.refusal = &credroute.Refusal{Backend: site.Backend, Reason: credroute.ReasonOutsideAllowlist, Cause: model.ErrProviderOutsideAllowlist}
		return verdict, nil
	}
	base, err := site.Base(a.detected)
	if err != nil {
		// The dispatch refuses the node before its first leg: the
		// detector's pick is outside the run's allowlist.
		verdict.refusal = &credroute.Refusal{Backend: site.Backend, Reason: credroute.ReasonOutsideAllowlist, Cause: err}
		return verdict, nil
	}
	for i, leg := range site.Legs {
		spec := site.LegSpec(i, base)
		spend, err := credroute.Resolve(ctx, site.Call(i, spec, a.placement))
		if errors.Is(err, credroute.ErrNoArm) {
			return nodeVerdict{}, err
		}
		if err == nil {
			verdict.refusal = nil
			verdict.spend = spend
			return verdict, nil
		}
		verdict.refusal = asRefusal(site.Backend, spec, leg, err)
	}
	return verdict, nil
}

// judgeRescue resolves a rescue site — a `fallbacks:` route, an interaction
// generation, the review companion, a pinned supervisor — and names it when
// no credential funds it: warned, never refused, the site it rescues decides.
// A supervisor naming no model takes the spawn's default, which the spawn
// decides; a site whose base the dispatch refuses (the detector's pick
// outside the allowlist) dispatches nothing to warn on.
func (a credentialAdmission) judgeRescue(ctx context.Context, site model.CallSite) string {
	if site.Kind == model.SiteSupervisor && site.Model == "" {
		return ""
	}
	base, err := site.Base(a.detected)
	if err != nil {
		return ""
	}
	spec := site.LegSpec(0, base)
	var leg credroute.Leg
	if len(site.Legs) > 0 {
		leg = site.Legs[0]
	}
	_, err = credroute.Resolve(ctx, site.Call(0, spec, a.placement))
	if err == nil || errors.Is(err, credroute.ErrNoArm) {
		return ""
	}
	reason := asRefusal(site.Backend, spec, leg, err).Reason
	switch site.Kind {
	case model.SiteSupervisor:
		return fmt.Sprintf("supervisor %q (%s: %s)", site.Name, spec, reason)
	case model.SiteFallback:
		return fmt.Sprintf("%s fallback %q (%s: %s)", site.NodeID, site.Name, site.Backend, reason)
	}
	return fmt.Sprintf("%s (%s: %s)", site.ID, site.Backend, reason)
}

// probeWithoutArm is today's per-backend reading of a sandboxed fork-lane
// node whose backend no arm decides yet (ADR-161 decision 9, interim):
// decided is false where no probe reads the backend, or the site exposes no
// fields to probe.
func (a credentialAdmission) probeWithoutArm(ctx context.Context, backend string, fields *ir.LLMFields) (decided, ok bool) {
	if fields == nil {
		return false, false
	}
	switch backend {
	case delegate.BackendClaudeCode:
		_, ok = delegate.ClaudeCodeSandboxCredential(ctx, fields.Provider, a.placement.Home)
		return true, ok
	case delegate.BackendPi:
		_, ok = delegate.PiSandboxCredential(ctx, fields.Model)
		return true, ok
	}
	return false, false
}

// asRefusal is err as the resolver's refusal; an arm's error that is none is
// a leg nothing funds, its cause kept.
func asRefusal(backend, spec string, leg credroute.Leg, err error) *credroute.Refusal {
	var r *credroute.Refusal
	if errors.As(err, &r) {
		return r
	}
	return &credroute.Refusal{Backend: backend, Spec: spec, Leg: leg, Reason: credroute.ReasonNoCredential, Cause: err}
}

func slotSuffix(s credroute.Spend) string {
	if s.Slot == "" {
		return ""
	}
	return " (" + s.Slot + ")"
}

// warnCredential says what the admission could not read, or what it read as
// unable to serve, on the run's stream and its log.
func warnCredential(emitEvent func(store.EventType, map[string]any) error, logger *iterlog.Logger, runID, reason string, names []string) {
	if logger != nil {
		logger.Warn("runtime: %s (run %s)", reason, runID)
	}
	if emitEvent != nil {
		_ = emitEvent(store.EventSandboxNoCredentialWarning, map[string]any{"reason": reason, "nodes": names})
	}
}

// admissionCode is the code a node refused for reason carries (ADR-161
// decision 7).
func admissionCode(reason credroute.Reason) ErrorCode {
	switch reason {
	case credroute.ReasonAmbientStamp, credroute.ReasonArmPending:
		return ErrCodeCredentialAmbientRefused
	case credroute.ReasonSubscriptionSwitch:
		return ErrCodeCredentialSubscriptionForbidden
	}
	return ErrCodeSandboxCredentialUnreachable
}

// refusal is the launch's typed refusal naming every refused node, sorted.
// Its code is the first node's (decision 7's code for its reason): one run,
// one code, and the message names each node with its own reason.
func (a credentialAdmission) refusal(refused []nodeVerdict, fork bool) error {
	parts := make([]string, 0, len(refused))
	for _, r := range refused {
		parts = append(parts, fmt.Sprintf("%s (%s: %s)", r.node, r.backend, r.refusal.Reason))
	}
	first := refused[0]
	code := admissionCode(first.refusal.Reason)
	nodeID := first.nodeID
	if nodeID == "" {
		nodeID = first.node
	}
	who := "run stamped to its own credentials"
	if fork {
		who = "untrusted (fork-lane) run"
	}
	where := "before its nodes start"
	if a.placement.Contained {
		where = "before its sandbox starts"
	}
	switch {
	case code == ErrCodeCredentialAmbientRefused && first.refusal.Reason == credroute.ReasonArmPending:
		return &RuntimeError{
			Code:    code,
			NodeID:  nodeID,
			Cause:   first.refusal,
			Message: fmt.Sprintf("%s refused %s: node(s) %s run on a backend whose credential arm has not landed (ADR-161 U4) — its dispatch would read the pod's credentials past the run's seal, and the run's credentials are its only funds (ADR-131 §7)", who, where, strings.Join(parts, ", ")),
			Hint:    "route each named node to claw or claude_code on a model the run's own credentials serve (a BYOK key, a pinned key, a connected forfait, a gateway pair) — pi and codex legs are refused on a run stamped to its own credentials until their arms land; an unstamped run keeps them.",
		}
	case code == ErrCodeCredentialAmbientRefused:
		return &RuntimeError{
			Code:    code,
			NodeID:  nodeID,
			Cause:   first.refusal,
			Message: fmt.Sprintf("%s refused %s: only the pod's environment would fund node(s) %s, and the run's credentials are its only funds (ADR-131 §7)", who, where, strings.Join(parts, ", ")),
			Hint:    "attach a credential for each named route to the run (a BYOK key, a pinned key, a connected forfait, a gateway pair), or route the node to a model the run's own credentials serve — the stamp refuses the pod's keys and logins by design. A backend that reads its own configuration (kimi, grok, opencode) never spends a run credential: route its node to claw or claude_code.",
		}
	case code == ErrCodeCredentialSubscriptionForbidden:
		return &RuntimeError{
			Code:    code,
			NodeID:  nodeID,
			Cause:   first.refusal,
			Message: fmt.Sprintf("%s refused %s: ITERION_FORBID_SUBSCRIPTION_OAUTH refuses the subscription node(s) %s would spend, and no other credential funds them", who, where, strings.Join(parts, ", ")),
			Hint:    "give the run a metered API key for each named route, route the node to claude_code (the vendor's own CLI spends the subscription within policy), or lift ITERION_FORBID_SUBSCRIPTION_OAUTH on this deployment.",
		}
	}
	message := fmt.Sprintf("%s refused %s: no credential can reach node(s) %s — no run credential, nothing the launcher forwards at exec, and no file-based login the host_state mount still carries",
		who, where, strings.Join(parts, ", "))
	return &RuntimeError{
		Code:    ErrCodeSandboxCredentialUnreachable,
		NodeID:  nodeID,
		Cause:   first.refusal,
		Message: message,
		Hint:    "hand the run a credential that crosses the container boundary: export the API key the backend forwards (e.g. ANTHROPIC_API_KEY / OPENAI_API_KEY — forwarded by name at exec), or — on a runner whose host_state still mounts the operator's login — keep host_state: auto. A credential declared `as: file` reaches the container's filesystem, not the backend CLI's auth, so it does not lift this refusal. The operator's own OAuth token is never handed to a fork's container by iterion itself.",
	}
}
