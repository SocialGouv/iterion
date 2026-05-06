// Package runtime — sandbox lifecycle (Phase 1 wiring).
//
// At run start the engine resolves the workflow's sandbox spec, picks
// a driver via the global factory, prepares the spec (which may pull
// an image), and starts a long-lived container that will host every
// delegate invocation for this run. The Run handle is pushed into the
// executor so tool nodes and (when Phase 1.5 lands) claude_code go
// through it transparently.
//
// The lifecycle is opt-in: workflows without a sandbox: declaration
// (and CLI invocations without --sandbox) skip every step here and
// the engine behaves exactly as before.
package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveAndStartSandbox produces a [sandbox.Run] for the workflow's
// active sandbox spec, or (nil, nil) when no sandbox is requested.
//
// Resolution order:
//
//  1. The workflow's declared sandbox.Mode (none/auto/inline) drives
//     spec construction. mode=auto reads .devcontainer/devcontainer.json
//     from repoRoot and converts to a sandbox.Spec.
//  2. The factory selects the best driver for the host (docker > podman
//     on local/desktop; kubernetes > noop on cloud).
//  3. Driver.Prepare validates and resolves resources (pulls images).
//  4. Driver.Start creates the container and returns the live Run.
//
// When the resolved driver cannot honour the requested mode (typically:
// the user wants a real sandbox but no docker/podman is on PATH), the
// function emits a `sandbox_skipped` event and returns a noop Run so
// callers can keep using the same code paths without nil-checking.
func resolveAndStartSandbox(
	ctx context.Context,
	wf *ir.Workflow,
	runID, friendlyName, repoRoot, workspacePath string,
	cliOverride string, // from --sandbox flag; "" means no override
	globalDefault string, // from ITERION_SANDBOX_DEFAULT
	emitEvent func(eventType store.EventType, data map[string]interface{}) error,
) (sandbox.Run, error) {
	spec, source, err := resolveSandboxSpec(wf, repoRoot, cliOverride, globalDefault)
	if err != nil {
		return nil, err
	}
	if spec == nil || !spec.Mode.IsActive() {
		// User opted out (Mode=none) or never opted in.
		return nil, nil
	}

	if wf != nil && containsClawNode(wf) {
		return nil, fmt.Errorf("runtime: sandbox: workflow contains a node using backend=claw which Phase 4 will split into a sub-binary; until then a sandboxed run cannot host claw nodes. Either drop the sandbox: block, switch the affected nodes to backend=claude_code, or run the workflow without --sandbox")
	}

	factory := sandbox.NewFactory(sandbox.FactoryOptions{
		AvailableDrivers: defaultDriverRegistry(),
	})
	driver, err := factory.Driver()
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: select driver: %w", err)
	}

	if driver.Name() == "noop" {
		// Active mode requested but no real driver available — emit the
		// skip event so operators can see in events.jsonl that the run
		// is NOT actually sandboxed, then continue with the noop Run
		// so callers don't need a special path.
		_ = emitEvent(store.EventSandboxSkipped, map[string]interface{}{
			"driver": "noop",
			"mode":   string(spec.Mode),
			"source": source,
			"reason": "no container runtime available on this host (install Docker or Podman to enable real sandboxing)",
		})
		prepared, err := driver.Prepare(ctx, *spec)
		if err != nil {
			return nil, fmt.Errorf("runtime: sandbox: noop prepare: %w", err)
		}
		return driver.Start(ctx, prepared, sandbox.RunInfo{
			RunID:         runID,
			FriendlyName:  friendlyName,
			WorkspacePath: workspacePath,
		})
	}

	// Install the engine logger on the docker driver so sandbox messages
	// are interleaved with the rest of the run's output. Best-effort:
	// drivers without a WithLogger method get the default discard sink.
	type loggable interface {
		WithLogger(*iterlogPlaceholder) sandbox.Driver
	}
	_ = loggable(nil) // documentation reference, not enforced at compile time

	prepared, err := driver.Prepare(ctx, *spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: prepare: %w", err)
	}

	run, err := driver.Start(ctx, prepared, sandbox.RunInfo{
		RunID:         runID,
		FriendlyName:  friendlyName,
		WorkspacePath: workspacePath,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: start: %w", err)
	}
	return run, nil
}

// iterlogPlaceholder is referenced only in the doc-comment loggable
// interface above to keep an import-free convention. It never appears
// in real call paths.
type iterlogPlaceholder = struct{}

// resolveSandboxSpec applies the precedence chain
// (CLI > workflow > global default) and produces a [sandbox.Spec] plus
// a `source` string describing where the spec came from (used in the
// sandbox_skipped event).
//
// CLI override syntax: "" (no override), "none" (force off), "auto"
// (force on, read devcontainer.json). Inline mode requires a DSL
// block and so cannot be expressed via the flag.
func resolveSandboxSpec(
	wf *ir.Workflow,
	repoRoot, cliOverride, globalDefault string,
) (*sandbox.Spec, string, error) {
	mode, source := pickMode(wf, cliOverride, globalDefault)
	if mode == "" || mode == string(sandbox.ModeNone) {
		return nil, source, nil
	}

	switch mode {
	case string(sandbox.ModeAuto):
		if repoRoot == "" {
			return nil, source, fmt.Errorf("runtime: sandbox: mode=auto requires a git repository (worktree must be active or workdir must be inside a repo)")
		}
		dc, path, err := devcontainer.ReadFromRepo(repoRoot)
		if err != nil {
			if err == devcontainer.ErrNotFound {
				return nil, source, fmt.Errorf("runtime: sandbox: mode=auto but no .devcontainer/devcontainer.json found at %s — add one or switch to inline mode", repoRoot)
			}
			return nil, source, fmt.Errorf("runtime: sandbox: read devcontainer.json: %w", err)
		}
		spec := devcontainer.ToSandboxSpec(dc)
		return &spec, source + " (" + path + ")", nil

	case string(sandbox.ModeInline):
		// Inline mode requires the workflow's DSL to carry the spec
		// fields. Phase 1 only ships the simple "sandbox: ident" form,
		// so an inline spec only flows through here once the block-form
		// parser lands. The IR field still goes through unchanged so
		// future block-form parsing wires up automatically.
		if wf == nil || wf.Sandbox == nil {
			return nil, source, fmt.Errorf("runtime: sandbox: mode=inline but no sandbox: block on the workflow")
		}
		spec := fromIRSpec(wf.Sandbox)
		return &spec, source, nil
	}

	return nil, source, fmt.Errorf("runtime: sandbox: unknown mode %q", mode)
}

// pickMode walks the precedence chain and returns the first
// non-empty mode along with a human-readable source label.
func pickMode(wf *ir.Workflow, cli, global string) (string, string) {
	if cli != "" {
		return cli, "cli flag --sandbox"
	}
	if wf != nil && wf.Sandbox != nil && wf.Sandbox.Mode != "" {
		return wf.Sandbox.Mode, "workflow sandbox: block"
	}
	if global != "" {
		return global, "ITERION_SANDBOX_DEFAULT"
	}
	return "", "default (no sandbox)"
}

// fromIRSpec converts the IR-level SandboxSpec to the runtime-level
// sandbox.Spec used by drivers. Phase 1 mirrors only the fields the
// IR carries today; later phases extend both shapes in lockstep.
func fromIRSpec(s *ir.SandboxSpec) sandbox.Spec {
	out := sandbox.Spec{
		Mode:            sandbox.Mode(s.Mode),
		Image:           s.Image,
		Mounts:          append([]string(nil), s.Mounts...),
		Env:             cloneStringMap(s.Env),
		User:            s.User,
		PostCreate:      s.PostCreate,
		WorkspaceFolder: s.WorkspaceFolder,
	}
	if s.Build != nil {
		out.Build = &sandbox.Build{
			Dockerfile: s.Build.Dockerfile,
			Context:    s.Build.Context,
			Args:       cloneStringMap(s.Build.Args),
		}
	}
	if s.Network != nil {
		out.Network = &sandbox.Network{
			Mode:    sandbox.NetworkMode(s.Network.Mode),
			Preset:  s.Network.Preset,
			Rules:   append([]string(nil), s.Network.Rules...),
			Inherit: sandbox.InheritMode(s.Network.Inherit),
		}
	}
	return out
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// containsClawNode reports whether any agent/judge node in the workflow
// uses the in-process claw backend. Sandboxing claw requires the
// Phase 4 sub-binary split which is not yet shipped; until then we
// fail fast with a clear message rather than silently running claw on
// the host while pretending the workflow is sandboxed.
func containsClawNode(wf *ir.Workflow) bool {
	for _, n := range wf.Nodes {
		switch nn := n.(type) {
		case *ir.AgentNode:
			if backendIsClaw(nn.Backend) {
				return true
			}
		case *ir.JudgeNode:
			if backendIsClaw(nn.Backend) {
				return true
			}
		case *ir.RouterNode:
			if backendIsClaw(nn.Backend) {
				return true
			}
		}
	}
	return false
}

// backendIsClaw mirrors the rule documented in CLAUDE.md: the claw
// backend is the *implicit* default when neither model nor backend is
// set on a claude/codex-eligible node, so we treat both the explicit
// "claw" name and the empty string as claw.
func backendIsClaw(name string) bool {
	switch strings.ToLower(name) {
	case "", "claw":
		return true
	}
	return false
}

// defaultDriverRegistry mirrors pkg/cli/sandbox.go's helper of the
// same name. We duplicate it here rather than importing pkg/cli to
// avoid a cli → runtime → cli cycle. Drift risk is low since both
// must list every shippable driver — if a CI test catches a drift
// (one knows about a driver the other doesn't), a sandbox driver was
// added without registering it everywhere.
func defaultDriverRegistry() map[string]sandbox.DriverConstructor {
	return map[string]sandbox.DriverConstructor{
		"docker": docker.Constructor,
		"podman": docker.Constructor,
		"noop":   noop.Constructor,
	}
}

// engineRepoRoot returns the path the sandbox should treat as the repo
// root for devcontainer.json lookup. When the engine is running on a
// per-run worktree, we still want to read the source repo's
// .devcontainer/ — that's the user-authored config, and the worktree
// is just a checkout of the same tree. The worktree path itself works
// because git worktree copies the .devcontainer/ files in.
func engineRepoRoot(workDir string) string {
	if workDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
		return ""
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return workDir
	}
	return abs
}

// sandboxSetter is the optional interface ClawExecutor implements so
// the engine can push the live [sandbox.Run] into the executor after
// the run starts. Type-asserted at call time so test stubs don't have
// to implement it.
type sandboxSetter interface {
	SetSandbox(run sandbox.Run)
}
