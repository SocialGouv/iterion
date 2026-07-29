// Package runtime — sandbox lifecycle (Phase 1 wiring; nested-worktree-aware).
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
//
// Helpers split across sibling files:
//   - sandbox_mounts.go: bind-mount + host-state wiring
//   - sandbox_lifecycle.go: driver selection, build, start helpers
package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/askusermcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// activeSandbox bundles a sandbox.Run with the optional network proxy
// that backs it. Both lifecycle handles are owned by the engine and
// must be shut down on Run() exit.
type activeSandbox struct {
	run             sandbox.Run
	proxy           *netproxy.Proxy
	workspaceFolder string       // in-container path the host worktree is bind-mounted to (Spec.WorkspaceFolder, e.g. "/workspace"); used by Engine to remap ${PROJECT_DIR}
	boardEndpoint   string       // http URL of the per-run gateway-reachable board MCP listener (C082); empty when not started (no handler / not sandboxed)
	boardListener   *http.Server // the board listener to shut down at teardown; nil when not started
	askUserEndpoint string       // http URL of the per-run gateway-reachable ask-user MCP listener (ADR-082 Phase 3); empty when not started (no interactive node / bind failure)
	askUserToken    string       // per-run bearer token authorizing calls to askUserEndpoint (X-Iterion-Run)
	askUserListener *http.Server // the ask-user listener to shut down at teardown; nil when not started
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
	if a.boardListener != nil {
		if err := a.boardListener.Shutdown(ctx); err != nil && logger != nil {
			logger.Warn("runtime: sandbox board listener shutdown: %v", err)
		}
	}
	if a.askUserListener != nil {
		if err := a.askUserListener.Shutdown(ctx); err != nil && logger != nil {
			logger.Warn("runtime: sandbox ask-user listener shutdown: %v", err)
		}
	}
}

// startSandboxMCPListener binds a per-run, gateway-reachable HTTP
// listener serving handler at path, so a sandboxed claude_code node can
// reach a host-side MCP endpoint from inside the container. It reuses
// the egress proxy's driver-specific bind (docker → 0.0.0.0:0
// advertised as host.docker.internal; kubernetes → the runner pod IP)
// so the container can dial it the same way it dials the proxy.
// Returns the container-reachable endpoint URL and the *http.Server
// (shut down at sandbox teardown).
//
// Two per-run MCP listeners ride this helper:
//   - the board transport (C082): serves the SAME in-process
//     native.Store the studio uses, so writes serialize through that
//     Store's mutex — the reason the board transport is HTTP and not a
//     second in-container process;
//   - the ask-user transport (ADR-082 Phase 3): serves the ask-user
//     tool surface (pkg/askusermcp) so interactive nodes keep the
//     native ask_user tools in-container — the stdio __mcp-ask-user
//     subcommand's host binary path is invisible there.
func startSandboxMCPListener(driver sandbox.Driver, handler http.Handler, path string) (endpoint string, srv *http.Server, err error) {
	bind, advertise, err := proxyAddressesForDriver(driver)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return "", nil, err
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		return "", nil, err
	}
	srv = &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	endpoint = "http://" + net.JoinHostPort(advertise, port) + path
	return endpoint, srv, nil
}

// workflowHasBoardCapability reports whether the workflow (or any of its
// agent/judge nodes) declares a `board.*` capability — the gate for
// starting the per-run board MCP listener.
func workflowHasBoardCapability(wf *ir.Workflow) bool {
	if wf == nil {
		return false
	}
	has := func(caps []string) bool {
		for _, c := range caps {
			if strings.HasPrefix(c, "board.") {
				return true
			}
		}
		return false
	}
	if has(wf.Capabilities) {
		return true
	}
	for _, n := range wf.Nodes {
		switch nn := n.(type) {
		case *ir.AgentNode:
			if has(nn.Capabilities) {
				return true
			}
		case *ir.JudgeNode:
			if has(nn.Capabilities) {
				return true
			}
		}
	}
	return false
}

// SandboxParams bundles the resolution inputs for
// [resolveAndStartSandbox]. Keeping these in a struct avoids long
// positional arg lists at call sites and makes the contract explicit.
//
// Required: Workflow (may carry a sandbox: block), RunID, FriendlyName,
// RepoRoot, WorkspacePath, EmitEvent, Logger. Optional: CLIOverride
// (from --sandbox flag), GlobalDefault (from
// ITERION_SANDBOX_DEFAULT), and DefaultImage (override of the
// fallback image used when sandbox: auto and no devcontainer.json
// is found).
type SandboxParams struct {
	Workflow      *ir.Workflow
	RunID         string
	FriendlyName  string
	RepoRoot      string
	WorkspacePath string
	SecretVars    map[string]any
	CLIOverride   string // "" means no override
	GlobalDefault string // "" means no global default
	DefaultImage  string // "" lets the runtime pick the built-in default

	// HostStateOverride / HostStateDefault carry the precedence inputs
	// for the host_state mount (auto-bind of ~/.iterion + ~/.claude).
	// Empty values defer to the next layer in the chain. The chain is
	// CLI > workflow > env > default("auto"). See pickHostState.
	HostStateOverride string
	HostStateDefault  string

	EmitEvent func(store.EventType, map[string]any) error
	Logger    *iterlog.Logger
	// AttachmentsHostDir, when non-empty, is bind-mounted read-only
	// into the container at AttachmentsContainerPath so {{attachments.X}}
	// path references resolve inside the sandbox. Empty disables the
	// mount (e.g. cloud mode where attachments are pulled by the
	// runner pod via blob.GetAttachment instead).
	AttachmentsHostDir       string
	AttachmentsContainerPath string

	// RunFilesHostDir, when non-empty, is bind-mounted READ-WRITE into
	// the container at RunFilesContainerPath, and surfaced to in-sandbox
	// tool scripts via the ITERION_ARTIFACT_FILES_DIR env var. Tools
	// (write_audit_md, emit_sbom, …) write report/SBOM/manifest files
	// here; iterion lists + serves them via /api/runs/<id>/artifact-
	// files endpoints + the studio's Artifacts panel — without polluting
	// the bench repo's worktree with `docs/renovacy/` commits. Empty
	// disables the mount (cloud mode: cross-machine bind isn't
	// supportable; needs an S3-backed scratch area instead).
	RunFilesHostDir       string
	RunFilesContainerPath string

	// BundleHostDir, when non-empty, is bind-mounted read-only into
	// the container at BundleContainerPath so bundle resources
	// (skills/, prompts/) stay reachable inside the sandbox even when
	// the cache lives outside the workspace bind-mount.
	BundleHostDir       string
	BundleContainerPath string

	// WorktreeGitDir, when non-empty, is the absolute host path of the
	// per-run worktree's git-private directory (e.g.
	// `<repoRoot>/.git/worktrees/<run-id>`). The sandbox bind-mounts it
	// READ-WRITE at the same absolute path inside the container so the
	// worktree's `.git` pointer file (`gitdir: <this-path>`) resolves
	// from in-sandbox git commands. Without this every git command
	// inside the sandbox fails with `fatal: not a git repository`.
	//
	// We deliberately bind only this single per-run directory rather
	// than the whole repo `.git/` so concurrent runs cannot read each
	// other's worktree state. Empty disables the mount (non-worktree
	// runs, cloud runners with no host filesystem).
	WorktreeGitDir string

	// SecretRewriter, when non-nil, enables the egress proxy's
	// TLS-inspection mode (Layer 2): the proxy terminates TLS and uses
	// this rewriter to substitute secret placeholders for real values
	// toward approved hosts and to block real-secret exfiltration. The
	// engine sets it from the executor's guard when the run has known
	// secrets and ITERION_SANDBOX_TLS_INSPECT is not disabled. Enabling
	// inspection also forces a proxy to run even under network: open.
	SecretRewriter netproxy.SecretRewriter

	// BoardMCPHandler, when non-nil, serves the board MCP routes for a
	// per-run gateway-reachable listener started alongside the egress
	// proxy, so sandboxed board-capability nodes can write the operator's
	// board (C082). The engine sets it from WithBoardMCP (server path);
	// nil (CLI / no server) leaves sandboxed board-emit disabled.
	BoardMCPHandler http.Handler
}

// workflowMaxDurationSeconds returns the workflow budget's max_duration
// as whole seconds (0 = unbounded / unset / unparseable). Drivers that
// can self-terminate a leaked sandbox (kubernetes → activeDeadlineSeconds)
// consume it via [sandbox.RunInfo.MaxDurationSeconds]. Mirrors the
// env-expansion the shared budget applies so a `${VAR:-2h}` form resolves
// identically.
func workflowMaxDurationSeconds(wf *ir.Workflow) int64 {
	if wf == nil || wf.Budget == nil || wf.Budget.MaxDuration == "" {
		return 0
	}
	d, err := time.ParseDuration(ir.ExpandEnvWithDefault(wf.Budget.MaxDuration))
	if err != nil || d <= 0 {
		return 0
	}
	return int64(d.Seconds())
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
func resolveAndStartSandbox(ctx context.Context, p SandboxParams) (*activeSandbox, error) {
	logger := p.Logger
	// Wrap the raw emitter so every callsite that discards the error
	// (sandbox lifecycle and build events are not load-bearing for
	// engine correctness) still surfaces store-side failures at warn
	// level — otherwise a degraded store silently drops
	// sandbox_build_failed / sandbox_started and the operator has no
	// signal that the run is unobservable.
	rawEmit := p.EmitEvent
	emitEvent := func(ev store.EventType, payload map[string]any) error {
		err := rawEmit(ev, payload)
		if err != nil && logger != nil {
			logger.Warn("runtime: emit %s event for run %s: %v", ev, p.RunID, err)
		}
		return err
	}
	spec, source, skipReason, err := resolveSandboxSpec(p.Workflow, p.RepoRoot, p.CLIOverride, p.GlobalDefault, resolveDefaultSandboxImage(p.DefaultImage))
	if err != nil {
		return nil, err
	}
	if spec == nil || !spec.Mode.IsActive() {
		// Explicit opt-out (Mode=none / override none), or the built-in
		// default degraded because the host can't sandbox — the latter
		// must stay visible: emit sandbox_skipped so the run record says
		// it executed unsandboxed and why.
		if skipReason != "" {
			_ = emitEvent(store.EventSandboxSkipped, map[string]any{
				"mode":   string(sandbox.ModeAuto),
				"source": source,
				"reason": "sandbox-by-default degraded to unsandboxed: " + skipReason,
			})
			if logger != nil {
				logger.Warn("runtime: sandbox-by-default degraded to unsandboxed for run %s: %s", p.RunID, skipReason)
			}
		}
		return nil, nil
	}

	// Select the driver up front: its capabilities decide which
	// host-convenience mounts are even possible. selectSandboxDriver keys
	// off spec.Mode + host availability only (not the mounts), so it's
	// safe here; the mounts still land before driver.Prepare below, which
	// is what the "configure mounts first" invariant requires.
	driver, err := selectSandboxDriver(spec, logger)
	if err != nil {
		// A sandbox chosen by the built-in default must not brick runs on
		// hosts with no container runtime — degrade to unsandboxed with a
		// visible event. An EXPLICIT request keeps the hard error.
		if source == sandboxDefaultSource {
			_ = emitEvent(store.EventSandboxSkipped, map[string]any{
				"mode":   string(spec.Mode),
				"source": source,
				"reason": "sandbox-by-default degraded to unsandboxed: " + err.Error(),
			})
			if logger != nil {
				logger.Warn("runtime: sandbox-by-default degraded to unsandboxed for run %s: %v", p.RunID, err)
			}
			return nil, nil
		}
		return nil, err
	}
	caps := driver.Capabilities()

	// Configure all mounts BEFORE the driver prepares resources. Each
	// helper is a silent no-op when its host source is missing, so
	// callers don't have to guard.
	addOptionalBindMount(spec, p.AttachmentsHostDir, p.AttachmentsContainerPath, "/run/iterion/attachments", "attachments", true, logger)
	if runFilesContainerPath := addOptionalBindMount(spec, p.RunFilesHostDir, p.RunFilesContainerPath, "/iterion/artifact-files", "run-files", false, logger); runFilesContainerPath != "" {
		// Tool scripts find the path via $ITERION_ARTIFACT_FILES_DIR
		// so recipe authors don't have to hard-code container paths.
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env["ITERION_ARTIFACT_FILES_DIR"] = runFilesContainerPath
	}
	bundleContainerPath := addOptionalBindMount(spec, p.BundleHostDir, p.BundleContainerPath, "/run/iterion/bundle", "bundle", true, logger)
	applyHostStateMounts(spec, p.Workflow, p, emitEvent, logger)
	// Devbox provisioning (bot's bundle devbox.json + target repo's) runs
	// after both: it needs the bundle mount's resolved container path, and
	// it reads spec.WorkspaceFolder, which applyHostStateMounts may have
	// just defaulted. Gated on SupportsPostCreate — a driver with no
	// post-create hook has nothing to install into (noop has no container).
	if caps.SupportsPostCreate {
		applyDevboxProvisioning(spec, p, bundleContainerPath, emitEvent, logger)
	}
	// Host-convenience bind mounts — the claw/rtk runner binaries and the
	// worktree .git — require a driver with a shared host filesystem. On
	// kubernetes (SupportsHostBindMounts=false) type=bind is rejected at
	// translateMounts; there the iterion/rtk binaries are baked into the
	// sandbox image instead (see sandbox/*/Dockerfile).
	if caps.SupportsHostBindMounts {
		addClawBinaryMount(spec, p.Workflow)
		addRewriterMounts(spec)
		addWorktreeGitMount(spec, p.WorktreeGitDir, logger)
	}
	if err := addSecretFileMounts(ctx, spec, p.Workflow, p.SecretVars); err != nil {
		return nil, err
	}

	// Drop any host bind mount the selected driver can't honour. Runs AFTER all
	// mounts are configured so it catches BOTH a bot's `sandbox.mounts:` (e.g. a
	// ~/.claude OAuth mount authored for docker) AND the runtime's own optional
	// host binds (bundle / attachments / run-files). A driver with no host
	// filesystem (kubernetes: SupportsHostBindMounts=false) would otherwise
	// hard-fail manifest building on type=bind; instead we warn and drop, and the
	// sandboxed agent falls back to env creds + the workspace mount (skills are
	// mirrored into <workspace>/.claude). type=secret/pvc/configmap pass through;
	// docker keeps everything (the capability is true there).
	if !caps.SupportsHostBindMounts {
		spec.Mounts = dropHostBindMounts(spec.Mounts, logger)
	}

	// Phase 4 V1: claw nodes are forwarded to the iterion-claw-runner
	// sub-process inside the container so their tool calls (Bash, file
	// edits) execute inside the sandbox. Surface the routing decision
	// so operators can audit it and opt out by setting `backend:` on
	// the affected nodes.
	if p.Workflow != nil && containsClawNode(p.Workflow) {
		_ = emitEvent(store.EventSandboxClawRoutedViaRunner, map[string]any{
			"reason":         "claw nodes will run via iterion-claw-runner inside the container",
			"limitations_v1": "no MCP servers, no mid-tool-loop ask_user — see docs/sandbox.md",
		})
	}

	if driver.Name() == "noop" {
		if len(spec.SecretFiles) > 0 {
			return nil, fmt.Errorf("runtime: sandbox: file secrets require a real sandbox driver; noop cannot mount secret files")
		}
		return startNoopSandbox(ctx, driver, spec, source, p.RunID, p.FriendlyName, p.WorkspacePath, emitEvent)
	}

	// Claude forfait delivery (ADR-082 Phase 3 blocker 3): when the run's
	// credentials carry a materialised Claude Code OAuth file, ship it into
	// the sandbox on the ADR-070 file-secret channel so in-pod claude auth
	// has a real credentials file instead of hanging on the per-spawn env
	// token alone. After the sandbox starts, seedClaudeConfigDir below
	// copies it into the writable CLAUDE_CONFIG_DIR the delegate points the
	// CLI at. Added past the noop gate — a real driver is required to mount.
	claudeOAuthMounted, err := addClaudeOAuthSecretFile(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: claude forfait delivery: %w", err)
	}

	// Optionally start the network proxy. When the workflow has no
	// explicit network policy, default to the iterion-default
	// allowlist preset so users get sensible defaults out of the box —
	// this is the security-first posture the design plan §5 calls for.
	proxy, proxyEndpoint, proxyCACert, err := startNetworkProxy(spec, driver, p.RunID, p.SecretRewriter, emitEvent, logger)
	if err != nil {
		return nil, fmt.Errorf("runtime: sandbox: network proxy: %w", err)
	}

	// Per-run MCP listener intents, decided BEFORE the container starts:
	// the listeners themselves bind after Driver.Start (they need the
	// live driver gateway), but the container's ability to RESOLVE their
	// advertised host (host.docker.internal on docker/podman) is settled
	// at `docker run` time — so the driver must know now that an
	// endpoint will be advertised, proxy or no proxy (the default
	// network: open starts none).
	wantsBoardListener := p.BoardMCPHandler != nil && workflowHasBoardCapability(p.Workflow)
	wantsAskUserListener := workflowHasInteractiveNode(p.Workflow)

	info := sandbox.RunInfo{
		RunID:              p.RunID,
		FriendlyName:       p.FriendlyName,
		WorkspacePath:      p.WorkspacePath,
		ProxyEndpoint:      proxyEndpoint,
		ProxyCACert:        proxyCACert,
		MaxDurationSeconds: workflowMaxDurationSeconds(p.Workflow),
		HostGatewayAlias:   wantsBoardListener || wantsAskUserListener,
	}

	prepared, err := driver.Prepare(ctx, *spec)
	if err != nil {
		if proxy != nil {
			_ = proxy.Shutdown(ctx)
		}
		return nil, fmt.Errorf("runtime: sandbox: prepare: %w", err)
	}

	prepared, err = buildSandboxImageIfRequested(ctx, driver, prepared, spec, info, emitEvent)
	if err != nil {
		if proxy != nil {
			_ = proxy.Shutdown(ctx)
		}
		return nil, err
	}

	run, err := driver.Start(ctx, prepared, info)
	if err != nil {
		if proxy != nil {
			_ = proxy.Shutdown(ctx)
		}
		return nil, fmt.Errorf("runtime: sandbox: start: %w", err)
	}
	emitSandboxStarted(prepared, spec, driver.Name(), source, emitEvent)

	active := &activeSandbox{run: run, proxy: proxy, workspaceFolder: spec.WorkspaceFolder}

	// Second half of the Claude forfait delivery: copy the read-only mount
	// into the writable CLAUDE_CONFIG_DIR the claude_code delegate points
	// sandboxed CLI spawns at. Hard error — the run resolved a forfait, so
	// a failed seed must stop the boot, not resurface as an auth failure
	// hours into the workflow.
	if claudeOAuthMounted {
		if err := seedClaudeConfigDir(ctx, run); err != nil {
			active.shutdown(ctx, logger)
			return nil, err
		}
		if logger != nil {
			logger.Info("runtime: sandbox: claude forfait credentials delivered (CLAUDE_CONFIG_DIR=%s)", secrets.ClaudeCodeSandboxConfigDir)
		}
	}

	// C082: when the server supplies a board MCP handler and a board-cap
	// node exists, start a per-run gateway-reachable board listener so
	// sandboxed claude_code can write the operator's board. Non-fatal: a
	// failure degrades to the prior (board-disabled) behaviour rather than
	// breaking the run.
	if wantsBoardListener {
		endpoint, srv, berr := startSandboxMCPListener(driver, p.BoardMCPHandler, "/api/v1/mcp/board")
		if berr != nil {
			if logger != nil {
				logger.Warn("runtime: sandbox: board MCP listener failed to start (board-emit disabled for this run): %v", berr)
			}
		} else {
			active.boardEndpoint = endpoint
			active.boardListener = srv
			if logger != nil {
				logger.Info("sandbox: board MCP listener on %s (sandboxed board-emit enabled)", endpoint)
			}
		}
	}

	// ADR-082 Phase 3: when the workflow has interactive LLM nodes, bind a
	// per-run ask-user MCP listener so sandboxed claude_code keeps the
	// native ask_user / ask_user_async / await_answers tools (the stdio
	// __mcp-ask-user subcommand is unreachable in-container). The
	// interaction semantics stay host-side (delegate PreToolUse hooks +
	// the interaction store); the listener only serves the MCP tool
	// surface, authenticated by a per-run bearer token. Non-fatal on
	// failure: the delegate then disables the tools per node with a loud
	// warning (the [INTERACTION PROTOCOL] JSON fallback still applies).
	if wantsAskUserListener {
		token, terr := askusermcp.NewRunToken()
		if terr != nil {
			if logger != nil {
				logger.Warn("runtime: sandbox: ask-user MCP token mint failed (native ask_user disabled for this run): %v", terr)
			}
		} else {
			endpoint, srv, aerr := startSandboxMCPListener(driver, askusermcp.Handler(askusermcp.DefaultPath, token), askusermcp.DefaultPath)
			if aerr != nil {
				if logger != nil {
					logger.Warn("runtime: sandbox: ask-user MCP listener failed to start (native ask_user disabled for this run): %v", aerr)
				}
			} else {
				active.askUserEndpoint = endpoint
				active.askUserToken = token
				active.askUserListener = srv
				if logger != nil {
					logger.Info("sandbox: ask-user MCP listener on %s (sandboxed ask_user enabled)", endpoint)
				}
			}
		}
	}
	return active, nil
}

// workflowHasInteractiveNode reports whether any LLM (agent/judge) node
// resolves to a non-none interaction mode — the gate for binding the
// per-run ask-user MCP listener next to a sandbox (ADR-082 Phase 3).
// Human nodes pause in the runtime (no MCP transport involved), so only
// LLM nodes count. The workflow-level `interaction:` default is already
// folded into each node's Interaction field at compile time.
func workflowHasInteractiveNode(wf *ir.Workflow) bool {
	if wf == nil {
		return false
	}
	for _, n := range wf.Nodes {
		if ln, ok := n.(ir.LLMNode); ok && ln.GetInteractionFields().Interaction != ir.InteractionNone {
			return true
		}
	}
	return false
}

// secretEgressRewriter is the structural view of the executor's secret
// guard that drives the proxy's TLS-inspection mode (Layer 2).
// ClawExecutor implements it; defining it here keeps the runtime
// decoupled from pkg/backend/secretguard.
type secretEgressRewriter interface {
	MaterializeForHost(s, host string) string
	ExfiltratesTo(s, host string) bool
	SecretsInspectActive() bool
}

// resolveSecretRewriter returns the egress rewriter for the sandbox
// proxy's TLS-inspection mode, or nil to keep the proxy a transparent
// tunnel. Inspection is ON by default whenever the run carries known
// secrets; ITERION_SANDBOX_TLS_INSPECT=off disables it — the escape
// hatch for a client that genuinely pins or a broken trust-store
// injection (degrades gracefully to Layer 1 placeholders + Layer 0
// redaction + the network allowlist).
func (e *Engine) resolveSecretRewriter() netproxy.SecretRewriter {
	if !sandboxTLSInspectEnabled() {
		return nil
	}
	rw, ok := e.executor.(secretEgressRewriter)
	if !ok || !rw.SecretsInspectActive() {
		return nil
	}
	return rw
}

// sandboxTLSInspectEnabled reports the ITERION_SANDBOX_TLS_INSPECT
// kill-switch state (default on).
func sandboxTLSInspectEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("ITERION_SANDBOX_TLS_INSPECT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// startNetworkProxy compiles the spec's network policy (with sensible
// defaults) and binds an HTTP CONNECT proxy on 127.0.0.1:0. Returns
// (nil, "", nil, nil) when policy mode is "open" and no secret
// inspection is requested — no proxy is needed.
//
// The returned endpoint is the URL the container should set as
// HTTPS_PROXY / HTTP_PROXY. On Linux containers we use the docker
// host-gateway alias so the container can reach the proxy on the
// host's loopback interface; on Docker Desktop (macOS/Windows)
// `host.docker.internal` is the canonical name.
func startNetworkProxy(
	spec *sandbox.Spec,
	driver sandbox.Driver,
	runID string,
	rewriter netproxy.SecretRewriter,
	emitEvent func(store.EventType, map[string]any) error,
	logger *iterlog.Logger,
) (*netproxy.Proxy, string, []byte, error) {
	mode, rules := ResolveNetworkPolicy(spec)

	// TLS inspection needs the driver to inject the per-run CA into the
	// container trust store; drivers advertise that via
	// Capabilities.SupportsTLSInspection. Where it's unsupported (k8s,
	// noop), enabling inspection would mint leaves the container can't
	// trust and break every TLS call — degrade to a transparent proxy
	// (Layer 1 + redaction + allowlist still apply). See docs/secrets.md.
	if rewriter != nil && !driver.Capabilities().SupportsTLSInspection {
		if logger != nil {
			logger.Warn("sandbox: TLS inspection unsupported on %q driver — disabling (Layer 1 + redaction + allowlist still apply)", driver.Name())
		}
		rewriter = nil
	}

	// With no host filtering AND no secret inspection there is nothing
	// for the proxy to do — keep the zero-overhead open path.
	if mode == netproxy.ModeOpen && rewriter == nil {
		return nil, "", nil, nil
	}

	policy, err := netproxy.Compile(mode, rules)
	if err != nil {
		return nil, "", nil, fmt.Errorf("compile policy: %w", err)
	}

	token, err := netproxy.NewToken()
	if err != nil {
		return nil, "", nil, fmt.Errorf("generate proxy token: %w", err)
	}

	bind, advertise, err := proxyAddressesForDriver(driver)
	if err != nil {
		return nil, "", nil, fmt.Errorf("driver proxy config: %w", err)
	}

	opts := netproxy.Options{
		Policy: policy,
		Token:  token,
		OnBlocked: func(host, reason string) {
			_ = emitEvent(store.EventNetworkBlocked, map[string]any{
				"host":   host,
				"reason": reason,
				"run_id": runID,
			})
			if logger != nil {
				logger.Warn("sandbox: network: blocked %s (%s)", host, reason)
			}
		},
	}

	// TLS-inspection mode (Layer 2): mint a per-run CA so the proxy can
	// terminate TLS and rewrite the plaintext request (secret egress
	// substitution + DLP). The CA cert PEM is returned for the driver to
	// inject into the container trust stores.
	var caPEM []byte
	if rewriter != nil {
		ca, err := netproxy.NewEphemeralCA()
		if err != nil {
			return nil, "", nil, fmt.Errorf("egress inspection CA: %w", err)
		}
		opts.InspectCA = ca
		opts.Rewriter = rewriter
		caPEM = ca.CertPEM()
	}

	prx, err := netproxy.New(opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("new proxy: %w", err)
	}
	if err := prx.Start(bind); err != nil {
		return nil, "", nil, fmt.Errorf("start proxy: %w", err)
	}

	endpoint := prx.Endpoint(advertise)
	if logger != nil {
		logger.Info("sandbox: network proxy on %s advertised as %s (mode=%s, %d rules, inspect=%t)",
			prx.Addr(), advertise, mode, len(rules), rewriter != nil)
	}
	return prx, endpoint, caPEM, nil
}

// proxyAddressesForDriver consults the optional [sandbox.ProxyConfigurer]
// interface so each driver can override the proxy bind address and the
// hostname injected into containers. Drivers that don't implement it
// fall back to the docker-friendly defaults.
func proxyAddressesForDriver(d sandbox.Driver) (bind, advertise string, err error) {
	if pc, ok := d.(sandbox.ProxyConfigurer); ok {
		return pc.ProxyConfig()
	}
	return "127.0.0.1:0", "host.docker.internal", nil
}

// ResolveNetworkPolicy derives the (mode, rules) pair to compile from
// the spec. Precedence:
//
//  1. spec.Network.Mode (when explicit) wins.
//  2. spec.Network.Preset, when set, prefixes the rule list.
//  3. spec.Network.Rules append after the preset.
//
// Default when spec.Network is nil: open (no proxy, full egress). Bots
// routinely shell out to package managers, build tooling, and
// integration endpoints that no static allowlist can predict — landing
// on a deny-by-default posture made every fresh workflow author fight
// the proxy before getting useful work done. Operators who want the
// stricter security-first posture opt in via:
//
//	sandbox:
//	  network:
//	    mode: allowlist
//	    preset: iterion-default   # or a custom rule list
//
// The iterion-default preset is still shipped — it's the recommended
// starting point for the allowlist mode — but is no longer applied
// implicitly. ModeAllowlist with an empty rule list is unchanged: it
// blocks everything, surfacing as `network_blocked` events.
func ResolveNetworkPolicy(spec *sandbox.Spec) (netproxy.Mode, []string) {
	mode := netproxy.ModeOpen
	preset := ""
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

// resolveSandboxSpec applies the precedence chain
// (CLI > workflow > global default > built-in auto) and produces a
// [sandbox.Spec] plus a `source` string describing where the spec came
// from (used in the sandbox_skipped event).
//
// CLI override syntax: "" (no override), "none" (force off), "auto"
// (force on, read devcontainer.json). Inline mode requires a DSL
// block and so cannot be expressed via the flag.
//
// The third return (skipReason) is non-empty ONLY when the mode was
// chosen by the built-in default and the host cannot honour it (not a
// git repo, unreadable devcontainer): the run degrades to unsandboxed
// and the caller must surface the reason (sandbox_skipped event). An
// EXPLICIT sandbox request never degrades — it errors.
func resolveSandboxSpec(
	wf *ir.Workflow,
	repoRoot, cliOverride, globalDefault, defaultImage string,
) (*sandbox.Spec, string, string, error) {
	mode, source := pickMode(wf, cliOverride, globalDefault)
	if mode == "" || mode == string(sandbox.ModeNone) {
		return nil, source, "", nil
	}
	byDefault := source == sandboxDefaultSource

	switch mode {
	case string(sandbox.ModeAuto):
		if repoRoot == "" {
			if byDefault {
				// auto is repo-bound by design (it mounts the repo tree);
				// outside a repo the default is simply not applicable —
				// quiet skip (no event), unlike the degrade cases below.
				return nil, source + " — not applicable (outside a git repository)", "", nil
			}
			return nil, source, "", fmt.Errorf("runtime: sandbox: mode=auto requires a git repository (worktree must be active or workdir must be inside a repo)")
		}
		dc, path, err := devcontainer.ReadFromRepo(repoRoot)
		if err != nil {
			if err == devcontainer.ErrNotFound {
				if defaultImage != "" {
					// Carry over Mounts/Env/PostCreate/User/WorkspaceFolder/Build/Network
					// from the block when present — they're equally meaningful when the
					// fallback image runs, and silently dropping them was the cause of
					// the inline-only workaround for vibe bots (sandbox.go pre-fix).
					var spec sandbox.Spec
					if wf != nil && wf.Sandbox != nil {
						spec = fromIRSpec(wf.Sandbox)
					}
					spec.Mode = sandbox.ModeAuto
					spec.Image = defaultImage
					expandSandboxSpec(&spec, repoRoot)
					return &spec, source + " (default image: " + defaultImage + ")", "", nil
				}
				return nil, source, "", fmt.Errorf("runtime: sandbox: mode=auto but no .devcontainer/devcontainer.json found at %s — add one or switch to inline mode", repoRoot)
			}
			if byDefault {
				// Ambient default: a devcontainer the sandbox cannot use
				// (parse error, refused runArgs like --privileged, …) must
				// not disable sandboxing when a default image exists — the
				// repo's devcontainer serves human dev environments first,
				// and rejecting it would leave every run on such a repo
				// permanently unsandboxed (observed live: iterion's own
				// devcontainer declares --privileged; run 019f8a0b degraded
				// to unsandboxed instead of using the default image).
				if defaultImage != "" {
					var spec sandbox.Spec
					if wf != nil && wf.Sandbox != nil {
						spec = fromIRSpec(wf.Sandbox)
					}
					spec.Mode = sandbox.ModeAuto
					spec.Image = defaultImage
					expandSandboxSpec(&spec, repoRoot)
					return &spec, source + fmt.Sprintf(" (devcontainer unusable — %v — default image: %s)", err, defaultImage), "", nil
				}
				return nil, source, fmt.Sprintf("devcontainer.json unreadable: %v", err), nil
			}
			return nil, source, "", fmt.Errorf("runtime: sandbox: read devcontainer.json: %w", err)
		}
		spec := devcontainer.ToSandboxSpec(dc)
		return &spec, source + " (" + path + ")", "", nil

	case string(sandbox.ModeInline):
		// Inline mode requires the workflow's DSL to carry the spec
		// fields. Phase 1 only ships the simple "sandbox: ident" form,
		// so an inline spec only flows through here once the block-form
		// parser lands. The IR field still goes through unchanged so
		// future block-form parsing wires up automatically.
		if wf == nil || wf.Sandbox == nil {
			return nil, source, "", fmt.Errorf("runtime: sandbox: mode=inline but no sandbox: block on the workflow")
		}
		spec := fromIRSpec(wf.Sandbox)
		// Expand devcontainer-style host-side variables in the inline
		// block too, so a recipe author can write
		//   mounts: ["type=bind,source=${localEnv:HOME}/.claude,target=..."]
		// the same way they would in a devcontainer.json. Without
		// expansion docker run rejects the literal `${localEnv:HOME}`
		// string with "mount path must be absolute".
		expandSandboxSpec(&spec, repoRoot)
		return &spec, source, "", nil
	}

	return nil, source, "", fmt.Errorf("runtime: sandbox: unknown mode %q", mode)
}

// ResolveSandboxSpecForDoctor produces the effective sandbox spec a run
// WOULD use, for `iterion sandbox doctor --strict` (and the opt-in
// pre-flight hook), WITHOUT starting anything. It applies the same
// precedence chains the engine uses at run start:
//
//   - mode + image/build/mounts/env/network via [resolveSandboxSpec]
//     (CLI > workflow > global default; mode=auto reads
//     .devcontainer/devcontainer.json or falls back to the default
//     image);
//   - host_state via [pickHostState] (CLI > workflow > env > "auto"),
//     baked into spec.HostState so the doctor's k8s mutual-exclusion
//     check sees the value the engine would.
//
// Unlike [resolveAndStartSandbox], it performs NO filesystem mounts, NO
// image pull, and NO os.Stat of host-state dirs — it is a pure dry-run
// resolution. Returns (nil, source, nil) when no active sandbox is
// requested (mode none / inherit), so callers can report "no sandbox
// configured" rather than guess.
//
// defaultImageFlag mirrors the --sandbox-default-image flag; the env var
// and built-in fallback are applied by [resolveDefaultSandboxImage].
func ResolveSandboxSpecForDoctor(
	wf *ir.Workflow,
	repoRoot, cliOverride, globalDefault, defaultImageFlag, hostStateOverride, hostStateDefault string,
) (*sandbox.Spec, string, error) {
	spec, source, skipReason, err := resolveSandboxSpec(wf, repoRoot, cliOverride, globalDefault, resolveDefaultSandboxImage(defaultImageFlag))
	if err != nil {
		return nil, source, err
	}
	if spec == nil || !spec.Mode.IsActive() {
		if skipReason != "" {
			source += " — would degrade to unsandboxed: " + skipReason
		}
		return spec, source, nil
	}
	resolvedHostState, _ := pickHostState(workflowHostState(wf), hostStateOverride, hostStateDefault)
	spec.HostState = sandbox.HostState(resolvedHostState)
	return spec, source, nil
}

// sandboxDefaultSource labels a mode chosen at the GLOBAL-DEFAULT tier
// (ITERION_SANDBOX_DEFAULT, or the sandbox-by-default policy that
// product entry points install via [ResolveGlobalSandboxDefault]), as
// opposed to an explicit per-run request from the CLI flag or the
// workflow's own block. Downstream resolution keys degrade-vs-fail on
// this: an explicit sandbox request that cannot be honoured must
// hard-error (never silently soften), but the ambient default must not
// brick runs on hosts that cannot sandbox (no container runtime) —
// those degrade to unsandboxed with a visible sandbox_skipped event.
const sandboxDefaultSource = "global sandbox default"

// ResolveGlobalSandboxDefault returns the effective global sandbox
// default a PRODUCT ENTRY POINT (iterion run / resume / studio /
// dispatch) should install via [WithSandboxDefault]: the
// ITERION_SANDBOX_DEFAULT env value when set, else "auto" —
// sandbox-by-default. The policy deliberately lives at the entry
// points, not in the engine: an Engine constructed without
// [WithSandboxDefault] (tests, embedders) stays neutral and runs
// unsandboxed workflows unsandboxed.
func ResolveGlobalSandboxDefault() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_SANDBOX_DEFAULT"))); v != "" {
		return v
	}
	return string(sandbox.ModeAuto)
}

// pickMode walks the precedence chain and returns the first
// non-empty mode along with a human-readable source label.
//
// Special case: a CLI override of "auto" expresses intent ("turn the
// sandbox on, read devcontainer.json") but is less specific than a
// workflow-level block-form `sandbox:` declaration that already
// carries an image/user/etc. In that case the workflow wins, since
// its block is the more specific expression of the same intent —
// and forcing CLI auto would break with "no devcontainer.json found"
// on workflows that don't ship one. CLI "none" still wins everywhere
// (explicit opt-out is non-overridable).
func pickMode(wf *ir.Workflow, cli, global string) (string, string) {
	hasInlineBlock := wf != nil && wf.Sandbox != nil &&
		wf.Sandbox.Mode == string(sandbox.ModeInline) && wf.Sandbox.Image != ""

	if cli == string(sandbox.ModeAuto) && hasInlineBlock {
		return wf.Sandbox.Mode, "workflow sandbox: block (overrides --sandbox=auto)"
	}
	if cli != "" {
		return cli, "cli flag --sandbox"
	}
	if wf != nil && wf.Sandbox != nil && wf.Sandbox.Mode != "" {
		return wf.Sandbox.Mode, "workflow sandbox: block"
	}
	if global != "" {
		return global, sandboxDefaultSource
	}
	return "", "default (no sandbox)"
}

// WorkflowSandboxActive reports whether a run of wf under the given
// CLI-strength override + global default resolves to an ACTIVE sandbox —
// the same pickMode precedence the engine itself applies. Callers that
// deliver run inputs differently for sandboxed vs in-pod execution (e.g.
// the cloud runner's file-secret materialization) must consult THIS,
// never wf.Sandbox directly: under ITERION_SANDBOX_OVERRIDE=none a
// workflow's static sandbox block is present but neutralized, and the
// run executes directly in the pod.
func WorkflowSandboxActive(wf *ir.Workflow, cliOverride, globalDefault string) bool {
	mode, _ := pickMode(wf, cliOverride, globalDefault)
	return sandbox.Mode(mode).IsActive()
}

// workflowHostState returns the workflow-scope host_state declaration
// (wf.Sandbox.HostState), or "" when the workflow declares none. Shared
// by applyHostStateMounts (engine) and ResolveSandboxSpecForDoctor
// (doctor / pre-flight) so both feed pickHostState the identical
// workflow input and can never disagree on the resolved host_state.
func workflowHostState(wf *ir.Workflow) string {
	if wf != nil && wf.Sandbox != nil {
		return wf.Sandbox.HostState
	}
	return ""
}

// pickHostState resolves the precedence chain for the `host_state`
// knob. Same ordering as pickMode but with the built-in default of
// "auto" when nothing further down the chain has spoken — making
// "persistent memory in the sandbox" the out-of-the-box behaviour.
// Returns the resolved value and a human-readable source label used
// in the sandbox_host_state_mounted event.
func pickHostState(wfHostState, cli, global string) (string, string) {
	if cli != "" {
		return cli, "cli flag --sandbox-host-state"
	}
	if wfHostState != "" {
		return wfHostState, "workflow sandbox.host_state"
	}
	if global != "" {
		return global, "ITERION_SANDBOX_HOST_STATE"
	}
	return string(sandbox.HostStateAuto), "default"
}

// hostStateMount describes a single auto-bind from the host into the
// sandbox at the same absolute path. Returned by collectHostStateMounts
// so the caller can decide read/write mode and emit a single event
// listing everything that landed.
type hostStateMount struct {
	HostPath      string
	ContainerPath string // intentionally identical to HostPath for path-key parity
	ReadOnly      bool
}

// collectHostStateMounts returns the auto-bind set for host_state=auto
// given the resolved workspace path. Honors overlap: when the workspace
// already contains a candidate (e.g. project-local <repo>/.iterion is
// nested inside the workspace bind-mount), the candidate is skipped to
// avoid two competing binds. Missing host dirs are skipped silently —
// the user hasn't used the corresponding tool on this host yet, so
// there's nothing persistent to preserve. Each candidate must be an
// absolute path (or empty, which is silently skipped). Variadic so
// callers can pass any subset of the supported state dirs (iterion,
// claude, codex, …) without contortions.
func collectHostStateMounts(workspacePath string, candidates ...string) []hostStateMount {
	out := make([]hostStateMount, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err != nil {
			// Missing host candidate (operator hasn't used the
			// corresponding tool, or the file simply isn't there) →
			// silent skip. Permission errors fall into the same
			// bucket; surfacing them would spam the run console for
			// candidates the workflow never actually needs.
			continue
		}
		if pathContains(workspacePath, candidate) || pathContains(candidate, workspacePath) {
			// Workspace bind-mount already covers (or is covered by)
			// this path — adding another bind would either shadow the
			// workspace or be shadowed itself. Skip to keep the mount
			// graph unambiguous.
			continue
		}
		// Both directories (~/.iterion, ~/.claude, ~/.codex) and
		// single files (~/.gitconfig) are supported: docker's bind
		// machinery treats them uniformly as long as the target
		// path exists on the host. Files in particular are how
		// global git identity reaches in-container `git commit`.
		out = append(out, hostStateMount{
			HostPath:      candidate,
			ContainerPath: candidate,
		})
	}
	return out
}

// pathContains reports whether parent is an ancestor of (or equal to)
// child. Both inputs MUST already be absolute clean paths; the helper
// exists to encode the "skip overlap" rule, not to normalise — callers
// pre-normalise via filepath.Abs once and pass results in.
func pathContains(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// parseUserUID parses the UID prefix of a devcontainer-style remoteUser
// ("1000" or "1000:gid"). Returns (0, false) when the prefix isn't
// fully numeric (a username like "node") — callers can't compare a
// non-numeric user against the host UID without inspecting the image's
// /etc/passwd, so we skip the warning rather than emit a false positive.
func parseUserUID(user string) (int, bool) {
	if user == "" {
		return 0, false
	}
	head := strings.SplitN(user, ":", 2)[0]
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0, false
	}
	return n, true
}

// resolveHostHomeDir returns the host user's home directory, normalised
// to an absolute path. Empty string when the host has no usable HOME
// (CI containers without HOME, distroless, etc.) — callers treat that
// as "host_state cannot fire, skip silently".
func resolveHostHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ""
	}
	abs, err := filepath.Abs(h)
	if err != nil {
		return h
	}
	return abs
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
		HostState:       sandbox.HostState(s.HostState),
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

// expandSandboxSpec applies devcontainer-style host-side variable
// expansion (${localEnv:VAR}, ${localWorkspaceFolder*}) to every
// host-relevant field of an inline sandbox.Spec. Mirrors
// devcontainer.ExpandLocalVarsInFile but operates on the runtime
// shape so inline blocks in .bot files behave identically.
//
// Container-side vars (${containerEnv:...}, ${containerWorkspaceFolder*})
// are intentionally left as-is — they're resolved at runtime by
// lifecycle commands inside the container.
func expandSandboxSpec(s *sandbox.Spec, repoRoot string) {
	if s == nil {
		return
	}
	s.Image = devcontainer.ExpandLocalVars(s.Image, repoRoot)
	s.User = devcontainer.ExpandLocalVars(s.User, repoRoot)
	s.WorkspaceFolder = devcontainer.ExpandLocalVars(s.WorkspaceFolder, repoRoot)
	s.PostCreate = devcontainer.ExpandLocalVars(s.PostCreate, repoRoot)
	for i, m := range s.Mounts {
		s.Mounts[i] = devcontainer.ExpandLocalVars(m, repoRoot)
	}
	for k, v := range s.Env {
		s.Env[k] = devcontainer.ExpandLocalVars(v, repoRoot)
	}
	if s.Build != nil {
		s.Build.Dockerfile = devcontainer.ExpandLocalVars(s.Build.Dockerfile, repoRoot)
		s.Build.Context = devcontainer.ExpandLocalVars(s.Build.Context, repoRoot)
		for k, v := range s.Build.Args {
			s.Build.Args[k] = devcontainer.ExpandLocalVars(v, repoRoot)
		}
	}
}

func cloneStringMap(m map[string]string) map[string]string {
	return cloneMap(m)
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
//
// The IR stores `backend:` verbatim, so an env-templated value like
// `${ITERION_SEC_AUDIT_BACKEND:-claw}` reaches us unexpanded — the
// executor only resolves it at run time (resolveBackendName →
// ExpandEnvWithDefault). Expand here too, or a sandboxed bot whose claw
// nodes are env-templated (sec-audit-source/-deps) is mis-detected as
// claw-free, the iterion binary is never bind-mounted, and the claw
// runner dies with `exec: "iterion": executable file not found in $PATH`.
func backendIsClaw(name string) bool {
	switch strings.ToLower(ir.ExpandEnvWithDefault(name)) {
	case "", "claw":
		return true
	}
	return false
}

// isVolatileBuildPath reports whether p looks like a Go-toolchain
// temporary build artifact (`go run`, `go test`, watchexec-driven
// hot rebuilds). Such paths get unlinked and recreated under load,
// so bind-mounting them into a sandbox container resolves the inode
// at mount time but later exec()'s inside the container hit
// "no such file or directory" once watchexec rotates the build dir.
// Observed under `task studio:dev`: the daemon ran from
// /tmp/go-build*/b001/exe/iterion, the sandbox bound that path at
// /usr/local/bin/iterion inside the container, claw-runner exec'd
// it, and got ENOENT because the host file had been recycled.
//
// Resolver callers skip the sibling-of-Executable check when this
// returns true and fall through to the stable install paths
// (/usr/local/bin/iterion, /usr/bin/iterion, …) instead.
func isVolatileBuildPath(p string) bool {
	return strings.Contains(p, "/go-build") || strings.Contains(p, "/T/go-build")
}

// locateHostIterionBinary finds an `iterion` executable on the host
// suitable for bind-mounting into a sandbox container. Search order:
//
//  1. Sibling of the running executable (covers `dpkg -i` installs
//     where iterion-desktop and iterion live in the same /usr/bin
//     and the operator launches iterion-desktop directly). Skipped
//     when the executable lives under a Go-toolchain temp build dir
//     (see [isVolatileBuildPath]).
//  2. ITERION_BIN env var override (escape hatch for unusual installs).
//  3. /usr/local/bin/iterion → /usr/bin/iterion → ~/.local/bin/iterion
//     (standard Linux install paths).
//
// Returns "" when no binary can be located — the caller falls back to
// expecting the sandbox image to ship its own copy on PATH.
func locateHostIterionBinary() string {
	if exe, err := os.Executable(); err == nil && !isVolatileBuildPath(exe) {
		candidate := filepath.Join(filepath.Dir(exe), "iterion")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	if env := strings.TrimSpace(os.Getenv("ITERION_BIN")); env != "" {
		// #nosec G304 G703 — env is the operator-set ITERION_BIN override, host
		// configuration not external request input.
		if info, statErr := os.Stat(env); statErr == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return env
		}
	}
	candidates := []string{"/usr/local/bin/iterion", "/usr/bin/iterion"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "iterion"))
	}
	for _, p := range candidates {
		if info, statErr := os.Stat(p); statErr == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// engineRepoRoot returns the path the engine should treat as the
// source-of-truth repository root for this run — the operator's main
// checkout, NOT the per-run worktree.
//
// Three layers, first non-empty wins:
//
//  1. [gitlib.FindMainRepoRoot] walks up from workDir to the nearest
//     `.git`. If `.git` is a directory → that's the main repo. If `.git`
//     is a file (linked worktree pointer like
//     `gitdir: <main>/.git/worktrees/<name>`), it follows the pointer
//     back to the main repo. This case matters for dispatcher-spawned
//     bots running at `<repo>/.iterion/dispatcher/workspaces/<id>` —
//     without the pointer-resolution step, project-rooted memory
//     scopes under `${PROJECT_MEMORY_DIR}/` silently key off the
//     worktree's encoded path and a whats-next session at the repo
//     root reads a different (empty) memory tree.
//  2. The absolute path of workDir (legacy behaviour for non-git
//     workspaces).
//  3. `os.Getwd()` when workDir itself is empty.
func engineRepoRoot(workDir string) string {
	if workDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
		return ""
	}
	if main := gitlib.FindMainRepoRoot(workDir); main != "" {
		return main
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

// boardMCPSetter is the optional interface ClawExecutor implements so the
// engine can push the per-run board MCP HTTP endpoint (started with the
// sandbox) into the executor after the run starts. The executor sets
// Task.BoardHTTPEndpoint/BoardRunToken from it for sandboxed board-cap
// nodes (C082). Type-asserted at call time so test stubs need not implement it.
type boardMCPSetter interface {
	SetBoardEndpoint(endpoint string)
}

// askUserMCPSetter is the optional interface ClawExecutor implements so
// the engine can push the per-run ask-user MCP HTTP endpoint + bearer
// token (started with the sandbox) into the executor after the run
// starts (ADR-082 Phase 3). The executor sets
// Task.AskUserHTTPEndpoint/AskUserRunToken from it for sandboxed
// interactive nodes. Type-asserted at call time so test stubs need not
// implement it.
type askUserMCPSetter interface {
	SetAskUserEndpoint(endpoint, token string)
}

// startSandbox boots the run's sandbox container (if the workflow opts
// in), wires it into the executor, stashes the in-container workspace
// path, and returns a no-arg cleanup the caller must defer.
//
// Used by both [Engine.Run] (fresh launches) and the resume paths so
// resumed runs inherit the same filesystem/toolchain isolation as the
// original. Before this helper existed, resumeFromFailure / resumeFromPause
// skipped the bootstrap entirely, leaving e.sandbox == nil — tool nodes
// then ran on the host (host /bin/sh, host paths, host env), which broke
// recipes that depend on the container's toolchain (e.g. `set -o pipefail`
// requires a modern dash, host paths differ from /workspace, etc.).
//
// repoRoot is the absolute path of the git repo backing the run's
// workspace (used by sandbox driver to mount .git on worktree-active
// runs). Pass engineRepoRoot(e.workDir) on resume when no
// worktreeContext is available.
//
// worktreeGitDir, when non-empty, is the absolute host path of the
// per-run worktree's git-private dir (`<repoRoot>/.git/worktrees/<run-id>`).
// Wiring it through lets the sandbox bind-mount it at the same absolute
// path inside the container so the worktree's `.git` pointer file
// resolves from in-container git commands. Empty disables the bind
// (non-worktree runs).
//
// A non-nil error means the sandbox was requested but couldn't start.
// The caller is responsible for failing the run; the returned cleanup
// is a noop in that case but safe to defer.
func (e *Engine) startSandbox(ctx context.Context, runID string, repoRoot string, worktreeGitDir string, inputs map[string]any) (func(), error) {
	noopCleanup := func() {}
	emitForSandbox := func(t store.EventType, data map[string]any) error {
		return e.emit(ctx, runID, t, "", data)
	}
	var attachHost string
	if e.store != nil && e.store.Root() != "" {
		attachHost = filepath.Join(e.store.Root(), "runs", runID, "attachments")
	}
	// Pre-create the per-run artifact-files directory so the bind mount
	// has a source to point at. Both the filesystem store and the Mongo
	// (cloud) store satisfy RunFilesStore — the filesystem store's dir IS
	// the read source, while the Mongo store returns a runner-local
	// scratch dir it later bridges to S3 (see pkg/store/mongo/runfiles.go).
	// A store that doesn't satisfy the interface leaves runFilesHost empty
	// and resolveAndStartSandbox skips the mount silently. Errors
	// from EnsureRunFilesDir are logged but not fatal: the worst case is
	// in-sandbox tools see ITERION_ARTIFACT_FILES_DIR unset and either
	// fall back to a tmpdir or skip writing — far less disruptive than
	// failing the whole sandbox boot over a feature one tool may not use.
	var runFilesHost string
	if rfs := store.AsRunFilesStore(e.store); rfs != nil {
		dir, ensureErr := rfs.EnsureRunFilesDir(ctx, runID)
		if ensureErr != nil {
			e.logger.Warn("runtime: ensure run-files dir failed for run %s: %v", runID, ensureErr)
		} else {
			runFilesHost = dir
		}
	}

	bundleHost := bundleResourceDir(e.bundle, e.filePath)
	var secretVars map[string]any
	if workflowHasFileSecrets(e.workflow) {
		secretVars = e.resolveVars(inputs)
	}
	active, sbErr := resolveAndStartSandbox(ctx, SandboxParams{
		Workflow:                 e.workflow,
		RunID:                    runID,
		FriendlyName:             e.runName,
		RepoRoot:                 repoRoot,
		WorkspacePath:            e.workDir,
		SecretVars:               secretVars,
		CLIOverride:              e.sandboxOverride,
		GlobalDefault:            e.sandboxDefault,
		DefaultImage:             e.sandboxDefaultImage,
		HostStateOverride:        e.sandboxHostStateOverride,
		HostStateDefault:         e.sandboxHostStateDefault,
		EmitEvent:                emitForSandbox,
		Logger:                   e.logger,
		AttachmentsHostDir:       attachHost,
		AttachmentsContainerPath: attachmentsContainerPath,
		RunFilesHostDir:          runFilesHost,
		RunFilesContainerPath:    "/iterion/artifact-files",
		BundleHostDir:            bundleHost,
		BundleContainerPath:      "/run/iterion/bundle",
		WorktreeGitDir:           worktreeGitDir,
		SecretRewriter:           e.resolveSecretRewriter(),
		BoardMCPHandler:          e.boardMCPHandler,
	})
	if sbErr != nil {
		return noopCleanup, sbErr
	}
	// No sandbox → every command of this run executes directly on this
	// host (the cloud runner pod under ITERION_SANDBOX_OVERRIDE=none, or
	// a local run without a sandbox declaration). The sandbox-side devbox
	// provisioning above never fires on that path, so the bot's/repo's
	// devbox.json is honoured here instead — installed on the host and
	// exposed on the run's PATH.
	hostDevboxCleanup := noopCleanup
	if active == nil {
		hostDevboxCleanup = e.provisionHostDevbox(ctx, runID)
	}
	if active != nil && active.run != nil {
		if s, ok := e.executor.(sandboxSetter); ok {
			s.SetSandbox(active.run)
		}
		// Hand the live Run to the host observer (cloud runner) so it can
		// start mid-run file-secret refresh against the driver's
		// SecretFileRefresher. Must not block — the runner spawns its
		// refresh loop on a goroutine keyed to the run ctx.
		if e.sandboxRunObserver != nil {
			e.sandboxRunObserver(active.run)
		}
		// C082: hand the per-run board MCP endpoint to the executor so it
		// can wire Task.BoardHTTPEndpoint/BoardRunToken for sandboxed
		// board-cap nodes. Empty when no listener started (no handler /
		// no board cap / start failed) — executor then leaves it disabled.
		if active.boardEndpoint != "" {
			if bs, ok := e.executor.(boardMCPSetter); ok {
				bs.SetBoardEndpoint(active.boardEndpoint)
			}
		}
		// ADR-082 Phase 3: hand the per-run ask-user MCP endpoint + token
		// to the executor so it can wire Task.AskUserHTTPEndpoint/
		// AskUserRunToken for sandboxed interactive nodes. Empty when no
		// listener started (no interactive node / bind failure) — the
		// delegate then disables the tools loudly.
		if active.askUserEndpoint != "" {
			if as, ok := e.executor.(askUserMCPSetter); ok {
				as.SetAskUserEndpoint(active.askUserEndpoint, active.askUserToken)
			}
		}
		// Stash the in-container bind-mount target so resolveVars can
		// remap ${PROJECT_DIR} to a path processes RUNNING in the
		// sandbox can actually open.
		e.containerWorkspace = active.workspaceFolder
		if e.logger != nil {
			e.logger.Info("runtime: sandbox active (driver=%s, workspace=%s)", active.run.Driver(), active.workspaceFolder)
		}
	}
	cleanup := func() {
		hostDevboxCleanup()
		if active == nil {
			return
		}
		if active.run != nil {
			exportSandboxWorkspaceOnCleanup(active.run, e.logger, emitForSandbox)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		active.shutdown(cleanupCtx, e.logger)
	}
	return cleanup, nil
}

// sandboxWorkspaceExportTimeout bounds the end-of-run pod→host workspace
// export (a tar stream of the whole workspace — sized for large repos,
// distinct from the 30s teardown bound).
const sandboxWorkspaceExportTimeout = 5 * time.Minute

// exportSandboxWorkspaceOnCleanup performs the pod→host workspace
// write-back (ADR-082 Phase 3 blocker 1) at sandbox teardown: copy-based
// drivers (kubernetes) must export the pod workspace back BEFORE the
// sandbox is destroyed — the in-pod commits only exist there, and the
// host-side consumers that run after this cleanup (the deferred
// finalizeOnExit in Engine.Run — LIFO puts this cleanup first — and the
// cloud runner's recordRunGitMeta / diff capture after Run returns) read
// the HOST workspace. Drivers that share the host filesystem
// (docker bind mount, noop) don't implement the interface — no-op.
//
// Runs on its own background ctx: the run ctx is typically
// cancelled/expired by now, and a cancelled export would silently lose
// the run's work. A failure is loud (warn + event), never silent — it
// means un-pushed commits are about to be destroyed with the pod.
func exportSandboxWorkspaceOnCleanup(
	run sandbox.Run,
	logger *iterlog.Logger,
	emitEvent func(store.EventType, map[string]any) error,
) {
	exporter, ok := run.(sandbox.WorkspaceExporter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxWorkspaceExportTimeout)
	defer cancel()
	if err := exporter.ExportWorkspace(ctx); err != nil {
		if logger != nil {
			logger.Warn("runtime: sandbox workspace export back to host FAILED — in-pod commits not visible host-side (a bot-side push, if any, still delivered them): %v", err)
		}
		_ = emitEvent(store.EventSandboxWorkspaceExportFailed, map[string]any{
			"driver": run.Driver(),
			"error":  err.Error(),
		})
		return
	}
	if logger != nil {
		logger.Info("runtime: sandbox workspace exported back to host")
	}
}
