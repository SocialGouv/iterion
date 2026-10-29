package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fixedResolver answers one backend for every node, the way a launch-time
// `--backend` override or the host's default does.
type fixedResolver string

func (r fixedResolver) EffectiveBackendName(ir.Node) string { return string(r) }

func grantWorkflow(sinks []string, nodes ...ir.Node) *ir.Workflow {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{}, Secrets: map[string]*ir.Secret{"tok": {Name: "tok", As: "value", Sinks: sinks}}}
	for i, n := range nodes {
		wf.Nodes[string(rune('a'+i))] = n
	}
	return wf
}

func agentOn(backend string) *ir.AgentNode {
	n := &ir.AgentNode{}
	n.Backend = backend
	return n
}

// The launch gate of ADR-146 (axis 3b), on its own: a declared secret whose
// grants resolve at no sink this run serves is refused, typed, naming the
// secret, its grants and the path that could not serve each. `command`,
// `params` and `file` are served by every run (the host materialises a file
// secret too); `egress` by an inspecting sandbox; `agent` by a claude_code
// agent or judge node, or in-process claw unsandboxed; `prompt` counts for
// nothing on its own; a secret with no `sinks:` is never refused, nor an
// optional one the run does not hold, nor anything on a dry run.
func TestRefuseUnservableSecretGrantsReadsTheRunsService(t *testing.T) {
	cases := []struct {
		name     string
		wf       *ir.Workflow
		resolver effectiveBackendResolver
		svc      secretSinkService
		refused  bool
		path     string
	}{
		{"no sinks, unsandboxed", grantWorkflow(nil), nil, secretSinkService{}, false, ""},
		{"no sinks, sandboxed without inspection", grantWorkflow(nil), nil, secretSinkService{sandboxed: true}, false, ""},
		{"file on an unsandboxed run is materialised on the host", grantWorkflow([]string{secretsink.File}), nil, secretSinkService{}, false, ""},
		{"file on a real driver", grantWorkflow([]string{secretsink.File}), nil, secretSinkService{sandboxed: true}, false, ""},
		{"egress on an unsandboxed run", grantWorkflow([]string{secretsink.Egress}), nil, secretSinkService{}, true, "no inspecting proxy"},
		{"egress on a sandbox that does not inspect", grantWorkflow([]string{secretsink.Egress}), nil, secretSinkService{sandboxed: true}, true, "substitutes nothing on the wire"},
		{"egress on an inspecting sandbox", grantWorkflow([]string{secretsink.Egress}), nil, secretSinkService{sandboxed: true, egress: true}, false, ""},
		{"prompt and egress, unsandboxed", grantWorkflow([]string{secretsink.Prompt, secretsink.Egress}), nil, secretSinkService{}, true, "egress:"},
		{"agent, unsandboxed, in-process claw", grantWorkflow([]string{secretsink.Agent}, agentOn("claw")), nil, secretSinkService{}, false, ""},
		{"agent, unsandboxed, an unbacked node reads as claw", grantWorkflow([]string{secretsink.Agent}, agentOn("")), nil, secretSinkService{}, false, ""},
		{"agent, sandboxed, every node claw", grantWorkflow([]string{secretsink.Agent}, agentOn("claw")), nil, secretSinkService{sandboxed: true}, true, "in-container runner"},
		{"agent, sandboxed, a claude_code node declared", grantWorkflow([]string{secretsink.Agent}, agentOn("claw"), agentOn("claude_code")), nil, secretSinkService{sandboxed: true}, false, ""},
		{"agent, sandboxed, the resolver routes unbacked nodes to claude_code", grantWorkflow([]string{secretsink.Agent}, agentOn("")), fixedResolver("claude_code"), secretSinkService{sandboxed: true}, false, ""},
		{"agent, sandboxed, the resolver overrides a declared claude_code with claw", grantWorkflow([]string{secretsink.Agent}, agentOn("claude_code")), fixedResolver("claw"), secretSinkService{sandboxed: true}, true, "in-container runner"},
		{"agent, every node codex", grantWorkflow([]string{secretsink.Agent}, agentOn("codex")), nil, secretSinkService{}, true, "codex, pi, kimi, grok and opencode never do"},
		{"agent, a router only", grantWorkflow([]string{secretsink.Agent}, &ir.RouterNode{}), nil, secretSinkService{}, true, "router carries no materialiser"},
		{"one served grant is enough", grantWorkflow([]string{secretsink.Egress, secretsink.Command}), nil, secretSinkService{}, false, ""},
		{"an optional secret the run does not hold", &ir.Workflow{Secrets: map[string]*ir.Secret{"tok": {Name: "tok", As: "value", Optional: true, Sinks: []string{secretsink.Egress}}}}, nil, secretSinkService{}, false, ""},
		{"an optional secret the run holds is judged", &ir.Workflow{Secrets: map[string]*ir.Secret{"tok": {Name: "tok", As: "value", Optional: true, Sinks: []string{secretsink.Egress}}}}, nil, secretSinkService{held: func(string) bool { return true }}, true, "no inspecting proxy"},
		{"a dry run is never judged", grantWorkflow([]string{secretsink.Egress}), nil, secretSinkService{simulation: true}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseUnservableSecretGrants(tc.wf, tc.resolver, tc.svc)
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			detail, ok := IsSecretGrantUnservable(err)
			if !ok {
				t.Fatalf("not refused as SecretGrantUnservableError: %v", err)
			}
			var rt *RuntimeError
			if !errors.As(err, &rt) || rt.Code != ErrCodeCapabilityUnsupported {
				t.Fatalf("the refusal is not a RuntimeError coded CAPABILITY_UNSUPPORTED: %v", err)
			}
			if detail.Secret() != "tok" || strings.Join(detail.Secrets[0].Sinks, ",") != strings.Join(tc.wf.Secrets["tok"].Sinks, ",") {
				t.Errorf("the refusal names secret %q sinks %v, want tok %v", detail.Secret(), detail.Secrets[0].Sinks, tc.wf.Secrets["tok"].Sinks)
			}
			if !strings.Contains(strings.Join(detail.Secrets[0].Paths, "; "), tc.path) {
				t.Errorf("the refusal's paths %v do not name %q", detail.Secrets[0].Paths, tc.path)
			}
		})
	}
	// Every refused secret is named at once: two bad declarations need one
	// failed launch, not two.
	two := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"a": {Name: "a", As: "value", Sinks: []string{secretsink.Egress}},
		"b": {Name: "b", As: "value", Sinks: []string{secretsink.Egress}},
	}}
	detail, ok := IsSecretGrantUnservable(refuseUnservableSecretGrants(two, nil, secretSinkService{}))
	if !ok || len(detail.Secrets) != 2 || detail.Secrets[0].Secret != "a" || detail.Secrets[1].Secret != "b" {
		t.Fatalf("two unservable secrets were not both named: %+v", detail)
	}
}

// tlsCapableDriver is the canary driver claiming TLS-inspection support, so
// the launch counts the `egress` grant as served; it still refuses to
// Start, which is how a test tells "the launch went on" from "refused".
type tlsCapableDriver struct{ specCapturingDriver }

func (d *tlsCapableDriver) Capabilities() sandbox.Capabilities {
	c := d.specCapturingDriver.Capabilities()
	c.SupportsTLSInspection = true
	return c
}

func secretGrantParams(t *testing.T, wf *ir.Workflow) SandboxParams {
	t.Helper()
	return SandboxParams{
		Workflow: wf, RunID: "run-grants", WorkspacePath: t.TempDir(), RepoRoot: t.TempDir(),
		DefaultImage: "example.invalid/iterion-sandbox:test",
		Logger:       iterlog.Nop(),
		EmitEvent:    func(store.EventType, map[string]any) error { return nil },
	}
}

// Through the real launch path: a sandboxed bundle whose secret is granted
// `egress` alone is refused before anything starts unless BOTH facts hold —
// a rewriter is wired and the driver injects the CA — and goes on to
// Prepare when they do; an unsandboxed run refuses the same grant, serves a
// `file` grant, never judges a secret with no `sinks:`, and a dry run is
// never judged. Mutation: make refuseUnservableSecretGrants return nil →
// every refusal turns into a launch that carries a placeholder nothing
// resolves, red on the typed-error assertion.
func TestALaunchRefusesASecretWhoseGrantsItsSandboxCannotServe(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory preference to docker,podman,noop
	egressOnly := func(mode string) *ir.Workflow {
		return &ir.Workflow{
			Name:    "wf",
			Sandbox: &ir.SandboxSpec{Mode: mode, Image: "example.invalid/sbx:test", HostState: "none"},
			Secrets: map[string]*ir.Secret{"tok": {Name: "tok", As: "value", Sinks: []string{secretsink.Egress}}},
		}
	}
	withDriver := func(p SandboxParams, d sandbox.Driver) SandboxParams {
		p.Drivers = map[string]sandbox.DriverConstructor{"docker": func() (sandbox.Driver, error) { return d, nil }}
		return p
	}
	var events []store.EventType
	recording := func(ev store.EventType, _ map[string]any) error { events = append(events, ev); return nil }

	t.Run("unsandboxed, egress alone", func(t *testing.T) {
		active, err := resolveAndStartSandbox(context.Background(), secretGrantParams(t, egressOnly("none")))
		if detail, ok := IsSecretGrantUnservable(err); !ok || detail.Secret() != "tok" || active != nil {
			t.Fatalf("unsandboxed run with an egress-only secret: active=%v err=%v, want the typed refusal naming tok", active, err)
		}
	})
	t.Run("unsandboxed, file alone is served on the host", func(t *testing.T) {
		wf := egressOnly("none")
		wf.Secrets["tok"].As = "file"
		wf.Secrets["tok"].Sinks = []string{secretsink.File}
		active, err := resolveAndStartSandbox(context.Background(), secretGrantParams(t, wf))
		if err != nil || active != nil {
			t.Fatalf("a file-only secret on an unsandboxed run: active=%v err=%v, want the launch to go on (the host materialises it)", active, err)
		}
	})
	t.Run("sandboxed, rewriter wired, driver cannot inject the CA", func(t *testing.T) {
		p := withDriver(secretGrantParams(t, egressOnly("inline")), &specCapturingDriver{hostBinds: true})
		p.SecretRewriter = &recordingRewriter{}
		p.EmitEvent = recording
		events = nil
		_, err := resolveAndStartSandbox(context.Background(), p)
		if detail, ok := IsSecretGrantUnservable(err); !ok || !strings.Contains(strings.Join(detail.Secrets[0].Paths, ";"), "substitutes nothing on the wire") {
			t.Fatalf("a driver without TLS inspection and an egress-only secret: err=%v, want the typed refusal naming the wire", err)
		}
		for _, ev := range events {
			if ev == store.EventSandboxHostStateMounted || ev == store.EventSandboxDevboxProvisioned {
				t.Errorf("a refused launch emitted %s for a sandbox that never starts", ev)
			}
		}
	})
	t.Run("sandboxed, driver injects the CA, no rewriter wired", func(t *testing.T) {
		p := withDriver(secretGrantParams(t, egressOnly("inline")), &tlsCapableDriver{specCapturingDriver{hostBinds: true}})
		_, err := resolveAndStartSandbox(context.Background(), p)
		if detail, ok := IsSecretGrantUnservable(err); !ok || !strings.Contains(strings.Join(detail.Secrets[0].Paths, ";"), "substitutes nothing on the wire") {
			t.Fatalf("no rewriter and an egress-only secret: err=%v, want the typed refusal naming the wire", err)
		}
	})
	t.Run("sandboxed, inspecting proxy", func(t *testing.T) {
		p := withDriver(secretGrantParams(t, egressOnly("inline")), &tlsCapableDriver{specCapturingDriver{hostBinds: true}})
		p.SecretRewriter = &recordingRewriter{}
		_, err := resolveAndStartSandbox(context.Background(), p)
		if _, refused := IsSecretGrantUnservable(err); refused {
			t.Fatalf("an inspecting sandbox serves egress, yet the launch was refused: %v", err)
		}
		if !errors.Is(err, errCanaryStop) {
			t.Fatalf("the launch did not reach the driver's Prepare: %v", err)
		}
	})
	t.Run("sandboxed, agent alone, every node claw", func(t *testing.T) {
		wf := egressOnly("inline")
		wf.Secrets["tok"].Sinks = []string{secretsink.Prompt, secretsink.Agent}
		wf.Nodes = map[string]ir.Node{"a": agentOn("claw")}
		p := withDriver(secretGrantParams(t, wf), &tlsCapableDriver{specCapturingDriver{hostBinds: true}})
		_, err := resolveAndStartSandbox(context.Background(), p)
		if detail, ok := IsSecretGrantUnservable(err); !ok || !strings.Contains(strings.Join(detail.Secrets[0].Paths, ";"), "in-container runner") {
			t.Fatalf("a sandboxed all-claw run with an agent-only secret: err=%v, want the typed refusal naming the runner", err)
		}
		p.EffectiveBackend = fixedResolver("claude_code")
		if _, refused := IsSecretGrantUnservable(func() error { _, err := resolveAndStartSandbox(context.Background(), p); return err }()); refused {
			t.Fatal("the resolver routes the node to claude_code, yet the agent grant was refused")
		}
	})
	t.Run("a dry run is never judged", func(t *testing.T) {
		p := secretGrantParams(t, egressOnly("none"))
		p.Simulation = true
		if active, err := resolveAndStartSandbox(context.Background(), p); err != nil || active != nil {
			t.Fatalf("a simulation was judged: active=%v err=%v", active, err)
		}
	})
	t.Run("no sinks declared is never refused", func(t *testing.T) {
		wf := egressOnly("none")
		wf.Secrets["tok"].Sinks = nil
		active, err := resolveAndStartSandbox(context.Background(), secretGrantParams(t, wf))
		if err != nil || active != nil {
			t.Fatalf("a secret with no sinks: active=%v err=%v, want the unsandboxed branch to go on", active, err)
		}
	})
	t.Run("a nil workflow with an active spec does not panic", func(t *testing.T) {
		p := withDriver(secretGrantParams(t, nil), &specCapturingDriver{hostBinds: true})
		p.CLIOverride = "auto"
		if _, err := resolveAndStartSandbox(context.Background(), p); err != nil && !errors.Is(err, errCanaryStop) {
			t.Logf("nil workflow: %v", err)
		}
	})
}

// A child adopting its parent's sandbox goes out through the parent's proxy
// — whose rewriter knows nothing of the child's secrets — and mounts
// nothing of its own: the service it is judged against serves `agent`
// through a claude_code node and nothing else the sandbox would.
func TestAnAdoptedChildIsJudgedAgainstTheParentsSandbox(t *testing.T) {
	child := grantWorkflow([]string{secretsink.Prompt, secretsink.Egress}, agentOn("claude_code"))
	if _, refused := IsSecretGrantUnservable(refuseUnservableSecretGrants(child, nil, secretSinkService{sandboxed: true})); !refused {
		t.Fatal("a child's egress-only secret was accepted in a shared sandbox whose rewriter cannot serve it")
	}
	child.Secrets["tok"].Sinks = []string{secretsink.Prompt, secretsink.Agent}
	if err := refuseUnservableSecretGrants(child, nil, secretSinkService{sandboxed: true}); err != nil {
		t.Fatalf("a child's agent grant on a claude_code node was refused in the shared sandbox: %v", err)
	}
}
