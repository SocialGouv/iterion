package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/reviewtopology"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// RunOptions holds the configuration for the run command.
type RunOptions struct {
	File          string               // .bot file path or .botz bundle path
	Recipe        string               // recipe JSON file path (alternative to File)
	Vars          map[string]string    // --var key=value overrides
	Preset        string               // --preset <name>: applies an in-source named preset before --var
	RunID         string               // explicit run ID (auto-generated if empty)
	Source        *store.RunSource     // originating-action provenance stamped on the run (schedule launches)
	StoreDir      string               // store directory (default: nearest .iterion ancestor of the workflow, or alongside it)
	Timeout       time.Duration        // maximum run duration (0 = no limit)
	LogLevel      string               // log level (default: "info", env: ITERION_LOG_LEVEL)
	NoInteractive bool                 // disable interactive TTY prompting on human pause
	Executor      runtime.NodeExecutor // pluggable executor (nil = stub)
	// Background marks this invocation as a managed-runner subprocess
	// spawned by the studio server. The CLI writes a .pid file so the
	// server can detect liveness across its own restart, and forces
	// NoInteractive (no TTY in the spawned process).
	Background bool
	// MergeInto controls the worktree-finalization fast-forward target
	// for `worktree: auto` runs. "" or "current" → FF the user's
	// currently-checked-out branch (default); "none" → skip FF;
	// <branch-name> → FF that branch (must match currently-checked-out).
	MergeInto string
	// BranchName overrides the default storage branch
	// `iterion/run/<friendly>` created on the worktree's HEAD. The
	// branch is always created (GC guard); on collision a numeric
	// suffix is appended.
	BranchName string
	// MergeStrategy selects how the run's commits are landed on the
	// merge target when AutoMerge is on: "squash" (default) collapses
	// into one commit; "merge" fast-forwards (preserves history).
	MergeStrategy string
	// AutoMerge toggles whether the engine performs the merge at end
	// of run. CLI default is true (preserves prior behaviour); the
	// studio sets false by default to defer merge to a UI action.
	AutoMerge bool
	// Sandbox is the run-level override for the sandbox activation
	// mode ("", "none", "auto"). "" inherits the project default
	// (ITERION_SANDBOX_DEFAULT) which itself defaults to "" (no
	// sandbox). The workflow's own `sandbox:` block is the next layer
	// of precedence; per-node overrides win above all. See pkg/sandbox.
	Sandbox string
	// SandboxDefaultImage overrides the image ref used by `sandbox: auto`
	// when no .devcontainer/devcontainer.json is found in the workspace.
	// Empty inherits ITERION_SANDBOX_DEFAULT_IMAGE then the built-in
	// default (`ghcr.io/socialgouv/iterion-sandbox-slim:<iterion-version>`).
	SandboxDefaultImage string
	// SandboxHostState controls whether the host's `~/.iterion` (run
	// store) and `~/.claude` (Claude Code OAuth + sessions) are
	// auto-mounted into the sandbox so persistent memory survives
	// across runs. Values: "", "auto", "none". Empty inherits
	// ITERION_SANDBOX_HOST_STATE then the built-in default "auto".
	SandboxHostState string
	// Compress is the run-level command-output-compression override
	// ("", "on", "ultra", "off"). "" inherits the workflow/node `compress:`
	// DSL then the ITERION_COMPRESS env default. It is the highest-priority
	// input to rewrite.Resolve. See docs/plugins.md.
	Compress string
	// Permission is the run-level tool-permission-gate mode override
	// ("", "off", "ask", "deny"). "" inherits the workflow/node
	// `permission:` DSL then the ITERION_PERMISSION env default.
	// PermissionAllow/Ask/Deny are run-level rules, additive to the
	// workflow-level lists. See docs/permissions.md.
	Permission      string
	PermissionAllow []string
	PermissionAsk   []string
	PermissionDeny  []string
	// Budget carries CLI overrides for the workflow's budget: block
	// (--max-cost-usd / --max-tokens / --max-duration / --max-iterations /
	// --max-parallel-branches). Non-zero fields win over the DSL/recipe
	// budget; zero fields inherit. See applyBudgetOverrides.
	Budget BudgetOverrides
	// ReviewMode is the run-level override for bi-model review-loop bots'
	// mono/dual topology ("", "auto", "mono", "dual"). Only bots that
	// declare a review_mode var are affected. "" / "auto" resolves from
	// detected provider credentials at launch (pkg/reviewtopology);
	// "mono"/"dual" force the topology. See docs on review topology.
	ReviewMode string
	// ModelFor / BackendFor are launch-time per-node/-group model+backend
	// overrides (repeatable --model / --backend flags), each a
	// "selector=value" (or a bare "value" targeting every LLM node). They win
	// over the node's DSL model:/backend: so a run can re-target the bot per
	// node-group without editing the .bot. Parsed via model.ParseModelOverrides
	// in buildRunExecutor. See model_override.go.
	ModelFor   []string
	BackendFor []string
	// AutoResume is the bounded run-level auto-resume budget N
	// (`--auto-resume`, env ITERION_AUTO_RESUME; default 0 = off). When the
	// run exits failed_resumable with a retryable cause, the CLI waits
	// (capped exponential backoff) and re-invokes resume in-process up to N
	// times, re-using this run's exact overrides. See auto_resume.go.
	AutoResume int
	// SkipMCPHealth downgrades a failing MCP startup health-check from a
	// fatal error to a warning: the check still runs (its diagnostics are
	// logged) but a failure no longer aborts the run. Set by --skip-mcp-health
	// or a truthy ITERION_SKIP_MCP_HEALTH env var. Useful when the project
	// .mcp.json declares a server that is unreachable/unauthorized in this
	// environment (e.g. an HTTP-OAuth MCP) but the run does not depend on it.
	SkipMCPHealth bool
}

// skipMCPHealthFromEnv reports whether ITERION_SKIP_MCP_HEALTH is truthy, so
// the toggle also applies without the CLI flag — e.g. to launcher scripts
// that export it, and to any entry point that reuses RunRun.
func skipMCPHealthFromEnv() bool {
	v := os.Getenv("ITERION_SKIP_MCP_HEALTH")
	return v == "1" || strings.EqualFold(v, "true")
}

// RunRun executes a workflow or recipe and reports the outcome.
func RunRun(ctx context.Context, opts RunOptions, p *Printer) error {
	level, err := iterlog.ResolveLevel(opts.LogLevel, "ITERION_LOG_LEVEL")
	if err != nil {
		return err
	}
	logger := iterlog.New(level, os.Stderr)
	if opts.Background {
		// Managed-runner mode: no TTY available in the spawned
		// subprocess, and prompts would deadlock.
		opts.NoInteractive = true
	}

	if opts.BranchName != "" {
		if err := git.ValidateBranchName(opts.BranchName); err != nil {
			return UserInputError(fmt.Errorf("--branch-name: %w", err))
		}
	}

	runID := opts.RunID
	if runID == "" {
		var idErr error
		runID, idErr = store.GenerateRunID()
		if idErr != nil {
			return fmt.Errorf("mint run id: %w", idErr)
		}
	}

	telemetry, err := startRunTelemetry(runID, logger)
	if err != nil {
		return err
	}
	defer telemetry.shutdown()

	engineOpts := []runtime.EngineOption{
		runtime.WithLogger(logger),
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
	}
	if telemetry.prometheus != nil {
		engineOpts = append(engineOpts, runtime.WithEventObserver(telemetry.prometheus.EventObserver()))
	}
	if telemetry.otlp != nil {
		engineOpts = append(engineOpts, runtime.WithEventObserver(telemetry.otlp.EventObserver()))
	}

	// Resolve the workflow source: either via recipe (which may
	// override prompts/tools/budget) or directly from a .bot file.
	// Recipe overrides MUST be applied before BuildExecutor — the
	// executor snapshots Prompts/Schemas/ToolPolicy/Budget/Compaction
	// at construction time, so feeding it the raw workflow would make
	// the recipe's overrides invisible to the model/tool layer.
	wf, wfHash, iterFile, workflowName, bundleHandle, bundleCleanup, err := resolveWorkflow(opts)
	// Install cleanup BEFORE the error check: resolveWorkflow returns a live
	// cleanup (the .botz temp-dir remover) even on a bundle compile error, so
	// returning on err without deferring it leaks the extracted bundle dir.
	if bundleCleanup != nil {
		defer func() {
			if cerr := bundleCleanup(); cerr != nil {
				logger.Warn("bundle cleanup: %v", cerr)
			}
		}()
	}
	if err != nil {
		return err
	}

	// Apply CLI budget overrides AFTER the workflow (and any recipe/preset
	// budget) is resolved, but BEFORE buildRunExecutor — the executor
	// snapshots Budget at construction, so a later mutation would be
	// invisible to the model/cost layer. Validation happens up-front so a
	// malformed --max-duration fails fast instead of being silently dropped.
	if err := opts.Budget.Validate(); err != nil {
		return UserInputError(err)
	}
	applyBudgetOverrides(wf, opts.Budget)

	// DSL-declared supervisors (`supervisor NAME:`): wire an in-process
	// event hub onto the engine so each coordinator can observe this run
	// live. Injection (store-direct) is set up after the store exists.
	var superviseHub *supervise.EventHub
	if len(wf.Supervisors) > 0 {
		superviseHub = supervise.NewEventHub()
		engineOpts = append(engineOpts, runtime.WithEventObserver(superviseHub.Publish))
	}

	runName := store.GenerateRunName(iterFile + ":" + runID)
	storeDir := store.ResolveStoreDir(filepath.Dir(iterFile), opts.StoreDir)

	logger, logCloser := teeRunLog(logger, level, storeDir, runID)
	if logCloser != nil {
		defer logCloser.Close()
		// Re-emit the engineOpts entry so the engine sees the tee'd
		// logger; WithLogger overwrites on each call, so appending is
		// sufficient.
		engineOpts = append(engineOpts, runtime.WithLogger(logger))
	}

	s, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("cannot create store: %w", err)
	}

	// Wrap the concrete pointer in the interface only when non-nil so
	// the typed-nil gotcha doesn't fire inside buildRunExecutor —
	// passing a nil *PrometheusExporter directly yields a non-nil
	// interface value whose method dispatch panics on the first
	// callback.
	var exporterHooks exporterEventHooks
	if telemetry.prometheus != nil {
		exporterHooks = telemetry.prometheus
	}
	executor, err := buildRunExecutor(opts, wf, s, runID, storeDir, logger, exporterHooks)
	if err != nil {
		return err
	}
	if c, ok := executor.(io.Closer); ok {
		defer func() {
			if cerr := c.Close(); cerr != nil {
				logger.Warn("executor close: %v", cerr)
			}
		}()
	}
	if err := runview.MCPHealthCheck(ctx, executor, wf.ActiveMCPServers); err != nil {
		if opts.SkipMCPHealth || skipMCPHealthFromEnv() {
			logger.Warn("MCP health-check failed but tolerated (--skip-mcp-health / ITERION_SKIP_MCP_HEALTH): %v", err)
		} else {
			return err
		}
	}

	// Wire the subbot runner so `subbot` nodes can run a child .bot as a
	// nested run in the same store. Resolved relative to the parent .bot.
	engineOpts = append(engineOpts, runtime.WithSubbotRunner(
		subbotRunnerForCLI(iterFile, storeDir, s, logger, opts)))

	eng := buildEngine(wf, s, executor, opts, wfHash, iterFile, runName, bundleHandle, engineOpts)

	// Fold the bundle's file-based presets (presets/<name>.md) into the
	// workflow's preset set so `--preset <name>` resolves them and their var
	// overrides flow through buildRunInputs. The engine re-applies this as a
	// backstop and logs any malformed preset files.
	runtime.MergeBundlePresets(wf, bundleHandle, nil)

	inputs, err := buildRunInputs(wf, opts.Preset, opts.Vars)
	if err != nil {
		return err
	}

	// Resolve the mono/dual review topology for bi-model review-loop bots.
	// No-op unless the workflow declares a review_mode var; then detect
	// host provider credentials and inject the resolved review_mode +
	// mono_family (the --review-mode flag / a --var override wins over
	// auto-detection). See pkg/reviewtopology.
	if mode, family, injected := reviewtopology.InjectIfDeclared(wf, inputs, detect.Detect(ctx), opts.ReviewMode); injected {
		if family != "" {
			logger.Info("review topology: %s (family %s)", mode, family)
		} else {
			logger.Info("review topology: %s", mode)
		}
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// Acquire exclusive run lock. Use the SIGINT-aware ctx so a
	// contended lock can still be interrupted by Ctrl-C rather than
	// blocking forever.
	lock, err := s.LockRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("cannot acquire run lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// Managed-runner mode: the studio server writes the .pid file on
	// our behalf at spawn time, so we only need to remove it on exit.
	// The server's reconciler then flips this run to a terminal status
	// without waiting for the next reconcile sweep.
	if opts.Background {
		defer func() {
			if rmErr := s.RemovePIDFile(runID); rmErr != nil {
				logger.Warn("background: remove .pid: %v", rmErr)
			}
		}()
	}

	if p.Format == OutputHuman {
		p.Header("Run: " + workflowName)
		p.KV("Run name", runName)
		p.KV("Run ID", runID)
		p.KV("Store", storeDir)
		p.KV("Log Level", level.String())
		if opts.Timeout > 0 {
			p.KV("Timeout", FormatDuration(opts.Timeout))
		}
		p.Blank()
	}

	// Opt-in sandbox pre-flight (ITERION_SANDBOX_PREFLIGHT): validate the
	// effective sandbox spec before booting the engine so a misconfig
	// (bad image, daemon down, host_state-vs-k8s, malformed allowlist)
	// surfaces in ~1s with a remediation hint instead of 30s into the run.
	if sandboxPreflightEnabled() {
		pfOpts := SandboxDoctorOptions{
			File:                opts.File,
			Sandbox:             opts.Sandbox,
			SandboxDefaultImage: opts.SandboxDefaultImage,
			SandboxHostState:    opts.SandboxHostState,
			Target:              "auto",
		}
		if pfErr := PreflightSandbox(ctx, wf, pfOpts, func(status CheckStatus, c SandboxCheck) {
			if status == CheckFail {
				logger.Error("sandbox preflight [%s]: %s (hint: %s)", c.Name, c.Detail, c.Remediation)
			} else {
				logger.Warn("sandbox preflight [%s]: %s", c.Name, c.Detail)
			}
		}); pfErr != nil {
			return pfErr
		}
	}

	if superviseHub != nil {
		stop := startCLISupervisors(ctx, superviseHub, s, runID, wf, logger)
		defer stop()
	}

	err = eng.Run(ctx, runID, inputs)
	err = runInteractiveResumeLoop(ctx, eng, s, runID, opts.NoInteractive, err)
	err = autoResumeLoop(ctx, eng, s, runID, resolveAutoResume(opts.AutoResume, opts.Budget), err, logger)

	runResult := map[string]any{
		"run_id":   runID,
		"workflow": workflowName,
		"store":    storeDir,
	}
	return reportRunOutcome(p, s, runID, storeDir, opts.File, err, runResult)
}

// startCLISupervisors spawns a supervise.Coordinator for every
// `supervisor NAME:` block on the workflow, observing this run through
// the in-process hub and steering via a store-direct injector (same
// store handle as the engine, so the inbox doorbell stays in lockstep).
// Returns a stop func to drain them when the run ends.
func startCLISupervisors(ctx context.Context, hub *supervise.EventHub, s store.RunStore, runID string, wf *ir.Workflow, logger *iterlog.Logger) func() {
	inj := &supervise.StoreInjector{Store: s}
	var coords []*supervise.Coordinator
	for _, sup := range wf.Supervisors {
		system := ""
		if sup.System != "" {
			if p, ok := wf.Prompts[sup.System]; ok && p != nil {
				system = p.Body
			}
		}
		spec := supervise.Spec{
			Name:     sup.Name,
			Model:    sup.Model,
			System:   system,
			Watches:  sup.Watches,
			Cooldown: sup.Cooldown,
			MaxEvals: sup.MaxEvals,
		}
		coord := supervise.New(hub, inj, runID, spec, nil, logger)
		if coord == nil {
			continue
		}
		coord.Start(ctx)
		coords = append(coords, coord)
	}
	return func() {
		for _, c := range coords {
			c.Close()
		}
	}
}

// teeRunLog defers to store.TeeRunLog so the dispatcher and any
// other in-process runner share the same per-run log-file convention.
// Kept as a thin wrapper for the CLI's call sites; no behavior change.
func teeRunLog(logger *iterlog.Logger, level iterlog.Level, storeRoot, runID string) (*iterlog.Logger, io.Closer) {
	return store.TeeRunLog(logger, level, storeRoot, runID)
}

// buildRunExecutor constructs the default ClawExecutor for the run
// unless opts.Executor already supplies one (test path). Prometheus
// hooks are wired in when the exporter started so the executor emits
// the same per-turn metrics as the engine.
func buildRunExecutor(
	opts RunOptions,
	wf *ir.Workflow,
	s store.RunStore,
	runID, storeDir string,
	logger *iterlog.Logger,
	exporter exporterEventHooks,
) (runtime.NodeExecutor, error) {
	if opts.Executor != nil {
		return opts.Executor, nil
	}
	modelOverrides, err := model.ParseModelOverrides(opts.ModelFor, opts.BackendFor)
	if err != nil {
		return nil, err
	}
	execSpec := runview.ExecutorSpec{
		Workflow:        wf,
		Vars:            opts.Vars,
		Store:           s,
		RunID:           runID,
		Logger:          logger,
		StoreDir:        storeDir,
		Compress:        opts.Compress,
		Permission:      opts.Permission,
		PermissionAllow: opts.PermissionAllow,
		PermissionAsk:   opts.PermissionAsk,
		PermissionDeny:  opts.PermissionDeny,
		ModelOverrides:  modelOverrides,
		// Wire the operator-message inbox so queued messages (a CLI
		// `iterion supervise` attach, a DSL-declared supervisor, or a
		// future CLI chatbox) are drained at the agent's turn boundaries.
		// Studio/server wire this via service_launch; the CLI did not,
		// so supervisor steering silently never reached the agent.
		Inbox: &model.StoreInboxBinder{Store: s},
		// Async questions (ADR-081) work CLI-side too: answers written to
		// the same store (studio, `iterion runs answer`, REST) reach the
		// agent through the store-backed inbox; the await node's poll
		// ticker covers the cross-process doorbell gap.
		AsyncAsk: &model.StoreAsyncAskBinder{Store: s},
	}
	localStore, localSealer, err := localSecretsForRun(len(wf.Secrets) > 0, storeDir, logger)
	if err != nil {
		return nil, err
	}
	execSpec.LocalSecrets = localStore
	execSpec.LocalSealer = localSealer
	if exporter != nil {
		execSpec.ExtraHooks = append(execSpec.ExtraHooks, exporter.EventHooks())
	}
	return runview.BuildExecutor(execSpec)
}

// maxSubbotDepth bounds nested subbot recursion so a child that (directly or
// transitively) runs its own parent can't blow the stack. The wall-clock
// budget/timeout still applies; this is the structural backstop.
const maxSubbotDepth = 8

type subbotDepthKey struct{}

// subbotRunnerForCLI builds the runtime.SubbotRunner used by `subbot` nodes in
// CLI runs: it compiles the child .bot (resolved relative to the parent),
// builds a child executor + engine sharing the parent's store, runs it with the
// resolved `with:` data as inputs, and returns the child's terminal node
// output (mapped to outputs.<subbot>.<field> by the engine).
func subbotRunnerForCLI(parentPath, storeDir string, s store.RunStore, logger *iterlog.Logger, opts RunOptions) runtime.SubbotRunner {
	parentDir := filepath.Dir(parentPath)
	return func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		depth, _ := ctx.Value(subbotDepthKey{}).(int)
		if depth >= maxSubbotDepth {
			return nil, fmt.Errorf("subbot recursion too deep (>%d) at %q — possible cycle", maxSubbotDepth, req.Source)
		}

		childPath := req.Source
		if !filepath.IsAbs(childPath) {
			childPath = filepath.Join(parentDir, childPath)
		}
		childWf, hash, err := runview.CompileWorkflowWithHash(childPath)
		if err != nil {
			return nil, fmt.Errorf("compile child %q: %w", req.Source, err)
		}
		childRunID, err := store.GenerateRunID()
		if err != nil {
			return nil, err
		}

		childExec, err := buildRunExecutor(opts, childWf, s, childRunID, storeDir, logger, nil)
		if err != nil {
			return nil, err
		}
		if c, ok := childExec.(io.Closer); ok {
			defer func() { _ = c.Close() }()
		}

		// Capture the child's terminal-node output (the last node before Done)
		// as the subbot's result. The callback fires concurrently when the
		// child fans out parallel branches, so the capture is mutex-guarded.
		var (
			lastMu sync.Mutex
			last   map[string]any
		)
		childEng := runtime.New(childWf, s, childExec,
			runtime.WithLogger(logger),
			runtime.WithWorkflowHash(hash),
			runtime.WithFilePath(childPath),
			runtime.WithParentRunID(req.ParentRunID),
			runtime.WithParentNodeID(req.NodeID),
			// Wire the child engine with its own recursive runner so a child
			// .bot that itself declares subbot nodes can run them (sources
			// resolve relative to the CHILD's dir). Without this, nested
			// subbots died with "no SubbotRunner is wired" even though the
			// depth guard below exists precisely to bound that recursion.
			runtime.WithSubbotRunner(subbotRunnerForCLI(childPath, storeDir, s, logger, opts)),
			runtime.WithOnNodeFinished(func(_, _ string, out map[string]any) {
				if out != nil {
					lastMu.Lock()
					last = out
					lastMu.Unlock()
				}
			}),
		)
		childCtx := context.WithValue(ctx, subbotDepthKey{}, depth+1)
		if err := childEng.Run(childCtx, childRunID, req.Vars); err != nil {
			// A human gate inside the child pauses the CHILD run (its doc is
			// paused_waiting_human with a checkpoint + interaction); that is
			// not a parent failure. Park this subbot node until the operator
			// answers the child's review (pipeline-board sidebar / `iterion
			// resume --run-id <child>`) and the child reaches a terminal
			// state, then pick up its output from the store.
			if errors.Is(err, runtime.ErrRunPaused) || errors.Is(err, runtime.ErrRunPausedOperator) {
				return runview.AwaitSubbotTerminal(childCtx, s, childRunID, logger)
			}
			return nil, err
		}
		return last, nil
	}
}

// exporterEventHooks is the narrow subset of the Prometheus exporter
// surface buildRunExecutor depends on; using an interface lets the
// helper accept (*benchmark.PrometheusExporter)(nil) without importing
// the benchmark package here (it already lives in run_telemetry.go).
type exporterEventHooks interface {
	EventHooks() model.EventHooks
}

// buildEngine wires the per-run engine options that flow from the CLI
// flags + env. Kept out of RunRun so the orchestrator focuses on
// lifecycle rather than the option-slice plumbing.
func buildEngine(
	wf *ir.Workflow,
	s store.RunStore,
	executor runtime.NodeExecutor,
	opts RunOptions,
	wfHash, iterFile, runName string,
	bundleHandle *bundle.Bundle,
	base []runtime.EngineOption,
) *runtime.Engine {
	sandboxDefault := strings.ToLower(os.Getenv("ITERION_SANDBOX_DEFAULT"))
	sandboxHostStateDefault := strings.ToLower(os.Getenv("ITERION_SANDBOX_HOST_STATE"))
	return runtime.New(wf, s, executor,
		append(base,
			runtime.WithWorkflowHash(wfHash),
			runtime.WithFilePath(iterFile),
			runtime.WithRunName(runName),
			runtime.WithMergeInto(opts.MergeInto),
			runtime.WithBranchName(opts.BranchName),
			runtime.WithMergeStrategy(opts.MergeStrategy),
			runtime.WithAutoMerge(opts.AutoMerge),
			runtime.WithSandboxOverride(opts.Sandbox),
			runtime.WithSandboxDefault(sandboxDefault),
			runtime.WithSandboxDefaultImage(opts.SandboxDefaultImage),
			runtime.WithSandboxHostStateOverride(opts.SandboxHostState),
			runtime.WithSandboxHostStateDefault(sandboxHostStateDefault),
			runtime.WithBundle(bundleHandle),
			runtime.WithPreset(opts.Preset),
			runtime.WithSource(opts.Source),
		)...,
	)
}

// resolveWorkflow loads the workflow either via a recipe, a `.botz`
// bundle, or directly from a .bot file. When a recipe is given, its
// overrides are applied before the workflow is returned so the caller
// can hand a fully-realised workflow to BuildExecutor (which snapshots
// the policy fields at construction time). When the input is a
// bundle, the workflow source is extracted and any `prompts/*.md`
// files are merged into the compiled IR before any other consumer
// sees it.
//
// The returned bundle pointer is non-nil only for bundle inputs; the
// caller wires it into the engine via runtime.WithBundle and is also
// responsible for the cleanup function (no-op for cache-hit paths but
// reserved for future per-run extraction modes).
func resolveWorkflow(opts RunOptions) (wf *ir.Workflow, hash, filePath, displayName string, b *bundle.Bundle, cleanup func() error, err error) {
	cleanup = func() error { return nil }
	if opts.Recipe != "" {
		spec, recipeErr := recipe.LoadFile(opts.Recipe)
		if recipeErr != nil {
			return nil, "", "", "", nil, cleanup, fmt.Errorf("cannot load recipe: %w", recipeErr)
		}
		filePath = opts.File
		if filePath == "" {
			filePath = spec.WorkflowRef.Path
		}
		if filePath == "" {
			return nil, "", "", "", nil, cleanup, fmt.Errorf("recipe %q does not specify a workflow path; provide --file", spec.Name)
		}
		filePath = ResolveRecipePath(filePath)
		if !workflowfile.IsWorkflowFile(filePath) {
			return nil, "", "", "", nil, cleanup, fmt.Errorf("recipe workflow path %q must end in .bot", filePath)
		}
		raw, h, compileErr := runview.CompileWorkflowWithHash(filePath)
		if compileErr != nil {
			return nil, "", "", "", nil, cleanup, compileErr
		}
		applied, applyErr := spec.Apply(raw)
		if applyErr != nil {
			return nil, "", "", "", nil, cleanup, fmt.Errorf("runtime: apply recipe %q: %w", spec.Name, applyErr)
		}
		return applied, h, filePath, spec.Name + " (" + applied.Name + ")", nil, cleanup, nil
	}
	if opts.File == "" {
		return nil, "", "", "", nil, cleanup, fmt.Errorf("provide a .bot file, .botz bundle, or --recipe")
	}
	resolved := ResolveRecipePath(opts.File)
	if existErr := requireWorkflowPathExists(resolved); existErr != nil {
		return nil, "", "", "", nil, cleanup, existErr
	}
	opened, iterPath, _, c, openErr := openBundleOrFile(resolved)
	if openErr != nil {
		return nil, "", "", "", nil, cleanup, openErr
	}
	if opened != nil {
		cleanup = c
		raw, h, compileErr := runview.CompileBundleWorkflow(iterPath, opened)
		if compileErr != nil {
			return nil, "", "", "", opened, cleanup, compileErr
		}
		display := raw.Name
		if opened.Manifest != nil && opened.Manifest.Name != "" {
			display = opened.Manifest.Name + " (" + raw.Name + ")"
		}
		return raw, h, iterPath, display, opened, cleanup, nil
	}
	// F-NEW-4: when the operator points at a bare `main.bot` file whose
	// parent directory looks like a bundle (has skills/ or
	// manifest.yaml), promote to KindBundleDir on the parent so the
	// runtime mirrors the bundled skills/ into .claude/skills/ at run
	// time (as <name>/SKILL.md — the directory form claude_code's Skill
	// tool discovers). Without this promotion, nodes that invoke a skill
	// silently get nothing on bare-file launches — observed with
	// bots/whats-next/main.bot whose nodes invoke skills like repo-survey.
	if parent := bundleParentOf(resolved); parent != "" {
		opened, openErr := bundle.OpenDir(parent)
		if openErr == nil {
			raw, h, compileErr := runview.CompileBundleWorkflow(opened.IterPath, opened)
			if compileErr != nil {
				return nil, "", "", "", opened, cleanup, compileErr
			}
			display := raw.Name
			if opened.Manifest != nil && opened.Manifest.Name != "" {
				display = opened.Manifest.Name + " (" + raw.Name + ")"
			}
			return raw, h, opened.IterPath, display, opened, cleanup, nil
		}
		// On openErr, fall through to bare-file compile — better than
		// failing outright; the parent merely "looked like" a bundle.
	}
	raw, h, compileErr := runview.CompileWorkflowWithHash(resolved)
	if compileErr != nil {
		return nil, "", "", "", nil, cleanup, compileErr
	}
	return raw, h, resolved, raw.Name, nil, cleanup, nil
}

// bundleParentOf returns the absolute path of `path`'s parent directory
// when that parent is a bundle and `path` is its main.bot. Returns "" when
// no promotion is warranted.
//
// Conservative on purpose — promoting an arbitrary `*.bot` inside a
// folder with a sibling `skills/` could surprise operators who
// intentionally split bundle vs. one-off bots. What counts as a bundle is
// pkg/bundle's to define, not this package's.
func bundleParentOf(path string) string {
	return bundle.DirForMainBot(path)
}

// enrichPausedResult loads checkpoint and interaction details from the store
// and populates the result map with interaction_id, node_id, and questions.
// It is used by both run and resume to enrich paused-output for CI consumers.
func enrichPausedResult(s store.RunStore, runID string, result map[string]any) {
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil || r.Checkpoint == nil {
		return
	}
	result["interaction_id"] = r.Checkpoint.InteractionID
	result["node_id"] = r.Checkpoint.NodeID
	if interaction, err := s.LoadInteraction(context.Background(), runID, r.Checkpoint.InteractionID); err == nil {
		result["questions"] = interaction.Questions
	}
}

// printPausedQuestions prints human-readable question details from the result map.
func printPausedQuestions(p *Printer, result map[string]any) {
	q, ok := result["questions"].(map[string]any)
	if !ok || len(q) == 0 {
		return
	}
	keys := slices.Sorted(maps.Keys(q))
	p.Line("  Questions:")
	for _, k := range keys {
		p.Line("    %s: %v", k, q[k])
	}
}

// ParseVarFlags parses a slice of "key=value" strings into a map.
func ParseVarFlags(flags []string) (map[string]string, error) {
	return parseKVPairs[string](flags, kvOpts[string]{
		errFmt: "invalid --var format %q (expected key=value)",
	})
}

// ParseAnswersFile reads a JSON file containing answer key-value pairs.
func ParseAnswersFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read answers file: %w", err)
	}
	var answers map[string]any
	if err := json.Unmarshal(data, &answers); err != nil {
		return nil, fmt.Errorf("cannot parse answers file: %w", err)
	}
	return answers, nil
}
