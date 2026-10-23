package model

// A policy-isolated sandbox only lets egress through the run's network
// proxy. The exec env assembled for the in-container runner carries nothing
// of the container's own env, so the proxy endpoint must ride the exec env
// explicitly — or the runner's model registry dials its provider directly
// and drops on the network policy (measured in prod, 2026-10-08: every
// honorabilite review parked on `dial tcp 79.137.10.7:443: i/o timeout`
// while the same pod's own curls succeeded).

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// proxiedFakeRun satisfies sandbox.Run and answers ProxyEndpoint only when
// endpoint is set — the shapes sandboxProxyEnv must tell apart: proxied,
// policy-less, and a plain non-ProxiedRun handle.
type proxiedFakeRun struct{ endpoint string }

func (f proxiedFakeRun) Driver() string { return "kubernetes" }

func (f proxiedFakeRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	return nil
}

func (f proxiedFakeRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (f proxiedFakeRun) Cleanup(ctx context.Context) error { return nil }

func (f proxiedFakeRun) RefreshSecretFile(ctx context.Context, name string, value []byte) error {
	return nil
}

func (f proxiedFakeRun) ExportWorkspace(ctx context.Context) error { return nil }

func (f proxiedFakeRun) CaptureWorkspaceHead(ctx context.Context) (string, error) {
	return "", nil
}

func (f proxiedFakeRun) ProxyEndpoint() string { return f.endpoint }

func TestSandboxProxyEnv_ProxiedSandboxCarriesTheEndpoint(t *testing.T) {
	env := sandboxProxyEnv(proxiedFakeRun{endpoint: "http://tok@10.2.200.59:41625"})
	if env[compatgw.SandboxProxyEndpointEnv] != "http://tok@10.2.200.59:41625" {
		t.Fatalf("%s = %q, want the endpoint — the gateway would dial direct and drop on the policy", compatgw.SandboxProxyEndpointEnv, env[compatgw.SandboxProxyEndpointEnv])
	}
	if len(env) != 1 {
		t.Fatalf("env = %v, want EXACTLY the engine-owned key — no ambient HTTPS_PROXY may ride (the gateway never trusts it)", env)
	}
}

func TestSandboxProxyEnv_PolicyLessSandboxUnsetsTheName(t *testing.T) {
	env := sandboxProxyEnv(proxiedFakeRun{endpoint: ""})
	if env[compatgw.SandboxProxyEndpointEnv] != "" {
		t.Fatalf("policy-less sandbox env = %v, want the key UNSET — the container's own env must not speak for the engine", env)
	}
	if len(env) != 1 {
		t.Fatalf("env = %v, want only the unset entry", env)
	}
}

// A hostile image or spec env can forge the engine's voice on a run that is
// not proxied: the fold must UNSET the name, because the exec env rides the
// container's own and the runner would otherwise trust it (rva Rd67587).
func TestSandboxProxyEnv_NonProxiedRunUnsetsTheName(t *testing.T) {
	var plain sandbox.Run = nonProxiedFakeRun{}
	env := sandboxProxyEnv(plain)
	if env[compatgw.SandboxProxyEndpointEnv] != "" {
		t.Fatalf("non-proxied handle env = %v, want the key UNSET — a forged ambient value must not reach the runner", env)
	}
	if len(env) != 1 {
		t.Fatalf("env = %v, want only the unset entry", env)
	}
}

// nonProxiedFakeRun is a sandbox.Run that does NOT implement ProxiedRun —
// methods rewritten, NOT embedded: embedding would promote ProxyEndpoint()
// and the assertion would pass for the wrong reason (rva F5).
type nonProxiedFakeRun struct{}

func (f nonProxiedFakeRun) Driver() string { return "noop" }

func (f nonProxiedFakeRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	return nil
}

func (f nonProxiedFakeRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (f nonProxiedFakeRun) Cleanup(ctx context.Context) error { return nil }

func (f nonProxiedFakeRun) RefreshSecretFile(ctx context.Context, name string, value []byte) error {
	return nil
}

func (f nonProxiedFakeRun) ExportWorkspace(ctx context.Context) error { return nil }

func (f nonProxiedFakeRun) CaptureWorkspaceHead(ctx context.Context) (string, error) {
	return "", nil
}

// hostRunFakeRun is the noop passthrough shape: commands resolve on the
// operator's host, where no container network policy exists to bypass.
type hostRunFakeRun struct{ nonProxiedFakeRun }

func (f hostRunFakeRun) HostExecutesCommands() bool { return true }

// A sandboxed, gateway-routed node on a run that holds a sealed gateway
// credential is refused a sandbox whose handle carries no proxy endpoint:
// the credential's egress guard is the proxy's pin, and without one the key
// dials the gateway directly, past the network policy. The host passthrough
// is exempt — nothing of the run executes in a container, and the host's own
// dial guard judged the URL at the dispatch seam. A credential-less run is
// today's behaviour, refused by nobody here.
func TestRefuseUnproxiedGatewayCrossing_Table(t *testing.T) {
	runPair := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "sk-run-gw"},
		APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://run-gw.example"},
	})

	t.Run("a run pair on a non-proxied container is refused", func(t *testing.T) {
		task := delegate.Task{NodeID: "gw-node", Model: "openai_compatible/m", Sandbox: nonProxiedFakeRun{}}
		err := refuseUnproxiedGatewayCrossing(runPair, task)
		var refused *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &refused) {
			t.Fatalf("err = %v, want *ErrCapabilityUnsupported — the sealed key would dial direct, past the policy", err)
		}
	})

	t.Run("a proxied container serves the pair", func(t *testing.T) {
		task := delegate.Task{NodeID: "gw-node", Model: "openai_compatible/m", Sandbox: proxiedFakeRun{endpoint: "http://tok@10.2.200.59:41625"}}
		if err := refuseUnproxiedGatewayCrossing(runPair, task); err != nil {
			t.Fatalf("a proxied sandbox is the guard's satisfied condition: %v", err)
		}
	})

	t.Run("the host passthrough is exempt", func(t *testing.T) {
		task := delegate.Task{NodeID: "gw-node", Model: "openai_compatible/m", Sandbox: hostRunFakeRun{}}
		if err := refuseUnproxiedGatewayCrossing(runPair, task); err != nil {
			t.Fatalf("the noop passthrough executes on the host, not in a container: %v", err)
		}
	})

	t.Run("a credential-less run is today's behaviour", func(t *testing.T) {
		task := delegate.Task{NodeID: "gw-node", Model: "openai_compatible/m", Sandbox: nonProxiedFakeRun{}}
		if err := refuseUnproxiedGatewayCrossing(context.Background(), task); err != nil {
			t.Fatalf("an env-gateway node on a run without a sealed pair is unchanged: %v", err)
		}
	})

	t.Run("credentials without a gateway pair are not a holding", func(t *testing.T) {
		vendorOnly := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-an"},
		})
		task := delegate.Task{NodeID: "gw-node", Model: "openai_compatible/m", Sandbox: nonProxiedFakeRun{}}
		if err := refuseUnproxiedGatewayCrossing(vendorOnly, task); err != nil {
			t.Fatalf("a run holding no gateway pair crosses no gateway key: %v", err)
		}
	})

	t.Run("a vendor node is not the guard's business", func(t *testing.T) {
		task := delegate.Task{NodeID: "an-node", Model: "anthropic/claude-x", Sandbox: nonProxiedFakeRun{}}
		if err := refuseUnproxiedGatewayCrossing(runPair, task); err != nil {
			t.Fatalf("a vendor node spends no gateway key: %v", err)
		}
	})
}
