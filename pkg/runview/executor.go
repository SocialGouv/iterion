package runview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
	"github.com/SocialGouv/claw-code-go/pkg/permissions"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// ExecutorSpec carries the inputs required to construct a default
// ClawExecutor. Splitting the args into a struct keeps cli/run.go and
// the HTTP service layer in sync as new options accrue (compactor
// callbacks, recipe overrides, etc.).
type ExecutorSpec struct {
	// Ctx is captured by the store-backed event hooks for AppendEvent
	// calls during execution. Filesystem stores ignore it; cloud
	// (Mongo) stores honor it for cancellation/timeout.
	Ctx      context.Context
	Workflow *ir.Workflow
	Vars     map[string]string
	Store    model.EventEmitter // typically *store.RunStore
	RunID    string
	Logger   *iterlog.Logger
	StoreDir string
	// ExtraHooks are merged into the store-backed event hooks. Pass
	// the prometheus exporter's hooks here (cli does this); the HTTP
	// service can pass nothing or a future broker-side hook chain.
	ExtraHooks []model.EventHooks
}

// BuildExecutor wires up the default ClawExecutor: registry, default
// delegate registry, store-backed event hooks (chained with
// spec.ExtraHooks), tool registry with claw built-ins, MCP catalog
// (when the workflow declares servers), and the per-run plan-mode
// state directory. Used by both the CLI and the HTTP service so the
// two transports stay aligned on tool policies, MCP auth, and
// executor lifecycle.
func BuildExecutor(spec ExecutorSpec) (*model.ClawExecutor, error) {
	if spec.Workflow == nil {
		return nil, fmt.Errorf("runview: workflow is required")
	}
	if spec.Store == nil {
		return nil, fmt.Errorf("runview: store is required")
	}
	if spec.RunID == "" {
		return nil, fmt.Errorf("runview: run ID is required")
	}
	if spec.Logger == nil {
		spec.Logger = iterlog.New(iterlog.LevelInfo, os.Stderr)
	}

	reg := model.NewRegistry()
	backendReg := delegate.DefaultRegistry(spec.Logger)

	ctx := spec.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	hooks := model.NewStoreEventHooks(ctx, spec.Store, spec.RunID, spec.Logger)
	for _, extra := range spec.ExtraHooks {
		hooks = model.ChainHooks(hooks, extra)
	}

	lifecycle := model.NewDefaultLifecycleHooks(hooks)

	clawBackend := model.NewClawBackend(reg, hooks, model.RetryPolicy{}, model.WithBackendLifecycleHooks(lifecycle))
	backendReg.Register(delegate.BackendClaw, clawBackend)

	toolReg := tool.NewRegistry()

	workspace, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("runview: resolve working dir for tool workspace: %w", err)
	}

	planActive := false
	planDir := filepath.Join(spec.StoreDir, "plan-mode")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		spec.Logger.Warn("runview: prepare plan_mode dir: %v — plan_mode tools disabled", err)
		planDir = ""
	}

	opts := []model.ClawExecutorOption{
		model.WithBackendRegistry(backendReg),
		model.WithEventHooks(hooks),
		model.WithToolRegistry(toolReg),
		model.WithLogger(spec.Logger),
		model.WithLifecycleHooks(lifecycle),
	}

	checker := buildToolChecker(spec.Workflow)
	classifier, classErr := newLLMClassifierFromEnv(reg, spec.Logger)
	if classErr != nil {
		return nil, fmt.Errorf("runview: build LLM classifier: %w", classErr)
	}
	if classifier != nil {
		checker = &tool.ClassifierChecker{Classifier: classifier, Base: checker, Logger: spec.Logger}
	}
	if checker != nil {
		opts = append(opts, model.WithToolPolicy(checker))
	}

	mcpManager, oauthBroker, mcpErr := buildMCPManager(spec.Workflow, spec.StoreDir, spec.Logger)
	if mcpErr != nil {
		return nil, mcpErr
	}
	if mcpManager != nil {
		opts = append(opts, model.WithMCPManager(mcpManager))
	}

	clawDefaults := tool.ClawDefaults{Workspace: workspace}
	if planDir != "" {
		clawDefaults.PlanMode = &clawtools.PlanModeState{Active: &planActive, Dir: planDir}
	}
	if mcpManager != nil {
		clawDefaults.MCPProvider = mcpManager.ClawProvider(oauthBroker)
	}
	// Privacy tools — pure-Go detector, always available when a
	// store directory is wired. No external process or model
	// download; activating the pair surfaces privacy_filter and
	// privacy_unfilter to every workflow that allows them via
	// tool_policy.
	if spec.StoreDir != "" {
		clawDefaults.Privacy = &privacy.Config{
			StoreDir:     spec.StoreDir,
			Detector:     detector.New(),
			RunIDFromCtx: model.RunIDFromContext,
		}
	}
	if err := tool.RegisterClawAll(toolReg, clawDefaults); err != nil {
		spec.Logger.Warn("runview: RegisterClawAll: %v", err)
	}

	executor := model.NewClawExecutor(reg, spec.Workflow, opts...)

	if len(spec.Vars) > 0 {
		v := make(map[string]interface{}, len(spec.Vars))
		for k, val := range spec.Vars {
			v[k] = val
		}
		executor.SetVars(v)
	}

	return executor, nil
}

// MCPHealthCheck runs the executor's optional MCP health-check
// implementation. The `iterion run` and `iterion resume` paths invoke
// this just before eng.Run / eng.Resume so a misconfigured catalog
// surfaces an error before any node is dispatched.
func MCPHealthCheck(ctx context.Context, executor runtime.NodeExecutor, servers []string) error {
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

// buildMCPManager wires the OAuth broker, header substitution, and
// catalog plumbing for any MCP servers the workflow resolved.
//
// When at least one server declares an Auth block, broker init and
// PrepareAuth failures are fatal — continuing would dispatch the run
// with AuthFunc == nil and surface as 401s later, hiding the root
// cause from the operator.
func buildMCPManager(wf *ir.Workflow, storeDir string, logger *iterlog.Logger) (*mcp.Manager, *mcp.OAuthBroker, error) {
	if len(wf.ResolvedMCPServers) == 0 {
		return nil, nil, nil
	}
	catalog := make(map[string]*mcp.ServerConfig, len(wf.ResolvedMCPServers))
	hasAuth := false
	for name, server := range wf.ResolvedMCPServers {
		expandedArgs := make([]string, len(server.Args))
		for i, a := range server.Args {
			expandedArgs[i] = os.ExpandEnv(a)
		}
		catalog[name] = &mcp.ServerConfig{
			Name:      server.Name,
			Transport: mcp.FromIRTransport(server.Transport),
			Command:   os.ExpandEnv(server.Command),
			Args:      expandedArgs,
			URL:       os.ExpandEnv(server.URL),
			Headers:   server.Headers,
			Auth:      mcp.FromIRAuth(server.Auth),
		}
		if server.Auth != nil {
			hasAuth = true
		}
	}

	broker, brokerErr := mcp.NewOAuthBroker(storeDir)
	if brokerErr != nil {
		if hasAuth {
			return nil, nil, fmt.Errorf("mcp: oauth broker init (required by catalog Auth): %w", brokerErr)
		}
		logger.Warn("mcp: oauth broker init: %v", brokerErr)
	} else if err := mcp.PrepareAuth(catalog, broker); err != nil {
		if hasAuth {
			return nil, nil, fmt.Errorf("mcp: prepare oauth auth: %w", err)
		}
		logger.Warn("mcp: prepare oauth auth: %v", err)
	}

	mcpOpts := []mcp.ManagerOption{mcp.WithLogger(logger)}
	if cacheTTL := mcp.ResolveCacheTTL(); cacheTTL > 0 {
		mcpOpts = append(mcpOpts, mcp.WithToolCache(mcp.NewToolCache(storeDir, cacheTTL)))
	}
	mcpOpts = append(mcpOpts, mcp.WithFingerprintStore(mcp.NewFingerprintStore(storeDir)))
	manager := mcp.NewManager(catalog, mcpOpts...)
	return manager, broker, nil
}

// buildToolChecker constructs a tool.ToolChecker from the workflow's
// ToolPolicy fields. Workflow-level patterns become the base; per-node
// patterns (on AgentNode and JudgeNode) become overrides keyed by node
// ID. Returns nil when no policy is configured (open).
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

// newLLMClassifierFromEnv builds an LLMClassifier when the
// ITERION_LLM_CLASSIFIER_MODEL env var is set (e.g.
// "anthropic/claude-haiku-4-5"). The classifier is chained over the
// default RuleClassifier and uses a 30-minute TTL cache.
//
// Returns (nil, nil) when the env var is empty.
func newLLMClassifierFromEnv(reg *model.Registry, logger *iterlog.Logger) (permissions.Classifier, error) {
	spec := strings.TrimSpace(os.Getenv("ITERION_LLM_CLASSIFIER_MODEL"))
	if spec == "" {
		return nil, nil
	}
	client, err := reg.Resolve(spec)
	if err != nil {
		return nil, fmt.Errorf("resolve classifier model %q: %w", spec, err)
	}
	logger.Info("llm-classifier: enabled (model=%s)", spec)
	_, modelID, _ := model.ParseModelSpec(spec)
	return &permissions.LLMClassifier{
		Client:    client,
		Model:     modelID,
		Fallback:  permissions.NewRuleClassifier(),
		Cache:     permissions.NewClassifierCache(30 * time.Minute),
		MaxTokens: 64,
	}, nil
}
