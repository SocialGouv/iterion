package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// RunOptions holds the configuration for the run command.
type RunOptions struct {
	File          string               // .iter file path
	Recipe        string               // recipe JSON file path (alternative to File)
	Vars          map[string]string    // --var key=value overrides
	Preset        string               // --preset <name>: applies an in-source named preset before --var
	RunID         string               // explicit run ID (auto-generated if empty)
	StoreDir      string               // store directory (default: nearest .iterion ancestor of the .iter file, or alongside it)
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
	// override prompts/tools/budget) or directly from a .iter file.
	// Recipe overrides MUST be applied before BuildExecutor — the
	// executor snapshots Prompts/Schemas/ToolPolicy/Budget/Compaction
	// at construction time, so feeding it the raw workflow would make
	// the recipe's overrides invisible to the model/tool layer.
	wf, wfHash, iterFile, workflowName, bundleHandle, bundleCleanup, err := resolveWorkflow(opts)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := bundleCleanup(); cerr != nil {
			logger.Warn("bundle cleanup: %v", cerr)
		}
	}()

	runName := store.GenerateRunName(iterFile + ":" + runID)
	storeDir := store.ResolveStoreDir(filepath.Dir(iterFile), opts.StoreDir)

	logger, logCloser := teeRunLog(logger, level, filepath.Join(storeDir, "runs", runID))
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

	executor, err := buildRunExecutor(opts, wf, s, runID, storeDir, logger, telemetry.prometheus)
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
		return err
	}

	eng := buildEngine(wf, s, executor, opts, wfHash, iterFile, runName, bundleHandle, engineOpts)

	inputs, err := buildRunInputs(wf, opts.Preset, opts.Vars)
	if err != nil {
		return err
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
	defer lock.Unlock()

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

	err = eng.Run(ctx, runID, inputs)
	err = runInteractiveResumeLoop(ctx, eng, s, runID, opts.NoInteractive, err)

	runResult := map[string]interface{}{
		"run_id":   runID,
		"workflow": workflowName,
		"store":    storeDir,
	}
	return reportRunOutcome(p, s, runID, storeDir, opts.File, err, runResult)
}

// teeRunLog defers to store.TeeRunLog so the dispatcher and any
// other in-process runner share the same per-run log-file convention.
// Kept as a thin wrapper for the CLI's call sites; no behavior change.
func teeRunLog(logger *iterlog.Logger, level iterlog.Level, runDir string) (*iterlog.Logger, io.Closer) {
	return store.TeeRunLog(logger, level, runDir)
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
	execSpec := runview.ExecutorSpec{
		Workflow: wf,
		Vars:     opts.Vars,
		Store:    s,
		RunID:    runID,
		Logger:   logger,
		StoreDir: storeDir,
	}
	if exporter != nil {
		execSpec.ExtraHooks = append(execSpec.ExtraHooks, exporter.EventHooks())
	}
	return runview.BuildExecutor(execSpec)
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
		)...,
	)
}

// resolveWorkflow loads the workflow either via a recipe, a `.botz`
// bundle, or directly from a .iter file. When a recipe is given, its
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
		return nil, "", "", "", nil, cleanup, fmt.Errorf("provide a .iter file, .botz bundle, or --recipe")
	}
	resolved := ResolveRecipePath(opts.File)
	kind, detectErr := bundle.Detect(resolved)
	if detectErr != nil {
		return nil, "", "", "", nil, cleanup, detectErr
	}
	switch kind {
	case bundle.KindBundle:
		opened, c, openErr := bundle.Open(resolved, "")
		if openErr != nil {
			return nil, "", "", "", nil, cleanup, openErr
		}
		cleanup = c
		raw, h, compileErr := runview.CompileBundleWorkflow(opened.IterPath, opened)
		if compileErr != nil {
			return nil, "", "", "", opened, cleanup, compileErr
		}
		display := raw.Name
		if opened.Manifest != nil && opened.Manifest.Name != "" {
			display = opened.Manifest.Name + " (" + raw.Name + ")"
		}
		return raw, h, opened.IterPath, display, opened, cleanup, nil
	case bundle.KindBundleDir:
		opened, openErr := bundle.OpenDir(resolved)
		if openErr != nil {
			return nil, "", "", "", nil, cleanup, openErr
		}
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
	// F-NEW-4: when the operator points at a bare `.bot` / `.iter`
	// file whose parent directory looks like a bundle (has skills/ or
	// manifest.yaml), promote to KindBundleDir on the parent so the
	// runtime mirrors the bundled skills/ into .claude/skills/ at run
	// time. Without this promotion, prompts that read
	// `.claude/skills/<name>.md` silently get nothing on bare-file
	// launches — observed with examples/whats-next/main.bot where the
	// explore prompt reads `.claude/skills/repo-survey.md`.
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

// bundleParentOf returns the absolute path of `path`'s parent
// directory when the parent looks like a bundle (has skills/ or
// manifest.yaml) AND `path` is named main.bot / main.iter (the canonical
// bundle entrypoint). Returns "" when no promotion is warranted.
// Conservative on purpose — promoting an arbitrary `*.bot` inside a
// folder with a sibling `skills/` could surprise operators who
// intentionally split bundle vs. one-off bots.
func bundleParentOf(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	base := filepath.Base(abs)
	if base != "main.bot" && base != "main.iter" {
		return ""
	}
	parent := filepath.Dir(abs)
	for _, marker := range []string{"skills", "manifest.yaml"} {
		if _, err := os.Stat(filepath.Join(parent, marker)); err == nil {
			return parent
		}
	}
	return ""
}

// enrichPausedResult loads checkpoint and interaction details from the store
// and populates the result map with interaction_id, node_id, and questions.
// It is used by both run and resume to enrich paused-output for CI consumers.
func enrichPausedResult(s store.RunStore, runID string, result map[string]interface{}) {
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
func printPausedQuestions(p *Printer, result map[string]interface{}) {
	q, ok := result["questions"].(map[string]interface{})
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
	vars := make(map[string]string)
	for _, f := range flags {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --var format %q (expected key=value)", f)
		}
		vars[parts[0]] = parts[1]
	}
	return vars, nil
}

// ParseAnswersFile reads a JSON file containing answer key-value pairs.
func ParseAnswersFile(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read answers file: %w", err)
	}
	var answers map[string]interface{}
	if err := json.Unmarshal(data, &answers); err != nil {
		return nil, fmt.Errorf("cannot parse answers file: %w", err)
	}
	return answers, nil
}
