package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// SecretGrantUnservableError is the launch refusal of ADR-146 (axis 3b): a
// declared secret whose `sinks:` grants no sink this run can serve — the
// placeholder would stand in a command, a prompt or a request with nothing
// to resolve it, and the run would fail later, somewhere nobody is watching
// a launch. It names every such secret, with its grants and, per granted
// sink, the path that could not serve it. Carried as the Cause of a
// RuntimeError coded CAPABILITY_UNSUPPORTED, so `errors.As` reads the
// detail and the run record keeps an existing code.
type SecretGrantUnservableError struct {
	Secrets []UnservableSecret
}

// UnservableSecret is one refused declaration.
type UnservableSecret struct {
	Secret string
	Sinks  []string
	Paths  []string
}

// Secret is the first refused declaration's name — the short reading of a
// refusal that names several.
func (e *SecretGrantUnservableError) Secret() string {
	if e == nil || len(e.Secrets) == 0 {
		return ""
	}
	return e.Secrets[0].Secret
}

func (e *SecretGrantUnservableError) Error() string {
	parts := make([]string, 0, len(e.Secrets))
	for _, s := range e.Secrets {
		parts = append(parts, fmt.Sprintf("secret %q grants sinks [%s], none of which this run can serve: %s", s.Secret, strings.Join(s.Sinks, ", "), strings.Join(s.Paths, "; ")))
	}
	return strings.Join(parts, " / ")
}

// secretSinkService is what the run being launched can serve, decided where
// the sandbox is resolved — the one place that knows the driver, whether the
// egress proxy inspects, and where the model nodes run.
type secretSinkService struct {
	// sandboxed: a real driver runs the nodes in a container (the noop driver
	// runs them on the host).
	sandboxed bool
	// egress: the inspecting proxy substitutes placeholders on the wire
	// (Layer 2) — a rewriter is wired and the driver injects the CA.
	egress bool
	// simulation: a dry run, whose sandbox is the simulation's and not the
	// run's — never judged.
	simulation bool
	// held reports whether a declared secret resolved to something this run
	// carries; nil reads every optional secret as absent.
	held func(name string) bool
}

// serves reports whether the run serves sink, and names the path that could
// not when it does not. `prompt` resolves nothing on its own and is never a
// service; the compiler refuses a list that names it alone (C321).
func (s secretSinkService) serves(sink string, wf *ir.Workflow, resolver effectiveBackendResolver) (bool, string) {
	switch sink {
	case secretsink.Command, secretsink.Params:
		// Materialised on the host into the exec that runs the body —
		// unsandboxed, or through the sandbox's exec environment.
		return true, ""
	case secretsink.File:
		// Mounted by a real driver; materialised on the host by the executor
		// (ensureHostSecretFiles) or the runner for an unsandboxed run. The
		// noop driver, which mounts nothing, is refused by addSecretFileMounts.
		return true, ""
	case secretsink.Agent:
		if agentServed(wf, resolver, s.sandboxed) {
			return true, ""
		}
		if s.sandboxed {
			return false, "agent: no agent or judge node of this run resolves to claude_code, the one backend whose tool input is materialised from the host in a sandbox (a sandboxed claw runs through the in-container runner, which materialises nothing; codex, pi, kimi, grok and opencode never do)"
		}
		return false, "agent: no agent or judge node of this run resolves to claude_code or in-process claw, the backends whose tool input is materialised (codex, pi, kimi, grok and opencode never do; a router carries no materialiser)"
	case secretsink.Egress:
		if !s.sandboxed {
			return false, "egress: an unsandboxed run has no inspecting proxy to substitute the placeholder on the wire (Layer 2 needs a sandbox)"
		}
		if !s.egress {
			return false, "egress: this run's sandbox substitutes nothing on the wire (TLS inspection off — ITERION_SANDBOX_TLS_INSPECT — or a driver that cannot inject the proxy's CA)"
		}
		return true, ""
	}
	return false, sink + ": not a materialization sink"
}

// refuseUnservableSecretGrants is the launch gate behind a secret's
// `sinks:`: a declared secret whose grants resolve at no sink this run can
// serve fails the launch, typed, naming every such secret and the paths —
// never a run that carries a placeholder nothing resolves. A secret that
// declares no `sinks:` grants every sink, two of which (`command`, `params`)
// every run serves: it is never refused here, exactly as before the grants;
// nor is an optional secret the run does not hold (a launch that must
// proceed without it is not refused for the sinks it would have reached);
// nor anything on a dry run, whose sandbox is the simulation's.
//
// One check, above the backend choice: `claude_code`, in-process `claw`
// and sandboxed `claw` reach the same verdict for the same declaration on
// the same sandbox, the way the compiler reaches one verdict for a
// reference.
func refuseUnservableSecretGrants(wf *ir.Workflow, resolver effectiveBackendResolver, svc secretSinkService) error {
	if wf == nil || len(wf.Secrets) == 0 || svc.simulation {
		return nil
	}
	names := make([]string, 0, len(wf.Secrets))
	for name := range wf.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	var refused []UnservableSecret
	for _, name := range names {
		s := wf.Secrets[name]
		if s == nil || len(s.Sinks) == 0 {
			continue
		}
		if s.Optional && (svc.held == nil || !svc.held(name)) {
			continue
		}
		var paths []string
		served := false
		for _, sink := range s.Sinks {
			if sink == secretsink.Prompt {
				continue
			}
			ok, why := svc.serves(sink, wf, resolver)
			if ok {
				served = true
				break
			}
			paths = append(paths, why)
		}
		if served {
			continue
		}
		refused = append(refused, UnservableSecret{Secret: name, Sinks: append([]string(nil), s.Sinks...), Paths: paths})
	}
	if len(refused) == 0 {
		return nil
	}
	named := make([]string, 0, len(refused))
	for _, r := range refused {
		named = append(named, r.Secret)
	}
	return &RuntimeError{
		Code:    ErrCodeCapabilityUnsupported,
		Message: fmt.Sprintf("the grants of %d declared secret(s) cannot be served on this run (%s)", len(refused), strings.Join(named, ", ")),
		Hint:    "grant each named secret a sink this run serves (`sinks:` in its declaration — docs/secrets.md, « sinks »): `egress` needs a sandbox whose proxy inspects TLS, `agent` an agent or judge node on claude_code (or in-process claw, unsandboxed)",
		Cause:   &SecretGrantUnservableError{Secrets: refused},
	}
}

// agentServed reports whether some agent or judge node of the workflow
// runs on a backend that materialises the placeholders a model emits in its
// tool input: claude_code (its PreToolUse hook runs on the host, sandboxed
// or not) and in-process claw (unsandboxed only — a sandboxed claw runs
// through the in-container runner, where the materialiser does not cross).
// A router carries no materialiser; codex, pi, kimi, grok and opencode
// never read one. The backend is read the way dispatch reads it: the
// resolver's answer first, then the node's own spelling, then the
// workflow's default.
func agentServed(wf *ir.Workflow, resolver effectiveBackendResolver, sandboxed bool) bool {
	if wf == nil {
		return false
	}
	for _, n := range wf.Nodes {
		var declared string
		switch node := n.(type) {
		case *ir.AgentNode:
			declared = node.Backend
		case *ir.JudgeNode:
			declared = node.Backend
		default:
			continue
		}
		name := ""
		if resolver != nil {
			name = resolver.EffectiveBackendName(n)
		}
		if strings.TrimSpace(name) == "" {
			name = declared
		}
		if strings.TrimSpace(name) == "" {
			name = wf.DefaultBackend
		}
		name = strings.ToLower(strings.TrimSpace(ir.ExpandEnvWithDefault(name)))
		switch name {
		case delegate.BackendClaudeCode:
			return true
		case "", delegate.BackendClaw:
			if !sandboxed {
				return true
			}
		}
	}
	return false
}

// IsSecretGrantUnservable reports whether err carries the launch refusal of
// a secret's grants, and returns it.
func IsSecretGrantUnservable(err error) (*SecretGrantUnservableError, bool) {
	var e *SecretGrantUnservableError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
