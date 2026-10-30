package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sandbox_credential_admission.go is the fork-lane credential admission of
// #1768: an untrusted run executes only inside a per-run sandbox (the
// PR2b floor, admission.go), and a sandboxed node whose backend has NO
// credential channel is certain to die mid-run on the backend's opaque
// "not logged in" — hours in, after the review's expensive half. The
// launch refuses instead, typed, naming the backend and the ways out.
//
// The answer is a PER-BACKEND predicate read from what the real spawn
// delivers — including what the backend forwards at EXEC time (the
// launcher env forwarded by name, the forfait copied at dispatch), which
// a spec-level check cannot see; that exec-time invisibility is why the
// spec-level signal is a warning and this refusal lives here, where the
// sandbox, the driver and the host_state resolution are known and nothing
// has started yet.
//
// Deliberately under-refusing, like admitSandboxImageBackends:
//   - trusted runs are never judged (the fork lane is the scope; a trusted
//     operator run keeps its "Not logged in" as before);
//   - a node whose backend the predicate cannot read (an unknown name, an
//     undecided route, a {{vars.…}} the dispatch resolves) is a WARNING
//     (sandbox_no_credential_warning), never a refusal on a guess;
//   - the host_state channel counts while it is mounted: nothing on main
//     floors host_state for an untrusted run yet, so a local file-based
//     login still reaches a docker sandbox — the day a floor withdraws it
//     (the lane's design), this refusal is what tells the operator WHY the
//     run cannot launch, instead of the run dying in the container.

// sandboxCredentialHome resolves what this sandbox will actually bind of
// the operator's home: the resolved host_state, the driver's bind
// capability, and the planted-home refusal — the same inputs
// applyHostStateMounts will apply below, so the probe and the mount plan
// cannot disagree.
func sandboxCredentialHome(p SandboxParams, caps sandbox.Capabilities) delegate.SandboxHomeState {
	resolved, _ := pickHostState(workflowHostState(p.Workflow), p.HostStateOverride, p.HostStateDefault)
	if !sandbox.HostState(resolved).Active() || !caps.SupportsHostBindMounts {
		return delegate.SandboxHomeState{}
	}
	homeDir, planted := resolveHostHomeDir()
	if planted || homeDir == "" {
		return delegate.SandboxHomeState{}
	}
	return delegate.SandboxHomeState{Mounted: true, HomeDir: homeDir}
}

// admitSandboxCredentials refuses an untrusted run whose sandboxed agent
// nodes include one whose backend provably has no credential channel.
// Called after the driver is selected and before any pull, mount or
// start. Nil (admitted) for a trusted run, a run with no decidable gap,
// and a simulation (whose sandbox is not the run's).
func admitSandboxCredentials(ctx context.Context, p SandboxParams, caps sandbox.Capabilities, emitEvent func(store.EventType, map[string]any) error, logger *iterlog.Logger) error {
	if p.Trust.Trusted() || p.Simulation || p.Workflow == nil {
		return nil
	}
	home := sandboxCredentialHome(p, caps)
	type gap struct {
		node    string
		backend string
	}
	var refused []gap
	var undecidable []gap
	for _, n := range p.Workflow.Nodes {
		nn, ok := n.(ir.LLMNode)
		if !ok {
			continue
		}
		backend := dispatchedBackendName(n, p.Workflow, p.EffectiveBackend)
		fields := nn.GetLLMFields()
		var channel string
		var decidable bool
		switch backend {
		case delegate.BackendClaudeCode:
			channel, decidable = delegate.ClaudeCodeSandboxCredential(ctx, fields.Provider, home)
		case delegate.BackendClaw:
			channel, decidable = model.ClawSandboxCredential(ctx, fields.Model, home)
		case delegate.BackendPi:
			channel, decidable = delegate.PiSandboxCredential(ctx, fields.Model)
		default:
			// "", an undecided name, a backend without a predicate: the
			// warning's territory — never a refusal on a guess.
			undecidable = append(undecidable, gap{node: n.NodeID(), backend: backend})
			continue
		}
		if !decidable {
			refused = append(refused, gap{node: n.NodeID(), backend: backend})
		} else if logger != nil {
			logger.Info("runtime: fork-lane node %s (%s): credential reaches the sandbox via %s", n.NodeID(), backend, channel)
		}
	}
	if len(undecidable) > 0 {
		slices.SortFunc(undecidable, func(a, b gap) int { return strings.Compare(a.node, b.node) })
		names := make([]string, 0, len(undecidable))
		for _, u := range undecidable {
			names = append(names, fmt.Sprintf("%s (%s)", u.node, u.backend))
		}
		reason := fmt.Sprintf("untrusted run: no per-backend credential reading for %s — the launch cannot tell whether a credential will reach those sandboxed nodes", strings.Join(names, ", "))
		if logger != nil {
			logger.Warn("runtime: %s (run %s)", reason, p.RunID)
		}
		if emitEvent != nil {
			_ = emitEvent(store.EventSandboxNoCredentialWarning, map[string]any{"reason": reason, "nodes": names})
		}
	}
	if len(refused) == 0 {
		return nil
	}
	// Workflow.Nodes is a map: sorted, so the same run's refusal words its
	// list identically on every start.
	slices.SortFunc(refused, func(a, b gap) int { return strings.Compare(a.node, b.node) })
	parts := make([]string, 0, len(refused))
	for _, r := range refused {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.node, r.backend))
	}
	return &RuntimeError{
		Code:   ErrCodeSandboxCredentialUnreachable,
		NodeID: refused[0].node,
		Message: fmt.Sprintf(
			"untrusted (fork-lane) run refused before its sandbox starts: no credential can reach sandboxed node(s) %s — no run credential, nothing the launcher forwards at exec, and no file-based login the host_state mount still carries",
			strings.Join(parts, ", ")),
		Hint: "hand the run a credential that crosses the container boundary: export the API key the backend forwards (e.g. ANTHROPIC_API_KEY / OPENAI_API_KEY — forwarded by name at exec), or — on a runner whose host_state still mounts the operator's login — keep host_state: auto. A credential declared `as: file` reaches the container's filesystem, not the backend CLI's auth, so it does not lift this refusal. The operator's own OAuth token is never handed to a fork's container by iterion itself.",
	}
}
