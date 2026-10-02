package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResolveAndStartSandbox_InlineRefusesWithTypedCode pins the
// runtime-side half of #1425's split: an EXPLICIT inline sandbox
// (`sandbox: { mode: inline, image: … }`) on a host with no
// container-runtime driver returns a
// `RuntimeError{Code: ErrCodeSandboxDriverUnavailable}` wrapping
// `sandbox.ErrDriverUnavailable`, and emits a `sandbox_skipped` event
// with `refused: true` + `error_code`. The schedule's `last_error`
// (#1426) keys on the typed code; the studio's timeline reads the
// event.
//
// The AUTO half lives in this file too
// (TestResolveAndStartSandbox_AutoDegradesToHostWithVisibleEvent and
// its siblings): both halves are decided here, which is the point —
// `pkg/sandbox/factory_test.go` pins only that the factory keeps NO
// mode policy.
//
// Mutation: return nil,nil on the inline path in resolveAndStartSandbox
// → err == nil → red on the RuntimeError assertion.
func TestResolveAndStartSandbox_InlineRefusesWithTypedCode(t *testing.T) {
	drivers := driverlessHostRegistry()

	t.Run("inline refuses with SANDBOX_DRIVER_UNAVAILABLE", func(t *testing.T) {
		t.Setenv("ITERION_MODE", "local")
		var refusedEvt bool
		emit := func(ev store.EventType, payload map[string]any) error {
			if ev == store.EventSandboxSkipped && payload["refused"] == true {
				refusedEvt = true
				if got, _ := payload["error_code"].(string); got != string(store.FailureSandboxDriverUnavailable) {
					t.Errorf("refused event error_code=%q, want %q", got, string(store.FailureSandboxDriverUnavailable))
				}
			}
			return nil
		}
		wf := &ir.Workflow{
			Name: "inline_hard",
			Sandbox: &ir.SandboxSpec{
				Mode:  string(sandbox.ModeInline),
				Image: "ghcr.io/test/inline:v1",
			},
		}
		p := SandboxParams{
			Workflow: wf, RunID: "run-inline", WorkspacePath: t.TempDir(),
			Drivers: drivers, Logger: iterlog.Nop(), EmitEvent: emit,
		}
		_, err := resolveAndStartSandbox(context.Background(), p)
		if err == nil {
			t.Fatal("resolveAndStartSandbox(inline, no driver) err = nil, want RuntimeError")
		}
		var rte *RuntimeError
		if !errors.As(err, &rte) {
			t.Fatalf("err = %v, want *RuntimeError", err)
		}
		if rte.Code != ErrCodeSandboxDriverUnavailable {
			t.Fatalf("RuntimeError.Code = %q, want %q — schedule last_error keys on this (#1426)", rte.Code, ErrCodeSandboxDriverUnavailable)
		}
		if !errors.Is(err, sandbox.ErrDriverUnavailable) {
			t.Fatal("errors.Is(err, sandbox.ErrDriverUnavailable) must be true — the typed sentinel is the wire contract")
		}
		if !refusedEvt {
			t.Fatal("inline refusal must ALSO emit sandbox_skipped with refused:true so the run stream carries the reason")
		}
	})
}

// TestRetryPolicy_SandboxDriverUnavailableIsDeterministic pins the
// schedule non-retry promise: a redelivery reaches the same
// runtime-less host and burns MaxDeliver pods on one verdict — no
// automatic retry until the driver appears. #1425 / #1426.
//
// Mutation: drop the FailureSandboxDriverUnavailable row in
// classify.go's table → Classify returns DispositionUnknown → the
// runner's classifyExecResult would NAK the delivery, spinning a
// scheduled tick on the same absent driver. Red on this test.
func TestRetryPolicy_SandboxDriverUnavailableIsDeterministic(t *testing.T) {
	got := retrypolicy.Classify(store.FailureSandboxDriverUnavailable)
	if got != retrypolicy.DispositionDeterministic {
		t.Fatalf("Classify(SANDBOX_DRIVER_UNAVAILABLE) = %v, want DispositionDeterministic — a redelivery reaches the same absent driver; the schedule (#1426) must not auto-retry until an operator changes the isolation ask or provisions a driver", got)
	}
}

// TestResolveAndStartSandbox_AutoDegradesToHostWithVisibleEvent is the
// AUTO half of #1425's split, exercised where the run actually decides:
// a host with no container-runtime driver runs the workflow UNSANDBOXED
// (nil activeSandbox — the engine then provisions the host devbox and
// the `iterion` PATH shim, which only the no-sandbox branch does) and
// says so on the run's event stream.
//
// The nil check is the load-bearing one: handing back a noop
// activeSandbox also "runs on the host", but it takes the engine's
// sandboxed branch — no host devbox, no engine-binary shim — and emits
// the noop driver's own skip reason ("operator opted into the noop
// driver"), which nobody did here.
//
// Mutation: drop the emitEvent call on the auto branch → red on the
// event assertion; return the noop driver instead of nil,nil → red on
// the "active != nil" assertion.
func TestResolveAndStartSandbox_AutoDegradesToHostWithVisibleEvent(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	var skipped []map[string]any
	emit := func(ev store.EventType, payload map[string]any) error {
		if ev == store.EventSandboxSkipped {
			skipped = append(skipped, payload)
		}
		return nil
	}
	wf := &ir.Workflow{
		Name:    "auto_soft",
		Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)},
	}
	p := SandboxParams{
		Workflow: wf, RunID: "run-auto", WorkspacePath: t.TempDir(),
		// A repo root with no .devcontainer/devcontainer.json: spec
		// resolution falls back to the default image and reaches the
		// driver selection, which is the step under test.
		RepoRoot:     t.TempDir(),
		DefaultImage: "example.invalid/iterion-sandbox:test",
		Drivers:      driverlessHostRegistry(),
		Logger:       iterlog.Nop(), EmitEvent: emit,
	}
	active, err := resolveAndStartSandbox(context.Background(), p)
	if err != nil {
		t.Fatalf("auto on a driverless host must DEGRADE, not refuse: err = %v (this is the 107 dead scheduled ticks of #1425)", err)
	}
	if active != nil {
		t.Fatal("auto degraded run must report NO active sandbox — a noop activeSandbox takes the engine's sandboxed branch and skips provisionHostDevbox (no repo toolchain, no `iterion` PATH shim)")
	}
	if len(skipped) != 1 {
		t.Fatalf("sandbox_skipped events = %d, want exactly 1 — the degrade must be visible on the run's stream", len(skipped))
	}
	evt := skipped[0]
	if got, _ := evt["mode"].(string); got != string(sandbox.ModeAuto) {
		t.Errorf("event mode = %q, want %q", got, sandbox.ModeAuto)
	}
	if refused, _ := evt["refused"].(bool); refused {
		t.Error("the auto degrade must NOT carry refused:true — the run proceeded")
	}
	reason, _ := evt["reason"].(string)
	if !strings.Contains(reason, "container") && !strings.Contains(reason, "driver") {
		t.Errorf("event reason = %q, want it to name the absent container runtime — an operator reads this to know why the run was not isolated", reason)
	}
}

// TestResolveAndStartSandbox_EveryAutoDegradeSaysFileSecretsAreDropped:
// degrading is honest only if it says what it costs. `as: file` secrets
// reach a workflow through the container mount (addSecretFileMounts)
// or, on a cloud pod the runner already knows is unsandboxed, through
// materializeFileSecretsNoSandbox — an auto-degraded run gets neither,
// and an agent that opens the mount path finds nothing there.
//
// Both degrade sites are asserted, because they are one outcome met at
// two moments: spec resolution (no repo root to mount) and driver
// selection (no runtime to mount into). A field honoured at one site
// and forgotten at the other is how the same run acquires two
// descriptions.
//
// Mutation: drop the workflowHasFileSecrets branch in
// autoDegradePayload → both sub-cases redden. Give one site its own
// inline payload again and mutate that one → its sub-case reddens
// alone.
func TestResolveAndStartSandbox_EveryAutoDegradeSaysFileSecretsAreDropped(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	wf := func() *ir.Workflow {
		return &ir.Workflow{
			Name:    "auto_soft_secrets",
			Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)},
			Secrets: map[string]*ir.Secret{
				"forge_token": {Name: "forge_token", As: "file"},
			},
		}
	}
	for _, c := range []struct {
		name     string
		repoRoot string
	}{
		// The production obstacle: a host with no container runtime.
		{"no container-runtime driver", "use-temp-dir"},
		// The resolver's own obstacle. Reachable from an embedder that
		// passes no repo root; `iterion run` / studio / the runner
		// always resolve one (engineRepoRoot).
		{"no repo root", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var skipped []map[string]any
			emit := func(ev store.EventType, payload map[string]any) error {
				if ev == store.EventSandboxSkipped {
					skipped = append(skipped, payload)
				}
				return nil
			}
			repoRoot := c.repoRoot
			if repoRoot == "use-temp-dir" {
				repoRoot = t.TempDir()
			}
			p := SandboxParams{
				Workflow: wf(), RunID: "run-auto-secrets", WorkspacePath: t.TempDir(),
				RepoRoot:     repoRoot,
				DefaultImage: "example.invalid/iterion-sandbox:test",
				Drivers:      driverlessHostRegistry(),
				Logger:       iterlog.Nop(), EmitEvent: emit,
			}
			active, err := resolveAndStartSandbox(context.Background(), p)
			if err != nil {
				t.Fatalf("auto must still degrade with file secrets declared: %v", err)
			}
			if active != nil {
				t.Fatal("the degraded run must report no active sandbox")
			}
			if len(skipped) != 1 {
				t.Fatalf("sandbox_skipped events = %d, want 1", len(skipped))
			}
			if dropped, _ := skipped[0]["file_secrets_dropped"].(bool); !dropped {
				t.Fatalf("event = %v, want file_secrets_dropped:true — the run is about to look for a secret file nobody wrote", skipped[0])
			}
		})
	}
}

// TestRun_ExplicitSandboxRefusalParksTheTypedCodeOnTheRun exercises the
// GUARANTEE, not the site: a full engine Run whose workflow declares an
// EXPLICIT inline container, on a host with no driver, must park the
// PERSISTED run with FailureCode SANDBOX_DRIVER_UNAVAILABLE — that
// document field is what the schedule's `last_run_error_code` (#1426,
// PR #1510) and the studio's failure row read. Asserting the
// RuntimeError at resolveAndStartSandbox alone would leave the
// classification chain (setupFailureStatus → setupFailureCode →
// recordSetupOutcome) unproven.
//
// Mutation: remove the refusal (return the noop driver for inline too)
// → the run reaches the node loop and completes unsandboxed → red on
// the "Run must not succeed" assertion.
func TestRun_ExplicitSandboxRefusalParksTheTypedCodeOnTheRun(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	st := tmpStore(t)
	const runID = "run-inline-refused"
	if _, err := st.CreateRun(ctx, runID, "inline_hard", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	wf := &ir.Workflow{
		Name:  "inline_hard",
		Entry: "done",
		Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Sandbox: &ir.SandboxSpec{
			Mode:      string(sandbox.ModeInline),
			Image:     "example.invalid/inline:v1",
			HostState: "none",
		},
	}
	eng := New(wf, st, newStubExecutor(),
		WithLogger(iterlog.Nop()),
		WithWorkDir(t.TempDir()),
		WithSandboxDrivers(driverlessHostRegistry()),
	)
	err := eng.Run(ctx, runID, nil)
	if err == nil {
		t.Fatal("Run must not succeed: an author-declared container IS the isolation contract, and running it unsandboxed drops the guarantee silently")
	}
	if !errors.Is(err, sandbox.ErrDriverUnavailable) {
		t.Fatalf("Run err = %v, want it to carry sandbox.ErrDriverUnavailable", err)
	}
	run, loadErr := st.LoadRun(ctx, runID)
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %q, want failed — a redelivery reaches the same absent driver", run.Status)
	}
	if run.FailureCode != store.FailureSandboxDriverUnavailable {
		t.Fatalf("run.FailureCode = %q, want %q — the schedule's last_run_error_code (#1426) and the studio failure row read THIS field, not the error text",
			run.FailureCode, store.FailureSandboxDriverUnavailable)
	}
}

// TestResolveAndStartSandbox_BuildOnlyInlineBlockIsNotRewrittenByAutoFlag:
// `--sandbox auto` must not turn an author-wired container into a soft
// skip. A `sandbox: { build: … }` block names a container as explicitly
// as `image:` does, so the same guarantee applies — otherwise two
// workflows differing only in image-vs-build get opposite isolation
// under one flag, and the build-form one runs on the host with nothing
// but a degrade event to show for it.
//
// Mutation: drop `|| wf.Sandbox.Build != nil` from pickMode's
// hasInlineBlock → --sandbox=auto wins → the run degrades → red on the
// refusal assertion.
func TestResolveAndStartSandbox_BuildOnlyInlineBlockIsNotRewrittenByAutoFlag(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	var skipped int
	emit := func(ev store.EventType, _ map[string]any) error {
		if ev == store.EventSandboxSkipped {
			skipped++
		}
		return nil
	}
	wf := &ir.Workflow{
		Name: "inline_build",
		Sandbox: &ir.SandboxSpec{
			Mode:  string(sandbox.ModeInline),
			Build: &ir.SandboxBuild{Dockerfile: "Dockerfile"},
		},
	}
	p := SandboxParams{
		Workflow: wf, RunID: "run-inline-build", WorkspacePath: t.TempDir(),
		RepoRoot: t.TempDir(),
		// The operator asks for auto on the command line; the block is
		// the more specific expression of the same intent and wins.
		CLIOverride:  string(sandbox.ModeAuto),
		DefaultImage: "example.invalid/iterion-sandbox:test",
		Drivers:      driverlessHostRegistry(),
		Logger:       iterlog.Nop(), EmitEvent: emit,
	}
	_, err := resolveAndStartSandbox(context.Background(), p)
	if err == nil {
		t.Fatal("--sandbox auto over a `build:` block must still refuse: the author wired a container, and degrading to the host drops the isolation contract silently")
	}
	var rte *RuntimeError
	if !errors.As(err, &rte) || rte.Code != ErrCodeSandboxDriverUnavailable {
		t.Fatalf("err = %v, want RuntimeError{SANDBOX_DRIVER_UNAVAILABLE}", err)
	}
}

// driverlessHostRegistry is a driver set mimicking a host with no
// container runtime: docker/podman constructors fail the way the real
// ones do when the binary is absent, and only the always-constructible
// noop remains — exactly what registry.Default() yields on such a host.
func driverlessHostRegistry() map[string]sandbox.DriverConstructor {
	fail := func(name string) sandbox.DriverConstructor {
		return func() (sandbox.Driver, error) {
			return nil, &sandbox.ErrUnavailable{Driver: name, Reason: "not installed"}
		}
	}
	return map[string]sandbox.DriverConstructor{
		"docker": fail("docker"),
		"podman": fail("podman"),
		// The REAL noop driver, not a double: it is what
		// registry.Default() hands the factory on such a host, and the
		// difference is load-bearing — the real one prepares and starts
		// happily, so a fixture double that errors in Prepare would hide
		// the very branch under test.
		"noop": noop.Constructor,
	}
}
