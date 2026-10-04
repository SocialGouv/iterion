package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// WorkflowSandboxActive must reflect the RESOLVED sandbox decision, not the
// static wf.Sandbox declaration: ITERION_SANDBOX_OVERRIDE=none neutralizes a
// bot's inline sandbox block (the run executes in the runner pod), and
// callers like the cloud runner's file-secret materialization key off it.
func TestWorkflowSandboxActive(t *testing.T) {
	inline := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "x:y"}}
	cases := []struct {
		name     string
		wf       *ir.Workflow
		override string
		def      string
		want     bool
	}{
		{"inline block, no override", inline, "", "", true},
		{"inline block neutralized by override none", inline, "none", "", false},
		{"inline block wins over override auto", inline, "auto", "", true},
		// The engine itself is neutral: policy (sandbox-by-default) lives
		// at product entry points via ResolveGlobalSandboxDefault.
		{"no block, no override, no default", &ir.Workflow{}, "", "", false},
		{"no block, global default auto", &ir.Workflow{}, "", "auto", true},
		{"no block, global default none", &ir.Workflow{}, "", "none", false},
		{"nil workflow, override none", nil, "none", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WorkflowSandboxActive(c.wf, c.override, c.def); got != c.want {
				t.Errorf("WorkflowSandboxActive(override=%q, default=%q) = %v, want %v", c.override, c.def, got, c.want)
			}
		})
	}
}

// RunWillBeSandboxed must answer the DRIVER-aware question — "will this
// run execute inside a container" — where WorkflowSandboxActive answers
// the mode-only one. The case that splits them is the degraded host: the
// mode resolves active, no driver is selectable, and the engine degrades
// the run to unsandboxed at start (#1564). Every genuinely sandboxed
// shape, and an explicit `none`, must read exactly as before.
func TestRunWillBeSandboxed(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order to docker,podman,noop — CI runs on kubernetes pods, where HostCloud prefers kubernetes,noop and the docker stub never matches
	inline := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "x:y"}}
	auto := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "auto"}}
	working := func(t *testing.T) map[string]sandbox.DriverConstructor {
		return map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return &podDriver{root: t.TempDir()}, nil },
		}
	}
	cases := []struct {
		name     string
		wf       *ir.Workflow
		override string
		def      string
		repoRoot string // "tempdir" resolves to a fresh non-repo dir
		drivers  func(t *testing.T) map[string]sandbox.DriverConstructor
		want     bool
	}{
		{"inline block, driver available", inline, "", "", "tempdir", working, true},
		// The degraded divergence: WorkflowSandboxActive answers true for
		// both of these; the run executes on the host in both.
		{"inline block, driverless host", inline, "", "", "tempdir", func(*testing.T) map[string]sandbox.DriverConstructor { return driverlessHostRegistry() }, false},
		{"auto, driverless host", auto, "", "", "tempdir", func(*testing.T) map[string]sandbox.DriverConstructor { return driverlessHostRegistry() }, false},
		{"auto, driver available", auto, "", "", "tempdir", working, true},
		// mode=auto outside a repository degrades exactly as the engine's
		// own resolution does — even with a driver on the host.
		{"auto, no repo root", auto, "", "", "", working, false},
		// Unchanged behaviour: the explicit opt-out and the neutral engine.
		{"inline block neutralized by override none", inline, "none", "", "tempdir", working, false},
		{"no block, global default auto, driver available", &ir.Workflow{}, "", "auto", "tempdir", working, true},
		{"no block, no override, no default", &ir.Workflow{}, "", "", "tempdir", working, false},
		{"nil workflow", nil, "", "", "tempdir", working, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot := c.repoRoot
			if repoRoot == "tempdir" {
				repoRoot = t.TempDir()
			}
			if got := RunWillBeSandboxed(c.wf, c.override, c.def, repoRoot, c.drivers(t)); got != c.want {
				t.Errorf("RunWillBeSandboxed(override=%q, default=%q, repoRoot=%q) = %v, want %v",
					c.override, c.def, repoRoot, got, c.want)
			}
		})
	}
}
