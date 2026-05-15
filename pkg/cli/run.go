package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/apikit/telemetry/otlpgrpc"

	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/benchmark"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sortedKeys returns the keys of a map sorted alphabetically.
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
	// spawned by the editor server. The CLI writes a .pid file so the
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
	// editor sets false by default to defer merge to a UI action.
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
}

// RunRun executes a workflow or recipe and reports the outcome.
func RunRun(ctx context.Context, opts RunOptions, p *Printer) error {
	// Resolve log level early so the logger is available for store creation
	// and downstream subsystems.
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

	// Resolve run ID.
	runID := opts.RunID
	if runID == "" {
		runID = fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}

	// Optional Prometheus exporter (env-controlled, see docs/observability/).
	exporter, metricsServer, metricsErr := startPrometheusFromEnv(runID, logger)
	if metricsErr != nil {
		return metricsErr
	}
	if metricsServer != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
	}
	// Optional OTLP/gRPC exporter (env-controlled, see docs/observability/).
	otlpExporter, otlpErr := startOTLPGRPCFromEnv(runID, logger)
	if otlpErr != nil {
		return otlpErr
	}
	if otlpExporter != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = otlpExporter.Stop(shutdownCtx)
		}()
	}
	engineOpts := []runtime.EngineOption{
		runtime.WithLogger(logger),
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
	}
	if exporter != nil {
		engineOpts = append(engineOpts, runtime.WithEventObserver(exporter.EventObserver()))
	}
	if otlpExporter != nil {
		engineOpts = append(engineOpts, runtime.WithEventObserver(otlpExporter.EventObserver()))
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

	// Tee the logger output into <storeDir>/runs/<runID>/run.log so the
	// editor's Logs tab and the per-run log buffer (used by the WS
	// subscription that drives RunLogPanel) see the same content as the
	// CLI's stderr. Without this, CLI-launched runs show "No log
	// captured." in the editor — the daemon-launched path tees via
	// runview.Service.prepareRunLog, but a direct `iterion run`
	// invocation bypasses runview entirely. Errors are warned-and-
	// continue: a CLI run with no writable store dir still works (logs
	// go to stderr only) instead of failing the boot over a feature
	// the operator may not be using right now.
	runDir := filepath.Join(storeDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		logger.Warn("cli: mkdir run dir for log tee: %v", err)
	} else if logFile, openErr := os.OpenFile(filepath.Join(runDir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); openErr != nil {
		logger.Warn("cli: open run.log for tee: %v", openErr)
	} else {
		defer logFile.Close()
		logger = iterlog.New(level, io.MultiWriter(os.Stderr, logFile))
		// Re-emit the engineOpts entries that captured the stderr-only
		// logger so the engine sees the tee'd one. WithLogger overwrites
		// e.logger on each call, so appending is sufficient.
		engineOpts = append(engineOpts, runtime.WithLogger(logger))
	}

	s, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("cannot create store: %w", err)
	}

	executor := opts.Executor
	if executor == nil {
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
		exec, execErr := runview.BuildExecutor(execSpec)
		if execErr != nil {
			return execErr
		}
		executor = exec
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

	sandboxDefault := strings.ToLower(os.Getenv("ITERION_SANDBOX_DEFAULT"))

	eng := runtime.New(wf, s, executor,
		append(engineOpts,
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
			runtime.WithBundle(bundleHandle),
			runtime.WithPreset(opts.Preset),
		)...,
	)

	// Build run inputs. Precedence (lowest → highest):
	//   1. vars: defaults (applied by the engine when a key is unset here)
	//   2. --preset <name>: in-source named preset values
	//   3. --var key=value: CLI overrides
	// Recipe presets are applied earlier in resolveWorkflow.
	inputs := make(map[string]interface{})
	if opts.Preset != "" {
		preset, ok := wf.Presets[opts.Preset]
		if !ok {
			available := make([]string, 0, len(wf.Presets))
			for name := range wf.Presets {
				available = append(available, name)
			}
			sort.Strings(available)
			if len(available) == 0 {
				return fmt.Errorf("--preset %q: workflow has no presets declared", opts.Preset)
			}
			return fmt.Errorf("--preset %q: unknown preset (available: %s)", opts.Preset, strings.Join(available, ", "))
		}
		for k, v := range preset.Values {
			inputs[k] = v
		}
	}
	for k, v := range opts.Vars {
		inputs[k] = v
	}

	// Apply timeout to context if specified.
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// Acquire exclusive run lock to prevent concurrent processes.
	lock, err := s.LockRun(context.Background(), runID)
	if err != nil {
		return fmt.Errorf("cannot acquire run lock: %w", err)
	}
	defer lock.Unlock()

	// Managed-runner mode: the editor server writes the .pid file on
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

	// Execute.
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

	// Interactive TTY loop: when paused at a human node and stdin is a terminal,
	// prompt the user for answers and resume in the same process.
	for errors.Is(err, runtime.ErrRunPaused) && !opts.NoInteractive && IsTTY() {
		// Each error path replaces err so the outer reporting reflects
		// the actual failure rather than the stale ErrRunPaused: a load
		// or prompt error here was previously swallowed by `break`,
		// leaving the user with a paused_waiting_human status that hid
		// the real issue (corrupt checkpoint, missing interaction file,
		// stdin closed mid-prompt, etc.).
		r, loadErr := s.LoadRun(context.Background(), runID)
		if loadErr != nil {
			err = fmt.Errorf("interactive resume: load run: %w", loadErr)
			break
		}
		if r.Checkpoint == nil {
			err = fmt.Errorf("interactive resume: run %q has no checkpoint", runID)
			break
		}
		interaction, loadErr := s.LoadInteraction(context.Background(), runID, r.Checkpoint.InteractionID)
		if loadErr != nil {
			err = fmt.Errorf("interactive resume: load interaction: %w", loadErr)
			break
		}

		answers, promptErr := PromptHumanAnswers(interaction)
		if promptErr != nil {
			err = fmt.Errorf("interactive resume: prompt answers: %w", promptErr)
			break
		}
		err = eng.Resume(ctx, runID, answers)
	}

	// Build result.
	runResult := map[string]interface{}{
		"run_id":   runID,
		"workflow": workflowName,
		"store":    storeDir,
	}

	if err != nil {
		if errors.Is(err, runtime.ErrRunPaused) {
			runResult["status"] = "paused_waiting_human"
			if opts.File != "" {
				runResult["file"] = opts.File
			}
			enrichPausedResult(s, runID, runResult)

			if p.Format == OutputJSON {
				p.JSON(runResult)
			} else {
				p.Line("  Status: PAUSED (waiting for human input)")
				printPausedQuestions(p, runResult)
				p.Line("  Resume: iterion resume --run-id %s --store-dir %s --answers-file <file>", runID, storeDir)
			}
			return nil
		}
		if errors.Is(err, runtime.ErrRunCancelled) {
			runResult["status"] = "cancelled"
			if p.Format == OutputJSON {
				p.JSON(runResult)
			} else {
				p.Line("  Status: CANCELLED")
				p.Line("  Detail: %s", err.Error())
			}
			return err
		}
		runResult["status"] = "failed"
		runResult["error"] = err.Error()
		if p.Format == OutputJSON {
			p.JSON(runResult)
		} else {
			p.Line("  Status: FAILED")
			p.Line("  Error:  %s", err.Error())
			p.Line("  Hint:   use 'iterion inspect --run-id %s --events' for details", runID)
		}
		return err
	}

	runResult["status"] = "finished"
	if p.Format == OutputJSON {
		p.JSON(runResult)
	} else {
		p.Line("  Status: FINISHED")
	}
	return nil
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
	raw, h, compileErr := runview.CompileWorkflowWithHash(resolved)
	if compileErr != nil {
		return nil, "", "", "", nil, cleanup, compileErr
	}
	return raw, h, resolved, raw.Name, nil, cleanup, nil
}

// startPrometheusFromEnv builds a PrometheusExporter and serves /metrics
// on the address from the ITERION_PROMETHEUS_ADDR env var (e.g. ":9464").
// Returns (nil, nil, nil) when the env var is empty.
//
// The HTTP server runs in a goroutine; the caller should Shutdown it on
// exit. By default ListenAndServe failures (port in use, permission) are
// logged at error level and the exporter is returned anyway so the rest
// of the run can proceed (fail-soft).
//
// When ITERION_PROMETHEUS_REQUIRED is truthy, the address is bound
// synchronously upfront so a startup failure (port in use, missing
// permission, malformed addr) is surfaced as an error instead of being
// hidden in the background goroutine.
func startPrometheusFromEnv(runID string, logger *iterlog.Logger) (*benchmark.PrometheusExporter, *http.Server, error) {
	addr := strings.TrimSpace(os.Getenv("ITERION_PROMETHEUS_ADDR"))
	if addr == "" {
		return nil, nil, nil
	}
	required := isTruthyEnv("ITERION_PROMETHEUS_REQUIRED")
	exporter := benchmark.NewPrometheusExporter(runID, nil)
	mux := http.NewServeMux()
	mux.Handle("/metrics", exporter.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if required {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("prometheus: bind %s (ITERION_PROMETHEUS_REQUIRED=1): %w", addr, err)
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("prometheus: serve %s: %v", addr, err)
			}
		}()
	} else {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("prometheus: serve %s: %v", addr, err)
			}
		}()
	}
	logger.Info("prometheus: serving /metrics on %s (run_id=%s)", addr, runID)
	return exporter, srv, nil
}

// startOTLPGRPCFromEnv builds an OTLP/gRPC exporter from environment
// configuration and registers it as a secondary observer on the run's
// event bus. Returns (nil, nil) when no endpoint is configured.
//
// Recognised env vars (claw-code-go upstream):
//
//	CLAWD_OTLP_GRPC_ENDPOINT   host:port or URL (required to enable)
//	CLAWD_OTLP_GRPC_INSECURE   "1" / "true" disables TLS
//	CLAWD_OTLP_GRPC_HEADERS    comma-separated key=value pairs
//	CLAWD_SERVICE_NAME         service.name resource attr
//	CLAWD_SERVICE_VERSION      service.version resource attr
//
// ITERION_OTLP_GRPC_ENDPOINT is honored as an iterion-prefixed alias for
// the endpoint so operators can keep CLAWD_* reserved for claw-internal
// traffic if their deployment runs both side-by-side.
func startOTLPGRPCFromEnv(runID string, logger *iterlog.Logger) (*benchmark.OTLPGRPCExporter, error) {
	if alias := strings.TrimSpace(os.Getenv("ITERION_OTLP_GRPC_ENDPOINT")); alias != "" {
		if os.Getenv(otlpgrpc.EnvEndpoint) == "" {
			_ = os.Setenv(otlpgrpc.EnvEndpoint, alias)
		}
	}

	cfg, err := otlpgrpc.FromEnv()
	if err != nil {
		if errors.Is(err, otlpgrpc.ErrEndpointMissing) {
			return nil, nil
		}
		return nil, fmt.Errorf("otlp/grpc: %w", err)
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = "iterion"
	}
	exp, err := benchmark.NewOTLPGRPCExporter(runID, cfg)
	if err != nil {
		return nil, fmt.Errorf("otlp/grpc: %w", err)
	}
	logger.Info("otlp/grpc: exporting to %s (run_id=%s, service=%s)", cfg.Endpoint, runID, cfg.ServiceName)
	return exp, nil
}

// isTruthyEnv returns true for the conventional "yes" values: 1, true, yes, on.
func isTruthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
	keys := sortedKeys(q)
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
