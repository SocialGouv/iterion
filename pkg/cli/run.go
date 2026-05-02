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
	RunID         string               // explicit run ID (auto-generated if empty)
	StoreDir      string               // store directory (default: nearest .iterion ancestor of the .iter file, or alongside it)
	Timeout       time.Duration        // maximum run duration (0 = no limit)
	LogLevel      string               // log level (default: "info", env: ITERION_LOG_LEVEL)
	NoInteractive bool                 // disable interactive TTY prompting on human pause
	Executor      runtime.NodeExecutor // pluggable executor (nil = stub)
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
	wf, wfHash, iterFile, workflowName, err := resolveWorkflow(opts)
	if err != nil {
		return err
	}

	storeDir := store.ResolveStoreDir(filepath.Dir(iterFile), opts.StoreDir)

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

	eng := runtime.New(wf, s, executor,
		append(engineOpts,
			runtime.WithWorkflowHash(wfHash),
			runtime.WithFilePath(iterFile),
		)...,
	)

	// Build run inputs from vars.
	inputs := make(map[string]interface{})
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
	lock, err := s.LockRun(runID)
	if err != nil {
		return fmt.Errorf("cannot acquire run lock: %w", err)
	}
	defer lock.Unlock()

	// Execute.
	if p.Format == OutputHuman {
		p.Header("Run: " + workflowName)
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
		r, loadErr := s.LoadRun(runID)
		if loadErr != nil {
			err = fmt.Errorf("interactive resume: load run: %w", loadErr)
			break
		}
		if r.Checkpoint == nil {
			err = fmt.Errorf("interactive resume: run %q has no checkpoint", runID)
			break
		}
		interaction, loadErr := s.LoadInteraction(runID, r.Checkpoint.InteractionID)
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

// resolveWorkflow loads the workflow either via a recipe or directly
// from a .iter file. When a recipe is given, its overrides are applied
// before the workflow is returned so the caller can hand a fully-
// realised workflow to BuildExecutor (which snapshots the policy
// fields at construction time).
func resolveWorkflow(opts RunOptions) (wf *ir.Workflow, hash, filePath, displayName string, err error) {
	if opts.Recipe != "" {
		spec, recipeErr := recipe.LoadFile(opts.Recipe)
		if recipeErr != nil {
			return nil, "", "", "", fmt.Errorf("cannot load recipe: %w", recipeErr)
		}
		filePath = opts.File
		if filePath == "" {
			filePath = spec.WorkflowRef.Path
		}
		if filePath == "" {
			return nil, "", "", "", fmt.Errorf("recipe %q does not specify a workflow path; provide --file", spec.Name)
		}
		raw, h, compileErr := runview.CompileWorkflowWithHash(filePath)
		if compileErr != nil {
			return nil, "", "", "", compileErr
		}
		applied, applyErr := spec.Apply(raw)
		if applyErr != nil {
			return nil, "", "", "", fmt.Errorf("runtime: apply recipe %q: %w", spec.Name, applyErr)
		}
		return applied, h, filePath, spec.Name + " (" + applied.Name + ")", nil
	}
	if opts.File == "" {
		return nil, "", "", "", fmt.Errorf("provide a .iter file or --recipe")
	}
	raw, h, compileErr := runview.CompileWorkflowWithHash(opts.File)
	if compileErr != nil {
		return nil, "", "", "", compileErr
	}
	return raw, h, opts.File, raw.Name, nil
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
func enrichPausedResult(s *store.RunStore, runID string, result map[string]interface{}) {
	r, err := s.LoadRun(runID)
	if err != nil || r.Checkpoint == nil {
		return
	}
	result["interaction_id"] = r.Checkpoint.InteractionID
	result["node_id"] = r.Checkpoint.NodeID
	if interaction, err := s.LoadInteraction(runID, r.Checkpoint.InteractionID); err == nil {
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
