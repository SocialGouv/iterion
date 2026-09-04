// Package runtime — sandbox driver lifecycle helpers extracted from
// sandbox.go so [resolveAndStartSandbox] reads as a flat sequence of
// "configure spec → boot driver" steps.
package runtime

import (
	"context"
	"fmt"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/sandbox/registry"
	"github.com/SocialGouv/iterion/pkg/store"
)

// selectSandboxDriver picks the driver via the global factory and
// wraps it with the engine's logger when it's the docker or kubernetes
// driver — so `docker run`, `kubectl apply`, postCreate execution and
// container start messages land in the run.log alongside the rest of
// the run. Without this swap the factory hands back a sandbox.Driver
// whose default logger discards output, and silent-failure modes
// (postCreate skipped because spec was empty, image pull stalled, a
// defaulted sandbox.user, the pod scheduling policy in force) become
// impossible to debug from logs alone.
func selectSandboxDriver(spec *sandbox.Spec, logger *iterlog.Logger) (sandbox.Driver, error) {
	factory := sandbox.NewFactory(sandbox.FactoryOptions{
		AvailableDrivers: registry.Default(),
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

// startNoopSandbox runs the Prepare+Start sequence for the noop driver.
// We only land here when the operator explicitly opted into noop
// (PreferredDriver="noop") for an active spec — DriverForSpec
// hard-errors otherwise. The skip event still surfaces in
// events.jsonl + reports so it's visible the run is NOT sandboxed.
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
