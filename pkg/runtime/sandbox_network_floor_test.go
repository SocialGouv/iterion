package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WITNESSES (ADR-162 decision 7, U5) on the network floor, each driving
// the REAL path: the proxy the start runs (startNetworkProxy over the
// real netproxy), the admission that reads the driver's
// Capabilities.SupportsNetworkPolicy, and the RunInfo that carries the
// untrusted run's port scope to the driver that writes the per-run
// policy.
//
// Mutants: startNetworkProxy's fork override dropped → the first test's
// refused rows answer 200; the admission's SupportsNetworkPolicy term
// dropped → the second test's fork row reaches the canary; the
// pre-bind's trust term dropped → the third test sees one port, not two.

// TestTheForkLanesProxyAllowlistIsTheCredentialsOwnHost: the untrusted
// run's proxy allowlist is the credential's own host and NOTHING the
// workflow declares — not the iterion-default preset (which admits the
// code hosts, the registries and **.googleapis.com: exactly the
// exfiltration roads a fork's diff must not gain), not a `mode: open`
// the fork's own .bot file names. The verdict is read at the CONNECT
// the container's client issues — the moment the policy decides — with
// the run's own token, through the proxy startNetworkProxy composed.
func TestTheForkLanesProxyAllowlistIsTheCredentialsOwnHost(t *testing.T) {
	clearCredentialEnv(t)
	const key = "sk-gw-network-floor-witness"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: key},
		APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://example.com"},
	})
	forkEgressHosts := credentialCrossingEgressHosts(ctx)
	if len(forkEgressHosts) != 1 || forkEgressHosts[0] != "example.com" {
		t.Fatalf("the crossing table's hosts = %v, want exactly the credential's own host example.com", forkEgressHosts)
	}

	// connectThrough starts the real proxy for the given spec and trust
	// and returns the CONNECT verdict line for one host:port.
	connectThrough := func(spec *sandbox.Spec, forkEgress []string, host string) string {
		prx, endpoint, _, err := startNetworkProxy(spec, &specCapturingDriver{hostBinds: true}, "run-net-floor", nil,
			func(store.EventType, map[string]any) error { return nil }, nil, "", nil, forkEgress)
		if err != nil {
			t.Fatalf("startNetworkProxy: %v", err)
		}
		if prx == nil {
			t.Fatal("no proxy started — the run's only route out must exist")
		}
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = prx.Shutdown(sctx)
		})
		line, _ := connect(t, prx.Addr().String(), host+":443", tokenFromEndpoint(t, endpoint))
		return line
	}

	// The hostile spec a fork's diff would write: OPEN mode, and the
	// preset's roads named for good measure.
	hostile := &sandbox.Spec{Network: &sandbox.Network{
		Mode:   sandbox.NetworkModeOpen,
		Preset: netproxy.PresetIterionDefault,
	}}
	// The fork lane: the credential's own host is admitted. 200 = the
	// CONNECT established; 502 = the policy admitted it and the proxy's
	// own dial fell short (this host has no resolver — environmental,
	// AFTER the verdict). Either way the policy's answer was "admitted";
	// a 403 would be the floor refusing the credential's own road.
	if line := connectThrough(hostile, forkEgressHosts, "example.com"); !strings.Contains(line, "200") && !strings.Contains(line, "502") {
		t.Fatalf("the credential's own host through the fork proxy = %q, want admitted (200 established, or 502 past the verdict) — the one route out must serve the credential", line)
	}
	// …and the preset's roads — the code host, the registry, googleapis —
	// are refused by the policy the floor replaced the spec with.
	for _, host := range []string{"api.github.com", "registry.npmjs.org", "storage.googleapis.com"} {
		if line := connectThrough(hostile, forkEgressHosts, host); !strings.Contains(line, "403") {
			t.Fatalf("CONNECT %s through the fork proxy = %q, want 403 — the preset's roads are not this run's allowlist (open mode widens nothing)", host, line)
		}
	}

	// Trusted control: the SAME spec names the preset — under an
	// allowlist it admits github, which is exactly why the fork lane
	// may not inherit it. Same 200-or-502 reading as above.
	preset := &sandbox.Spec{Network: &sandbox.Network{
		Mode:   sandbox.NetworkModeAllowlist,
		Preset: netproxy.PresetIterionDefault,
	}}
	if line := connectThrough(preset, nil, "api.github.com"); !strings.Contains(line, "200") && !strings.Contains(line, "502") {
		t.Fatalf("trusted control: CONNECT api.github.com through the preset = %q, want admitted (200, or 502 past the verdict) — the preset is the trusted lane's", line)
	}
}

// TestABoundSecretsHostStillOpensAForkLanesAllowlist: the fork floor and
// the bound-secret opening (#3119) meet at the proxy the start composes —
// the floor REPLACES everything the workflow declares (the artifact is
// the untrusted thing), but the OPERATOR's bindings still open it: a
// fork's review reads the self-hosted forge its credential is pinned to
// the same way a trusted one does. Both roads admitted, the preset's
// roads still refused.
func TestABoundSecretsHostStillOpensAForkLanesAllowlist(t *testing.T) {
	clearCredentialEnv(t)
	const key = "sk-gw-fork-binding-witness"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: key},
		APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://example.com"},
	})
	forkEgressHosts := credentialCrossingEgressHosts(ctx)
	if len(forkEgressHosts) != 1 || forkEgressHosts[0] != "example.com" {
		t.Fatalf("the crossing table's hosts = %v, want exactly the credential's own host example.com", forkEgressHosts)
	}
	// The operator's binding: a self-hosted forge no preset names, exactly
	// the #3119 shape — a review that must read the forge it reviews.
	secretHosts := []string{"git.selfhosted.example.org"}

	connectBoth := func(spec *sandbox.Spec, secret, forkEgress []string, host string) string {
		prx, endpoint, _, err := startNetworkProxy(spec, &specCapturingDriver{hostBinds: true}, "run-fork-binding", nil,
			func(store.EventType, map[string]any) error { return nil }, nil, "", secret, forkEgress)
		if err != nil {
			t.Fatalf("startNetworkProxy: %v", err)
		}
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = prx.Shutdown(sctx)
		})
		line, _ := connect(t, prx.Addr().String(), host+":443", tokenFromEndpoint(t, endpoint))
		return line
	}

	// The hostile spec a fork's diff would write, again: OPEN mode with
	// the preset's roads named.
	hostile := &sandbox.Spec{Network: &sandbox.Network{
		Mode:   sandbox.NetworkModeOpen,
		Preset: netproxy.PresetIterionDefault,
	}}
	for _, host := range []string{"example.com", "git.selfhosted.example.org"} {
		if line := connectBoth(hostile, secretHosts, forkEgressHosts, host); !strings.Contains(line, "200") && !strings.Contains(line, "502") {
			t.Fatalf("CONNECT %s through the fork proxy = %q, want admitted (200 established, or 502 past the verdict) — the credential's host and the operator's binding are both the run's roads", host, line)
		}
	}
	for _, host := range []string{"api.github.com", "storage.googleapis.com"} {
		if line := connectBoth(hostile, secretHosts, forkEgressHosts, host); !strings.Contains(line, "403") {
			t.Fatalf("CONNECT %s through the fork proxy = %q, want 403 — the preset's roads stay refused beside the binding", host, line)
		}
	}
}

// dockerCapsDriver is the docker driver's advertisement today: it can
// inject the inspection CA (so the credential crossing arms on it) but
// enforces no network policy around the container — the proxy env alone
// is a convention, not a floor.
type dockerCapsDriver struct{ specCapturingDriver }

func (d *dockerCapsDriver) Capabilities() sandbox.Capabilities {
	c := d.specCapturingDriver.Capabilities()
	c.SupportsTLSInspection = true
	c.SupportsNetworkPolicy = false
	return c
}

// TestADriverThatCannotEnforceTheNetworkRefusesTheForkRun: the admission
// reads SupportsNetworkPolicy and refuses an untrusted run TYPED on a
// driver that cannot hold the floor — docker's own advertisement today.
// A trusted run keeps today's start on the same driver, and a driver
// that does enforce (kubernetes' advertisement) admits the fork run.
func TestADriverThatCannotEnforceTheNetworkRefusesTheForkRun(t *testing.T) {
	clearCredentialEnv(t)
	key := "sk-ant-network-floor-witness"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: key},
	})
	wf := credentialWorkflow("review", "claude_code", "none")

	d := &dockerCapsDriver{specCapturingDriver{hostBinds: true}}
	err := crossingStart(t, ctx, wf, store.RunTrustFork, d)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxNetworkRefused {
		t.Fatalf("fork run on a driver without network policy = %v, want the typed network refusal", err)
	}
	if !strings.Contains(rerr.Message, "docker") {
		t.Fatalf("the refusal must name the driver: %q", rerr.Message)
	}
	if errors.Is(err, errCanaryStop) {
		t.Fatal("the launch went on: the container's proxy env would be a convention nothing enforces")
	}
	// Controls: the trusted lane never asks, and a policy-capable driver
	// admits the fork run (the canary refusal is the start proceeding).
	if err := crossingStart(t, ctx, wf, store.RunTrustDefault, d); !errors.Is(err, errCanaryStop) {
		t.Fatalf("trusted run on the same driver = %v, want the canary — the floor is the fork lane's question", err)
	}
	if err := crossingStart(t, ctx, wf, store.RunTrustFork, &tlsCapableDriver{specCapturingDriver{hostBinds: true}}); !errors.Is(err, errCanaryStop) {
		t.Fatalf("fork run on a network-policy driver = %v, want the canary — kubernetes admits the fork lane", err)
	}
}

// probeRefusingDriver is the kindnetd stand-in: a driver that holds the
// fork lane's floors and writes the policy, whose start probe then finds
// a road the policy should have closed.
type probeRefusingDriver struct{ infoCapturingDriver }

func (d *probeRefusingDriver) Capabilities() sandbox.Capabilities {
	c := d.infoCapturingDriver.Capabilities()
	c.SupportsHostBindMounts = true
	c.SupportsTLSInspection = true
	c.SupportsNetworkPolicy = true
	return c
}

func (d *probeRefusingDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return nil, &sandbox.NetworkFloorProbeError{Target: "10.42.0.7:9090"}
}

// TestTheStartProbesRefusalSurfacesTypedThroughTheLaunch: the probe's
// verdict is a launch-time typed refusal — the operator reads WHICH road
// stayed open and on WHICH CNI to run the fork lane, not a wrapped
// start error.
func TestTheStartProbesRefusalSurfacesTypedThroughTheLaunch(t *testing.T) {
	clearCredentialEnv(t)
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-probe-floor-witness"},
	})
	wf := credentialWorkflow("review", "claude_code", "none")
	err := crossingStart(t, ctx, wf, store.RunTrustFork, &probeRefusingDriver{infoCapturingDriver{}})
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxNetworkRefused {
		t.Fatalf("a probe-refused fork run = %v, want the typed network refusal", err)
	}
	if !strings.Contains(rerr.Error(), "10.42.0.7:9090") {
		t.Fatalf("the refusal does not name the road that stayed open: %q", rerr.Error())
	}
	if !strings.Contains(rerr.Hint, "kindnetd") || !strings.Contains(rerr.Hint, "CNI") {
		t.Fatalf("the hint does not tell the operator where to run the fork lane: %q", rerr.Hint)
	}
}

// policyInfoDriver is infoCapturingDriver (which reaches Driver.Start
// and records the RunInfo handed it) on a driver that holds the fork
// lane's floors — where the per-run policy is written.
type policyInfoDriver struct{ infoCapturingDriver }

func (d *policyInfoDriver) Capabilities() sandbox.Capabilities {
	c := d.infoCapturingDriver.Capabilities()
	c.SupportsHostBindMounts = true
	c.SupportsTLSInspection = true
	c.SupportsNetworkPolicy = true
	return c
}

// TestTheForkLaneBindsItsAskUserListenerBeforeTheDriverWritesThePolicy:
// an interactive workflow's ask-user listener binds BEFORE Driver.Start
// — the untrusted run's policy names its port, so the port must exist
// when the driver renders the policy. The RunInfo Start receives
// carries the run's whole port scope: the proxy's port and the ask-user
// listener's, nothing else. A trusted run's RunInfo names no scope: its
// listener binds after Start, today's order.
func TestTheForkLaneBindsItsAskUserListenerBeforeTheDriverWritesThePolicy(t *testing.T) {
	clearCredentialEnv(t)
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-askuser-floor-witness"},
	})
	wf := credentialWorkflow("review", "claude_code", "none")
	// The interactive node: an LLM node with a non-none interaction mode
	// is what binds the ask-user listener (ADR-082 Phase 3).
	wf.Nodes["review"].(*ir.AgentNode).Interaction = ir.InteractionLLM

	for _, row := range []struct {
		name             string
		trust            store.RunTrust
		wantUntrusted    bool
		wantPortScopeLen int
	}{
		{"fork lane", store.RunTrustFork, true, 2},
		{"trusted run (the control)", store.RunTrustDefault, false, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			d := &policyInfoDriver{}
			err := crossingStart(t, ctx, wf, row.trust, d)
			if !errors.Is(err, errCanaryStop) {
				t.Fatalf("resolveAndStartSandbox = %v, want the canary (Start ran, the policy was written)", err)
			}
			info := d.info
			if info == nil {
				t.Fatal("Driver.Start never ran — no RunInfo captured")
			}
			if info.UntrustedRun != row.wantUntrusted {
				t.Fatalf("RunInfo.UntrustedRun = %v, want %v", info.UntrustedRun, row.wantUntrusted)
			}
			ports := info.RunnerEgressPorts
			if len(ports) != row.wantPortScopeLen {
				t.Fatalf("RunInfo.RunnerEgressPorts = %v, want exactly %d entries — the proxy's port and the run's own listener, no other run's", ports, row.wantPortScopeLen)
			}
			seen := map[int]bool{}
			for _, p := range ports {
				if p < 1024 || p > 65535 || seen[p] {
					t.Fatalf("port scope %v: %d is no distinct ephemeral listener port", ports, p)
				}
				seen[p] = true
			}
			if row.wantPortScopeLen > 0 && info.ProxyEndpoint == "" {
				t.Fatal("the proxy's endpoint is empty — the port scope names a proxy that did not start")
			}
		})
	}
}
