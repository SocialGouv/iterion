package runview

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// buildWithBackend runs BuildExecutor — the ONE path every plane builds its
// executor through — over a one-agent `sandbox: auto` workflow and returns
// what the run's logger was told.
func buildWithBackend(t *testing.T, nodeBackend string, overrides model.ModelOverrides, drivers map[string]sandbox.DriverConstructor) string {
	t.Helper()
	t.Setenv("ITERION_MODE", "local") // pin the factory order to docker,podman,noop
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	agent := &ir.AgentNode{}
	agent.ID = "work"
	agent.Backend = nodeBackend
	var buf bytes.Buffer
	spec := ExecutorSpec{
		Ctx:   context.Background(),
		Store: st,
		RunID: "run-image-warning",
		Workflow: &ir.Workflow{
			Name:    "image_warning",
			Nodes:   map[string]ir.Node{"work": agent},
			Sandbox: &ir.SandboxSpec{Mode: "auto"},
		},
		WorkDir:        t.TempDir(),
		ModelOverrides: overrides,
		Logger:         iterlog.New(iterlog.LevelDebug, &buf),
		SandboxDrivers: drivers,
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	return buf.String()
}

func withDriver() map[string]sandbox.DriverConstructor {
	return map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return stubSandboxDriver{name: "docker"}, nil },
	}
}

// TestALaunchSaysWhatTheImagesCannotServe: the backend an operator names at
// LAUNCH — `model_overrides`, the API's own field, which stays open by
// decision (ARB-82: no public-API tightening) — is a route the compiler never
// saw, so C321 cannot have warned about it. The one path every plane builds
// its executor through says it instead, while the run is still free.
//
// Mutations seen red: drop the SandboxImageGaps loop from BuildExecutor → the
// override case goes silent; read the node's IR backend instead of the
// override → the override case goes silent while the authored one stays.
func TestALaunchSaysWhatTheImagesCannotServe(t *testing.T) {
	t.Run("the authored route", func(t *testing.T) {
		out := buildWithBackend(t, backendcaps.Kimi, model.ModelOverrides{}, withDriver())
		for _, want := range []string{"work", backendcaps.Kimi, "PATH", "sandbox: none"} {
			if !strings.Contains(out, want) {
				t.Errorf("the launch said %q, which does not name %q", out, want)
			}
		}
	})
	t.Run("the launch override the compiler never saw", func(t *testing.T) {
		var overrides model.ModelOverrides
		overrides.SetBackend("work", backendcaps.OpenCode)
		out := buildWithBackend(t, backendcaps.ClaudeCode, overrides, withDriver())
		if !strings.Contains(out, backendcaps.OpenCode) {
			t.Errorf("a launch routing a node to opencode was not warned: %q", out)
		}
	})
	t.Run("a served route says nothing", func(t *testing.T) {
		out := buildWithBackend(t, backendcaps.ClaudeCode, model.ModelOverrides{}, withDriver())
		if strings.Contains(out, "PATH") {
			t.Errorf("a claude_code run was warned about an image gap: %q", out)
		}
	})
	// The degraded host is the case #1564 paid for: a `sandbox: auto` run on a
	// host with no container runtime executes on the HOST, where the
	// operator's own CLI may well be installed — warning there would train
	// them to ignore the warning.
	t.Run("a run that will not be sandboxed says nothing", func(t *testing.T) {
		out := buildWithBackend(t, backendcaps.Kimi, model.ModelOverrides{}, driverlessSandboxRegistry())
		if strings.Contains(out, "PATH") {
			t.Errorf("a run degraded to the host was warned about a sandbox image: %q", out)
		}
	})
}
