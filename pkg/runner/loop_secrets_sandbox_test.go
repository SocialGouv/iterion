package runner

import (
	"context"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// stubSandboxDriver is a constructible sandbox.Driver for the registry
// seams: the sandbox PROBE under test only ever calls Name (and, for the
// attachments forecast, Capabilities) — Prepare/Start are the engine's,
// and no test here reaches them.
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

// workingSandboxRegistry is a driver set mimicking a host WITH a
// container runtime: the docker constructor succeeds.
func workingSandboxRegistry() map[string]sandbox.DriverConstructor {
	return map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return stubSandboxDriver{name: "docker"}, nil },
	}
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

// A `sandbox: auto` run on a host with no container runtime degrades to
// UNSANDBOXED at engine start (sandbox_skipped) — but its mode still
// resolves active, so a gate that reads the mode alone skips the
// materialization and the in-pod agent opens an empty mount path
// (#1564). The gate must be driver-aware: on the degraded host the
// secrets are written here; on a host with a real driver they are not
// (the container mount delivers them).
func TestMaterializeFileSecretsNoSandbox_DegradedAutoStillWrites(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order to docker,podman,noop — CI runs on kubernetes pods (HostCloud prefers kubernetes,noop)
	wf := &ir.Workflow{
		Sandbox: &ir.SandboxSpec{Mode: "auto"},
		Secrets: map[string]*ir.Secret{
			"forge_token": {Name: "forge_token", As: "file"},
		},
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{"forge_token": "tok"},
	})
	repoRoot := t.TempDir() // not a git repo: mode auto resolves via the default image

	// The degraded host: mode resolves active, no driver answers — the
	// gate must OPEN. Whether the write itself lands depends on this
	// machine's /run writability, so the assertion keys on the gate,
	// not the filesystem: the early (nil, nil, nil) return is the bug.
	degraded := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		SandboxDrivers: driverlessSandboxRegistry(),
	}}
	written, cleanup, err := degraded.materializeFileSecretsNoSandbox(ctx, wf, repoRoot)
	if cleanup != nil {
		defer cleanup()
	}
	if written == nil && cleanup == nil && err == nil {
		t.Fatal("a sandbox: auto run degraded to the host was read as sandboxed — " +
			"its `as: file` secrets were never materialized and the agent will open an empty mount path (#1564)")
	}

	// The same workflow on a host with a driver: the gate must stay
	// CLOSED — the container mount is the delivery channel.
	sandboxed := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		SandboxDrivers: workingSandboxRegistry(),
	}}
	written, cleanup, err = sandboxed.materializeFileSecretsNoSandbox(ctx, wf, repoRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != nil || cleanup != nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("a genuinely sandboxed run must not materialize secrets on the host, got %v", written)
	}
}
