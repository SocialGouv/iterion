// Package runtime — sandbox driver lifecycle helpers extracted from
// sandbox.go so [resolveAndStartSandbox] reads as a flat sequence of
// "configure spec → boot driver" steps.
package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/sandbox/registry"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sandboxDriverForRun resolves the run's sandbox spec and selects its
// driver exactly as [resolveAndStartSandbox] will at run start, without
// preparing or starting anything. It is the ONE implementation of "will
// this run be sandboxed": predictAttachmentsDir forecasts through it,
// and [RunWillBeSandboxed] answers the launch-time consumers (in-pod
// file-secret delivery, the codex fallback screen) from it — before it
// existed each side of that question had its own reading and they
// disagreed precisely on the degraded case (#1564).
//
// A (nil, nil) answer means the run settles WITHOUT a sandbox: nothing
// active was requested, or — the case that motivated the helper —
// resolveSandboxSpec degraded mode=auto (not a git repo, unreadable
// devcontainer with no default image), mirroring the (nil, nil)
// resolveAndStartSandbox returns on the same inputs. A (nil, err)
// answer covers the selection failure resolveAndStartSandbox turns
// into the mode split: auto degrades to the host, inline refuses.
// What either answer MEANS is the caller's policy, as it is the
// engine's.
func sandboxDriverForRun(
	wf *ir.Workflow,
	repoRoot, cliOverride, globalDefault, defaultImage string,
	drivers map[string]sandbox.DriverConstructor,
) (sandbox.Driver, error) {
	spec, _, _, err := resolveSandboxSpec(wf, repoRoot, cliOverride, globalDefault, defaultImage)
	if err != nil || spec == nil || !spec.Mode.IsActive() {
		return nil, err
	}
	driver, err := selectSandboxDriver(spec, nil, drivers)
	if err != nil {
		return nil, err
	}
	return driver, nil
}

// selectSandboxDriver picks the driver from the given driver set — nil
// means the shipped registry, which is what every production caller
// passes — and wraps it with the engine's logger when it's the docker or
// kubernetes driver, so `docker run`, `kubectl apply`, postCreate
// execution and container start messages land in the run.log alongside
// the rest of the run. Without this swap the factory hands back a
// sandbox.Driver whose default logger discards output, and
// silent-failure modes (postCreate skipped because spec was empty, image
// pull stalled, a defaulted sandbox.user, the pod scheduling policy in
// force) become impossible to debug from logs alone.
//
// drivers is the engine's WithSandboxDrivers seam: the registry used to
// be read here as a process global, which is why no test could drive a
// real run against a sandbox whose setup it controls.
func selectSandboxDriver(spec *sandbox.Spec, logger *iterlog.Logger, drivers map[string]sandbox.DriverConstructor) (sandbox.Driver, error) {
	if drivers == nil {
		drivers = registry.Default()
	}
	factory := sandbox.NewFactory(sandbox.FactoryOptions{
		AvailableDrivers: drivers,
	})
	driver, err := factory.DriverForSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: select driver: %w", err)
	}
	if logger != nil {
		switch d := driver.(type) {
		case *docker.Driver:
			driver = d.WithLogger(logger)
		case *kubernetes.Driver:
			driver = d.WithLogger(logger)
		}
	}
	return driver, nil
}

// schedulingSummary is the driver's scheduling policy for the
// sandbox_started event, "" when the driver has none or it is
// misconfigured (Start reports that error itself).
func schedulingSummary(driver sandbox.Driver) string {
	if r, ok := driver.(sandbox.SchedulingPolicyReporter); ok {
		if s, err := r.SchedulingPolicy(); err == nil {
			return s
		}
	}
	return ""
}

// startNoopSandbox runs the Prepare+Start sequence for the noop driver
// against an ACTIVE spec, which requires a caller that pinned noop
// through FactoryOptions.PreferredDriver: DriverForSpec refuses
// otherwise. selectSandboxDriver never sets it, so NO run reaches this
// today — it is the implementation of that seam, kept for a caller
// that wires it (the option is exported). A `sandbox: auto` run on a
// driverless host does not come through here: resolveAndStartSandbox
// returns no sandbox and the run executes on the host. The skip event
// surfaces in events.jsonl + reports so it's visible the run is NOT
// sandboxed.
func startNoopSandbox(
	ctx context.Context,
	driver sandbox.Driver,
	spec *sandbox.Spec,
	source, runID, friendlyName, workspacePath string,
	emitEvent func(store.EventType, map[string]any) error,
) (*activeSandbox, error) {
	_ = emitEvent(store.EventSandboxSkipped, map[string]any{
		"driver": "noop",
		"mode":   string(spec.Mode),
		"source": source,
		"reason": "operator opted into the noop driver; the run is NOT actually sandboxed",
	})
	prepared, err := driver.Prepare(ctx, *spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: noop prepare: %w", err)
	}
	run, err := driver.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         runID,
		FriendlyName:  friendlyName,
		WorkspacePath: workspacePath,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: noop start: %w", err)
	}
	return &activeSandbox{run: run, workspaceFolder: spec.WorkspaceFolder}, nil
}

// buildSandboxImageIfRequested materialises an image via
// [sandbox.Builder] when spec.Build is non-nil. Returns the prepared
// handle unchanged when Build is nil or the driver doesn't implement
// Builder — non-Builder drivers must reject Spec.Build in their
// Prepare so the engine surfaces a clear error rather than silently
// ignoring (kubernetes is the canonical example).
func buildSandboxImageIfRequested(
	ctx context.Context,
	driver sandbox.Driver,
	prepared sandbox.PreparedSpec,
	spec *sandbox.Spec,
	info sandbox.RunInfo,
	emitEvent func(store.EventType, map[string]any) error,
) (sandbox.PreparedSpec, error) {
	if spec.Build == nil {
		return prepared, nil
	}
	b, ok := driver.(sandbox.Builder)
	if !ok {
		return prepared, nil
	}
	buildStart := time.Now()
	_ = emitEvent(store.EventSandboxBuildStarted, map[string]any{
		"driver":     driver.Name(),
		"dockerfile": spec.Build.Dockerfile,
		"context":    spec.Build.Context,
	})
	built, buildErr := b.Build(ctx, prepared, info)
	if buildErr != nil {
		_ = emitEvent(store.EventSandboxBuildFailed, map[string]any{
			"driver": driver.Name(),
			"error":  buildErr.Error(),
		})
		return nil, fmt.Errorf("runtime: sandbox: build: %w", buildErr)
	}
	builtImage := ""
	if sp, ok := built.(interface{ Spec() sandbox.Spec }); ok {
		builtImage = sp.Spec().Image
	}
	_ = emitEvent(store.EventSandboxBuildFinished, map[string]any{
		"driver":      driver.Name(),
		"target":      builtImage,
		"duration_ms": time.Since(buildStart).Milliseconds(),
	})
	return built, nil
}

// emitSandboxStarted records the resolved image so operators can tell
// from events.jsonl which spec actually backed the sandbox — without
// this we only see "sandbox active (driver=docker)" in the log, which
// doesn't reveal whether `auto` resolved to the project's devcontainer
// or to the slim fallback (the silent-fallback bug that ate the
// modjo postCreate).
//
// The scheduling policy the driver stamped on the sandbox is recorded for
// the same reason: a run resumed on another runner during a rollout is
// re-rendered under THAT runner's policy, and the event is the only place
// the difference is visible.
func emitSandboxStarted(
	prepared sandbox.PreparedSpec,
	spec *sandbox.Spec,
	driverName, source, scheduling string,
	emitEvent func(store.EventType, map[string]any) error,
) {
	resolvedImage := ""
	if sp, ok := prepared.(interface{ Spec() sandbox.Spec }); ok {
		resolvedImage = sp.Spec().Image
	}
	if resolvedImage == "" {
		resolvedImage = spec.Image
	}
	data := map[string]any{
		"driver":          driverName,
		"mode":            string(spec.Mode),
		"source":          source,
		"image":           resolvedImage,
		"has_post_create": spec.PostCreate != "",
	}
	if scheduling != "" {
		data["scheduling"] = scheduling
	}
	_ = emitEvent(store.EventSandboxStarted, data)
}
