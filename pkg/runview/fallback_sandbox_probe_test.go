package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// stubSandboxDriver is a constructible sandbox.Driver for the registry
// seam: the fallback screen's sandbox PROBE only ever calls Name —
// Prepare/Start are the engine's, and no test here reaches them.
type stubSandboxDriver struct{ name string }

func (d stubSandboxDriver) Name() string { return d.name }
func (d stubSandboxDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsHostBindMounts: true}
}
func (d stubSandboxDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return nil, nil
}
func (d stubSandboxDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return nil, nil
}

// driverlessSandboxRegistry mimics a host with no container runtime:
// docker/podman constructors fail the way the real ones do when the
// binary is absent, and only the always-constructible noop remains —
// exactly what registry.Default() yields on such a host. Mirrors the
// pkg/runtime helper of the same shape (driverlessHostRegistry); each
// package owns its copy because test helpers do not cross packages.
func driverlessSandboxRegistry() map[string]sandbox.DriverConstructor {
	fail := func(name string) sandbox.DriverConstructor {
		return func() (sandbox.Driver, error) {
			return nil, &sandbox.ErrUnavailable{Driver: name, Reason: "not installed"}
		}
	}
	return map[string]sandbox.DriverConstructor{
		"docker": fail("docker"),
		"podman": fail("podman"),
		"noop":   noop.Constructor,
	}
}

// buildWithCodexFallback runs BuildExecutor over a one-agent workflow
// carrying `sandbox: auto`, with the operator's `--fallback codex:…`
// stage, against the given driver set, and returns the run's timeline.
func buildWithCodexFallback(t *testing.T, drivers map[string]sandbox.DriverConstructor) []*store.Event {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	agent := &ir.AgentNode{}
	agent.ID = "work"

	spec := ExecutorSpec{
		Ctx:   context.Background(),
		Store: st,
		RunID: "run-fallback-codex",
		Workflow: &ir.Workflow{
			Name:    "canary",
			Nodes:   map[string]ir.Node{"work": agent},
			Sandbox: &ir.SandboxSpec{Mode: "auto"},
		},
		// Not a git repo: mode=auto resolves its spec through the default
		// image, the same path the engine takes for such a workspace.
		WorkDir: t.TempDir(),
		// The operator's launch-time ask: survive a quota wall by crossing
		// to the codex CLI.
		RunFallback:    []ir.Fallback{{Backend: "codex", Model: "gpt-5.4"}},
		SandboxDrivers: drivers,
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	evts, err := st.LoadEvents(context.Background(), spec.RunID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return evts
}

// A `sandbox: auto` run on a host with no container runtime degrades to
// UNSANDBOXED at engine start — yet its mode still resolves active, and a
// screen that reads the mode alone refuses the operator's codex fallback
// with "cannot run inside the sandbox this run resolves to" on a run that
// has no sandbox at all (#1564). The codex delegate's own guard keys on
// the DRIVER (task.Sandbox.Driver()), so the screen must too: degraded
// host → the stage is taken; a host with a real driver → it is refused.
func TestBuildExecutor_CodexFallbackScreenFollowsTheDriver(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order to docker,podman,noop — CI runs on kubernetes pods (HostCloud prefers kubernetes,noop)
	// The degraded host: no refusal — dispatch would allow the stage too.
	if refusals := refusalEvents(buildWithCodexFallback(t, driverlessSandboxRegistry())); len(refusals) != 0 {
		reason, _ := refusals[0].Data["reason"].(string)
		t.Fatalf("a sandbox: auto run degraded to the host had its codex fallback refused (%q) — "+
			"the run executes on the host, where codex CAN run (#1564)", reason)
	}

	// The same launch on a host with a driver: exactly one refusal, and
	// it names the sandbox as the reason.
	refusals := refusalEvents(buildWithCodexFallback(t, map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return stubSandboxDriver{name: "docker"}, nil },
	}))
	if len(refusals) != 1 {
		t.Fatalf("a genuinely sandboxed run must refuse the codex stage exactly once, got %d", len(refusals))
	}
	reason, _ := refusals[0].Data["reason"].(string)
	if !strings.Contains(reason, "codex") || !strings.Contains(reason, "sandbox") {
		t.Errorf("the refusal must name codex and the sandbox, got %q", reason)
	}
}
