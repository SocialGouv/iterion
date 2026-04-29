package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/mcp"
	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/recipe"
	"github.com/SocialGouv/iterion/runtime"
	"github.com/SocialGouv/iterion/store"
	"github.com/SocialGouv/iterion/tool"
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
	StoreDir      string               // store directory (default: .iterion)
	Timeout       time.Duration        // maximum run duration (0 = no limit)
	LogLevel      string               // log level (default: "info", env: ITERION_LOG_LEVEL)
	NoInteractive bool                 // disable interactive TTY prompting on human pause
	Executor      runtime.NodeExecutor // pluggable executor (nil = stub)
}

// RunRun executes a workflow or recipe and reports the outcome.
func RunRun(ctx context.Context, opts RunOptions, p *Printer) error {
	// Resolve store.
	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = ".iterion"
	}
	// Resolve log level early so the logger is available for store creation.
	level, err := iterlog.ResolveLevel(opts.LogLevel, "ITERION_LOG_LEVEL")
	if err != nil {
		return err
	}
	logger := iterlog.New(level, os.Stderr)

	s, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("cannot create store: %w", err)
	}

	// Resolve run ID.
	runID := opts.RunID
	if runID == "" {
		runID = fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}

	// Build engine: either from recipe or raw workflow.
	var eng *runtime.Engine
	var workflowName string

	if opts.Recipe != "" {
		// Load recipe.
		spec, err := recipe.LoadFile(opts.Recipe)
		if err != nil {
			return fmt.Errorf("cannot load recipe: %w", err)
		}

		// Resolve the .iter file path from recipe or option.
		iterFile := opts.File
		if iterFile == "" {
			iterFile = spec.WorkflowRef.Path
		}
		if iterFile == "" {
			return fmt.Errorf("recipe %q does not specify a workflow path; provide --file", spec.Name)
		}

		wf, wfHash, err := compileWorkflowWithHash(iterFile)
		if err != nil {
			return err
		}

		executor := opts.Executor
		if executor == nil {
			exec, execErr := newDefaultExecutor(wf, opts.Vars, s, runID, logger, storeDir)
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
		if err := mcpHealthCheck(ctx, executor, wf.ActiveMCPServers); err != nil {
			return err
		}

		eng, err = runtime.NewFromRecipe(spec, wf, s, executor, runtime.WithLogger(logger), runtime.WithWorkflowHash(wfHash))
		if err != nil {
			return err
		}
		workflowName = spec.Name + " (" + wf.Name + ")"
	} else {
		if opts.File == "" {
			return fmt.Errorf("provide a .iter file or --recipe")
		}

		wf, wfHash, err := compileWorkflowWithHash(opts.File)
		if err != nil {
			return err
		}

		executor := opts.Executor
		if executor == nil {
			exec, execErr := newDefaultExecutor(wf, opts.Vars, s, runID, logger, storeDir)
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
		if err := mcpHealthCheck(ctx, executor, wf.ActiveMCPServers); err != nil {
			return err
		}

		eng = runtime.New(wf, s, executor, runtime.WithLogger(logger), runtime.WithWorkflowHash(wfHash))
		workflowName = wf.Name
	}

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
		r, loadErr := s.LoadRun(runID)
		if loadErr != nil || r.Checkpoint == nil {
			break
		}
		interaction, loadErr := s.LoadInteraction(runID, r.Checkpoint.InteractionID)
		if loadErr != nil {
			break
		}

		answers, promptErr := PromptHumanAnswers(interaction)
		if promptErr != nil {
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

// newDefaultExecutor creates a ClawExecutor with the default delegate registry
// and event hooks wired to the store for observability.
//
// Returns an error if a hard precondition cannot be met: the working
// directory cannot be resolved (workspace gating relies on it), or any
// MCP server with a declared `Auth` block fails OAuth wiring (the run
// would otherwise dispatch unauthenticated requests and surface 401s
// at runtime, which is harder to diagnose).
func newDefaultExecutor(wf *ir.Workflow, vars map[string]string, s *store.RunStore, runID string, logger *iterlog.Logger, storeDir string) (*model.ClawExecutor, error) {
	reg := model.NewRegistry()
	backendReg := delegate.DefaultRegistry(logger)

	hooks := model.NewStoreEventHooks(s, runID, logger)
	lifecycle := model.NewDefaultLifecycleHooks(hooks)

	// Register the claw backend explicitly (API-based LLM path).
	clawBackend := model.NewClawBackend(reg, hooks, model.RetryPolicy{}, model.WithBackendLifecycleHooks(lifecycle))
	backendReg.Register(delegate.BackendClaw, clawBackend)

	toolReg := tool.NewRegistry()
	workspace, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("cli: resolve working dir for tool workspace: %w", err)
	}
	if err := tool.RegisterClawAll(toolReg, tool.ClawDefaults{Workspace: workspace}); err != nil {
		// Non-fatal: log and continue with whatever was registered.
		// A malformed registry is better than a hard run failure on
		// startup; downstream tool resolution will surface the gap.
		logger.Warn("RegisterClawAll: %v", err)
	}

	opts := []model.ClawExecutorOption{
		model.WithBackendRegistry(backendReg),
		model.WithEventHooks(hooks),
		model.WithToolRegistry(toolReg),
		model.WithLogger(logger),
		model.WithLifecycleHooks(lifecycle),
	}

	// Build tool policy from workflow-level and per-node ToolPolicy fields.
	if checker := buildToolChecker(wf); checker != nil {
		opts = append(opts, model.WithToolPolicy(checker))
	}
	if len(wf.ResolvedMCPServers) > 0 {
		catalog := make(map[string]*mcp.ServerConfig, len(wf.ResolvedMCPServers))
		hasAuth := false
		for name, server := range wf.ResolvedMCPServers {
			catalog[name] = &mcp.ServerConfig{
				Name:      server.Name,
				Transport: mcp.FromIRTransport(server.Transport),
				Command:   server.Command,
				Args:      append([]string(nil), server.Args...),
				URL:       server.URL,
				Headers:   server.Headers,
				Auth:      mcp.FromIRAuth(server.Auth),
			}
			if server.Auth != nil {
				hasAuth = true
			}
		}
		// Wire OAuth tokens for any server with Auth.Type == "oauth2".
		// Storage lives under the run's store dir so refresh tokens
		// survive across runs of the same project.
		//
		// When at least one server declares an Auth block, broker
		// init and PrepareAuth failures are fatal: continuing would
		// dispatch the run with AuthFunc == nil and surface as 401s
		// later, hiding the root cause from the operator.
		broker, brokerErr := mcp.NewOAuthBroker(storeDir)
		if brokerErr != nil {
			if hasAuth {
				return nil, fmt.Errorf("mcp: oauth broker init (required by catalog Auth): %w", brokerErr)
			}
			logger.Warn("mcp: oauth broker init: %v", brokerErr)
		} else if err := mcp.PrepareAuth(catalog, broker); err != nil {
			if hasAuth {
				return nil, fmt.Errorf("mcp: prepare oauth auth: %w", err)
			}
			logger.Warn("mcp: prepare oauth auth: %v", err)
		}
		var mcpOpts []mcp.ManagerOption
		mcpOpts = append(mcpOpts, mcp.WithLogger(logger))
		if cacheTTL := mcp.ResolveCacheTTL(); cacheTTL > 0 {
			mcpOpts = append(mcpOpts, mcp.WithToolCache(mcp.NewToolCache(storeDir, cacheTTL)))
		}
		mcpOpts = append(mcpOpts, mcp.WithFingerprintStore(mcp.NewFingerprintStore(storeDir)))
		opts = append(opts,
			model.WithMCPManager(mcp.NewManager(catalog, mcpOpts...)),
		)
	}

	executor := model.NewClawExecutor(reg, wf, opts...)

	if len(vars) > 0 {
		v := make(map[string]interface{}, len(vars))
		for k, val := range vars {
			v[k] = val
		}
		executor.SetVars(v)
	}

	return executor, nil
}

// mcpHealthCheck runs a pre-execution health check on active MCP servers if
// the executor supports it. Controlled by ITERION_MCP_HEALTHCHECK (default: on).
func mcpHealthCheck(ctx context.Context, executor runtime.NodeExecutor, servers []string) error {
	if len(servers) == 0 || !mcp.HealthCheckEnabled() {
		return nil
	}
	type healthChecker interface {
		MCPHealthCheck(ctx context.Context, servers []string) error
	}
	if hc, ok := executor.(healthChecker); ok {
		if err := hc.MCPHealthCheck(ctx, servers); err != nil {
			return fmt.Errorf("MCP health check failed: %w", err)
		}
	}
	return nil
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

// buildToolChecker constructs a tool.ToolChecker from the compiled workflow's
// ToolPolicy fields. Workflow-level ToolPolicy becomes the base; per-node
// ToolPolicy fields (on AgentNode and JudgeNode) become node overrides.
// Returns nil when no policy is configured (open).
func buildToolChecker(wf *ir.Workflow) tool.ToolChecker {
	var nodeOverrides map[string][]string

	for _, node := range wf.Nodes {
		var patterns []string
		switch n := node.(type) {
		case *ir.AgentNode:
			patterns = n.ToolPolicy
		case *ir.JudgeNode:
			patterns = n.ToolPolicy
		}
		if len(patterns) > 0 {
			if nodeOverrides == nil {
				nodeOverrides = make(map[string][]string)
			}
			nodeOverrides[node.NodeID()] = patterns
		}
	}

	if len(wf.ToolPolicy) == 0 && len(nodeOverrides) == 0 {
		return nil
	}

	return tool.BuildChecker(wf.ToolPolicy, nodeOverrides, nil)
}
