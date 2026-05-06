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
	"runtime"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// activeSandbox bundles a sandbox.Run with the optional network proxy
// that backs it. Both lifecycle handles are owned by the engine and
// must be shut down on Run() exit.
type activeSandbox struct {
	run   sandbox.Run
	proxy *netproxy.Proxy
}

// shutdown tears down both handles best-effort. Safe to call multiple
// times — the underlying drivers/proxy are themselves idempotent.
func (a *activeSandbox) shutdown(ctx context.Context, logger *iterlog.Logger) {
	if a == nil {
		return
	}
	if a.run != nil {
		if err := a.run.Cleanup(ctx); err != nil && logger != nil {
			logger.Warn("runtime: sandbox cleanup: %v", err)
		}
	}
	if a.proxy != nil {
		if err := a.proxy.Shutdown(ctx); err != nil && logger != nil {
			logger.Warn("runtime: sandbox proxy shutdown: %v", err)
		}
	}
}

// resolveAndStartSandbox produces an [activeSandbox] for the workflow's
// active sandbox spec, or (nil, nil) when no sandbox is requested.
//
// Resolution order:
//
//  1. The workflow's declared sandbox.Mode (none/auto/inline) drives
//     spec construction. mode=auto reads .devcontainer/devcontainer.json
//     from repoRoot and converts to a sandbox.Spec.
//  2. The factory selects the best driver for the host (docker > podman
//     on local/desktop; kubernetes > noop on cloud).
//  3. The network proxy is started (when policy is non-open) and its
//     endpoint is threaded into Driver.Start so the container env
//     carries HTTPS_PROXY / HTTP_PROXY pointing at it.
//  4. Driver.Prepare validates and resolves resources (pulls images).
//  5. Driver.Start creates the container and returns the live Run.
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
	logger *iterlog.Logger,
) (*activeSandbox, error) {
	spec, source, err := resolveSandboxSpec(wf, repoRoot, cliOverride, globalDefault)
	if err != nil {
		return nil, err
	}
	if spec == nil || !spec.Mode.IsActive() {
		// User opted out (Mode=none) or never opted in.
		return nil, nil
	}

	// Phase 4 V1: claw nodes are forwarded to the iterion-claw-runner
	// sub-process inside the container so their tool calls (Bash,
	// file edits) execute inside the sandbox. The hard error from
	// earlier phases is replaced by an event so operators can see
	// when the sandboxed claw path is in use, and can opt out by
	// setting backend on the affected nodes.
	if wf != nil && containsClawNode(wf) {
		_ = emitEvent("sandbox_claw_routed_via_runner", map[string]interface{}{
			"reason":         "claw nodes will run via iterion-claw-runner inside the container",
			"limitations_v1": "no MCP servers, no mid-tool-loop ask_user — see docs/sandbox.md",
		})
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
		run, err := driver.Start(ctx, prepared, sandbox.RunInfo{
			RunID:         runID,
			FriendlyName:  friendlyName,
			WorkspacePath: workspacePath,
		})
		if err != nil {
			return nil, fmt.Errorf("runtime: sandbox: noop start: %w", err)
		}
		return &activeSandbox{run: run}, nil
	}

	// Optionally start the network proxy. When the workflow has no
	// explicit network policy, default to the iterion-default
	// allowlist preset so users get sensible defaults out of the box —
	// this is the security-first posture the design plan §5 calls for.
	proxy, proxyEndpoint, err := startNetworkProxy(spec, runID, emitEvent, logger)
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: network proxy: %w", err)
	}

	info := sandbox.RunInfo{
		RunID:         runID,
		FriendlyName:  friendlyName,
		WorkspacePath: workspacePath,
		ProxyEndpoint: proxyEndpoint,
	}

	prepared, err := driver.Prepare(ctx, *spec)
	if err != nil {
		if proxy != nil {
			_ = proxy.Shutdown(ctx)
		}
		return nil, fmt.Errorf("runtime: sandbox: prepare: %w", err)
	}

	run, err := driver.Start(ctx, prepared, info)
	if err != nil {
		if proxy != nil {
			_ = proxy.Shutdown(ctx)
		}
		return nil, fmt.Errorf("runtime: sandbox: start: %w", err)
	}
	return &activeSandbox{run: run, proxy: proxy}, nil
}

// startNetworkProxy compiles the spec's network policy (with sensible
// defaults) and binds an HTTP CONNECT proxy on 127.0.0.1:0. Returns
// (nil, "", nil) when policy mode is "open" — no proxy is needed.
//
// The returned endpoint is the URL the container should set as
// HTTPS_PROXY / HTTP_PROXY. On Linux containers we use the docker
// host-gateway alias so the container can reach the proxy on the
// host's loopback interface; on Docker Desktop (macOS/Windows)
// `host.docker.internal` is the canonical name.
func startNetworkProxy(
	spec *sandbox.Spec,
	runID string,
	emitEvent func(store.EventType, map[string]interface{}) error,
	logger *iterlog.Logger,
) (*netproxy.Proxy, string, error) {
	mode, rules := resolveNetworkPolicy(spec)
	if mode == netproxy.ModeOpen {
		return nil, "", nil
	}

	policy, err := netproxy.Compile(mode, rules)
	if err != nil {
		return nil, "", fmt.Errorf("compile policy: %w", err)
	}

	token, err := netproxy.NewToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate proxy token: %w", err)
	}

	prx, err := netproxy.New(netproxy.Options{
		Policy: policy,
		Token:  token,
		OnBlocked: func(host, reason string) {
			_ = emitEvent("network_blocked", map[string]interface{}{
				"host":   host,
				"reason": reason,
				"run_id": runID,
			})
			if logger != nil {
				logger.Warn("sandbox: network: blocked %s (%s)", host, reason)
			}
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("new proxy: %w", err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		return nil, "", fmt.Errorf("start proxy: %w", err)
	}

	endpoint := prx.Endpoint(proxyHostnameForContainer())
	if logger != nil {
		logger.Info("sandbox: network proxy on %s (mode=%s, %d rules)",
			prx.Addr(), mode, len(rules))
	}
	return prx, endpoint, nil
}

// resolveNetworkPolicy derives the (mode, rules) pair to compile from
// the spec. Precedence:
//
//  1. spec.Network.Mode (when explicit) wins.
//  2. spec.Network.Preset, when set, prefixes the rule list.
//  3. spec.Network.Rules append after the preset.
//
// Default when spec.Network is nil: allowlist + iterion-default preset.
// This makes "user enabled sandbox without thinking about network" land
// on the security-first posture rather than open egress.
func resolveNetworkPolicy(spec *sandbox.Spec) (netproxy.Mode, []string) {
	mode := netproxy.ModeAllowlist
	preset := netproxy.PresetIterionDefault
	var extra []string

	if spec != nil && spec.Network != nil {
		switch spec.Network.Mode {
		case sandbox.NetworkModeAllowlist:
			mode = netproxy.ModeAllowlist
		case sandbox.NetworkModeDenylist:
			mode = netproxy.ModeDenylist
		case sandbox.NetworkModeOpen:
			mode = netproxy.ModeOpen
		}
		if spec.Network.Preset != "" {
			preset = spec.Network.Preset
		}
		extra = spec.Network.Rules
	}

	rules := []string{}
	if preset != "" {
		if pr, ok := netproxy.PresetRules(preset); ok {
			rules = append(rules, pr...)
		}
	}
	rules = append(rules, extra...)
	return mode, rules
}

// proxyHostnameForContainer returns the hostname the *container*
// should use to reach the proxy on the host's loopback interface.
// Linux containers see the host via `host.docker.internal` only when
// the run command included `--add-host=host.docker.internal:host-gateway`
// (docker driver does this). On macOS / Windows Docker Desktop, the
// alias resolves natively.
func proxyHostnameForContainer() string {
	switch runtime.GOOS {
	case "darwin", "windows":
		return "host.docker.internal"
	}
	return "host.docker.internal"
}

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
		"docker":     docker.Constructor,
		"podman":     docker.Constructor,
		"kubernetes": kubernetes.Constructor,
		"noop":       noop.Constructor,
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
