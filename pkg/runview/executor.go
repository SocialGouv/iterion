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
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// rewriteChainFromPlugins loads the plugin registry and builds the command-
// output rewriter chain from every enabled rewriter plugin (rtk by default,
// in stable name order). Returns nil when the registry can't be loaded or no
// rewriter plugin is enabled — compression then resolves to a no-op.
func rewriteChainFromPlugins(logger *iterlog.Logger) *rewrite.Chain {
	reg, err := plugin.Load()
	if err != nil {
		if logger != nil {
			logger.Warn("runview: load plugins for rewriter chain: %v — compression disabled", err)
		}
		return nil
	}
	// EnabledRewriterSpecs resolves each rewriter's {{config.<key>}} placeholders
	// from operator config before the chain bakes the specs in (the mcp and
	// lifecycle surfaces expand via ExpandContext at run time instead).
	return rewrite.NewChain(reg.EnabledRewriterSpecs())
}

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
	// SourceIssueID is the ticket that owns this run (dispatcher / pipeline
	// launch). Empty for ad-hoc runs. Used to auto-stamp parent_id when the
	// bot creates child tickets via board.create.
	SourceIssueID string
	// ExtraHooks are merged into the store-backed event hooks. Pass
	// the prometheus exporter's hooks here (cli does this); the HTTP
	// service can pass nothing or a future broker-side hook chain.
	ExtraHooks []model.EventHooks
	// EventObservers (ADR-046) fire on every event the backend-hook
	// layer persists — the stall-heartbeat seam for high-frequency tool
	// events that bypass the runtime engine's WithEventObserver. The
	// dispatcher-via-service path sets these so it can observe the same
	// store-level event stream the direct path's heartbeatStore saw,
	// WITHOUT wrapping the store (which would shadow its optional
	// capabilities — PlanWriter / RunFilesStore / … — against the probes
	// below). Engine-level events reach the same observers through
	// runtime.WithEventObserver; the two event sets are disjoint.
	EventObservers []func(store.Event)
	// Inbox, when non-nil, wires the operator chatbox plumbing into
	// the claw backend so queued messages are delivered between
	// agent-loop iterations. Nil disables the inbox (CLI mode +
	// runs that opted out).
	Inbox model.InboxBinder
	// AsyncAsk, when non-nil, backs the ask_user_async / await_answers
	// tools of interaction: async nodes (ADR-081). Nil = the tools
	// error explicitly if a node resolves them.
	AsyncAsk model.AsyncAskBinder
	// Backend, when non-empty, takes precedence over the workflow's
	// `default_backend:` for this run only. Node-level explicit
	// `backend:` still wins (it's the most specific level in the
	// resolution chain). Used by the studio launch UI to A/B a
	// workflow against different backends without editing the .bot.
	Backend string
	// ModelOverrides are launch-time per-node/-group backend+model+provider
	// overrides (studio Launch dropdowns, CLI --model/--backend selector=spec).
	// Unlike Backend (a run-level default below node DSL), these target nodes
	// explicitly and win over the node's DSL backend:/model:, so a run can
	// re-target the bot per node-group without editing the .bot. Empty = no-op.
	ModelOverrides model.ModelOverrides
	// RunFallback is the operator's single run-level fallback route
	// (studio Launch row / CLI --fallback). Zero value = none.
	//
	// BuildExecutor materialises it onto the compiled workflow's eligible
	// agent nodes rather than handing it to the executor privately, so it
	// passes the same safety screen as an authored route and is visible
	// to the pre-run analyses the engine runs on the SAME *ir.Workflow.
	RunFallback ir.Fallback
	// BotID is the stable bundle/bot identifier used to qualify
	// structured visibility=bot memory. Empty falls back to Workflow.Name.
	BotID string
	// MemoryStore overrides the workspace-memory backend. nil → the
	// local filesystem store. Cloud runners pass the Mongo store so
	// shared knowledge persists in the tenant's document store.
	MemoryStore knowledge.MemoryStore
	// BoardRegister mints a per-node board MCP run token (C082, server
	// path): it registers the node's board caps with the server's token
	// registry and returns the token. nil (CLI) leaves sandboxed
	// board-emit disabled.
	BoardRegister func(caps []string, sourceIssueID string) string
	// Compress is the run-level command-output-compression override ("",
	// "on", "ultra", "off"), forwarded to the executor as the highest-priority
	// input to rewrite.Resolve (above node/workflow DSL and ITERION_COMPRESS).
	Compress string

	// AutoMemory is the run-level auto-memory (MEMORY.md) override ("", "on",
	// "off"), highest-priority input to automemory.Resolve (above node/workflow
	// DSL and ITERION_AUTO_MEMORY). See docs/memory-and-knowledge.md.
	AutoMemory string

	// UsageGuard enforces the operator's subscription usage cap
	// (pkg/usagecap) for this run. The cloud runner injects one backed by
	// the shared Mongo store, so every pod sees what the others learned;
	// nil makes BuildExecutor resolve the machine-wide policy from the
	// environment onto a process-local store, which is exactly right for
	// a CLI run and inert when no cap is configured.
	UsageGuard *usagecap.Guard

	// UsageCapSource, when non-nil (and UsageGuard is nil), builds the
	// run's guard over a LIVE policy source — the DB-backed runtime
	// settings resolver — instead of the frozen env policy, so a cap
	// changed through the admin API reaches in-process runs within the
	// source's TTL. The service threads its own source here.
	UsageCapSource usagecap.PolicySource

	// Permission is the run-level tool-permission-gate mode override
	// ("", "off", "ask", "deny"), highest-priority input to the gate's
	// mode precedence (above node/workflow DSL and ITERION_PERMISSION).
	// PermissionAllow/Ask/Deny are run-level rules, additive to the
	// workflow lists. See docs/permissions.md.
	Permission      string
	PermissionAllow []string
	PermissionAsk   []string
	PermissionDeny  []string

	// LocalSecrets + LocalSealer are the local (desktop / CLI / non-cloud
	// studio) secret store and its AES-GCM sealer. When both are set and the
	// ctx does not already carry resolved Credentials (the cloud runner path),
	// BuildExecutor resolves the workflow's declared `secrets:` names from the
	// store and stamps them into ctx via secrets.WithCredentials — the
	// in-process equivalent of the cloud runner's injectCredentials. Nil on
	// the cloud path (credentials arrive pre-resolved in ctx).
	LocalSecrets secrets.GenericSecretStore
	LocalSealer  secrets.Sealer
}

// BuildExecutor wires up the default ClawExecutor: registry, default
// delegate registry, store-backed event hooks (chained with
// spec.ExtraHooks), tool registry with claw built-ins, MCP catalog
// (when the workflow declares servers), and the per-run plan-mode
// state directory. Used by both the CLI and the HTTP service so the
// two transports stay aligned on tool policies, MCP auth, and
// executor lifecycle.
// declaredSecretNames returns the names of the workflow's declared
// `secrets:` block — the only names a `{{secrets.X}}` reference can legally
// use (compile-checked). Nil-safe.
// StampLocalCredentials resolves the workflow's declared `secrets:` names
// from the local sealed store and returns a context carrying them — the
// in-process equivalent of the cloud runner's injectCredentials. Returns
// ctx unchanged when there is no local store, or when credentials are
// already present (the cloud path's are never overwritten).
//
// It is EXPORTED because the context it returns has to reach further than
// the executor: the engine mounts declared file secrets into the sandbox
// at run start, reading them from the ctx passed to Run. A caller that
// stamps only the executor's own context ships a sandbox with no secret
// files at all — an optional one is skipped silently, and the bot simply
// never finds its token (observed: a docs bot's PR tail reporting "no
// forge_token secret" on a host whose store held exactly that secret).
func StampLocalCredentials(
	ctx context.Context,
	wf *ir.Workflow,
	localSecrets secrets.GenericSecretStore,
	sealer secrets.Sealer,
	logger *iterlog.Logger,
) (context.Context, error) {
	if localSecrets == nil || sealer == nil {
		return ctx, nil
	}
	if _, already := secrets.CredentialsFromContext(ctx); already {
		return ctx, nil
	}
	creds, err := secrets.ResolveLocalCredentials(ctx, localSecrets, sealer, declaredSecretNames(wf), logger)
	if err != nil {
		return nil, fmt.Errorf("runview: resolve local secrets: %w", err)
	}
	// Required-secret launch gate (parity with the cloud publisher): a
	// non-`optional` declared secret with no inline value MUST resolve to
	// a non-empty value. If it resolves to nothing, fail the launch here
	// rather than running the bot with the credential unset.
	haveValue := make(map[string]bool, len(creds.Generic))
	for name, v := range creds.Generic {
		if v != "" {
			haveValue[name] = true
		}
	}
	if missing := secrets.UnresolvedRequired(requiredSecretNames(wf), haveValue); len(missing) > 0 {
		return nil, secrets.RequiredSecretsError(missing, "this workspace")
	}
	return secrets.WithCredentials(ctx, creds), nil
}

func declaredSecretNames(wf *ir.Workflow) []string {
	if wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(wf.Secrets))
	for name := range wf.Secrets {
		names = append(names, name)
	}
	return names
}

// requiredSecretNames returns the declared secret names that MUST resolve to a
// non-empty value for the run to proceed: non-`optional` and with no inline
// literal `value:` (a literal is materialised at exec, never resolved from the
// store). These feed the launch-time required-secret gate — mirroring the cloud
// publisher's requiredSecretNamesForWorkflow so a required secret that resolves
// to nothing fails identically on either launch path. Nil-safe.
func requiredSecretNames(wf *ir.Workflow) []string {
	if wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(wf.Secrets))
	for name, s := range wf.Secrets {
		if s == nil || s.Optional || strings.TrimSpace(s.Value) != "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

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
	if err := ValidateModelOverridePermissions(spec.Workflow, spec.ModelOverrides, spec.Permission); err != nil {
		return nil, err
	}
	// Materialise the operator's launch-time fallback route onto the
	// compiled workflow BEFORE anything reads it. The engine is
	// constructed from the same *ir.Workflow, so writing it here is what
	// lets the sandbox bind-mount, the parallel-branch admission and the
	// fan_out_each guard all see the route — and what subjects it to the
	// same refusals the compiler applies to an authored one.
	//
	// A refused node keeps its primary and the operator is told; the
	// route is never silently taken.
	for _, refusal := range ir.ApplyRunFallback(spec.Workflow, spec.RunFallback) {
		if spec.Logger != nil {
			spec.Logger.Warn("run-level fallback not applied — %s", refusal)
		}
	}
	if spec.Logger == nil {
		spec.Logger = iterlog.NewFromEnv(os.Stderr)
	}

	reg := model.NewRegistry()
	backendReg := delegate.DefaultRegistry(spec.Logger)

	ctx := spec.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Local (desktop / CLI / non-cloud studio) secret injection: resolve the
	// workflow's declared `secrets:` names from the local sealed store and
	// stamp them into ctx — the in-process equivalent of the cloud runner's
	// injectCredentials. Skipped when the cloud path already put Credentials
	// in ctx (never overwrite pre-resolved cloud credentials).
	stamped, err := StampLocalCredentials(ctx, spec.Workflow, spec.LocalSecrets, spec.LocalSealer, spec.Logger)
	if err != nil {
		return nil, err
	}
	ctx = stamped
	// Build the per-run secret guard (Layer 0/1/2) from the resolved
	// credentials in ctx + sensitive host env + declared workflow
	// secrets, then thread it through the event hooks so every sink is
	// scrubbed before persistence.
	guard := model.BuildSecretGuard(ctx, spec.Workflow, spec.Vars)
	hooks := model.NewStoreEventHooks(ctx, spec.Store, spec.RunID, spec.Logger, guard, spec.EventObservers...)
	for _, extra := range spec.ExtraHooks {
		hooks = model.ChainHooks(hooks, extra)
	}

	lifecycle := model.NewDefaultLifecycleHooks(hooks)

	clawOpts := []model.ClawBackendOption{model.WithBackendLifecycleHooks(lifecycle)}
	if spec.Inbox != nil {
		clawOpts = append(clawOpts, model.WithInbox(spec.Inbox))
	}
	if spec.MemoryStore != nil {
		clawOpts = append(clawOpts, model.WithMemoryStore(spec.MemoryStore))
	}
	// LAYER-1 in-executor transient-retry budget (env-configurable via
	// ITERION_NODE_MAX_TRANSIENT_RETRIES / ITERION_NODE_MAX_RETRIES; defaults
	// preserved when unset). The same policy is handed to both the claw
	// backend's own retry loop and the executor's delegate-retry loop below so
	// a rate-limit / network / idle blip is ridden out before it surfaces as a
	// run-level failed_resumable.
	retryPolicy := model.RetryPolicyFromEnv()
	clawOpts = append(clawOpts, model.WithClawLogger(spec.Logger))
	clawBackend := model.NewClawBackend(reg, hooks, retryPolicy, clawOpts...)
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

	// Dispatcher sub-store: capability-gated tools (the board MCP server) read
	// and write under <run-root>/dispatcher/. Translate the run-level store dir
	// every caller passes into the dispatcher-specific path the model layer
	// forwards to backends as task.StoreDir. Without this, __mcp-board falls
	// back to <cwd>/.iterion/dispatcher and any --store-dir isolation is lost.
	dispatcherStoreDir := ""
	if spec.StoreDir != "" {
		dispatcherStoreDir = filepath.Join(spec.StoreDir, "dispatcher")
	}

	opts := []model.ClawExecutorOption{
		model.WithBackendRegistry(backendReg),
		model.WithRetryPolicy(retryPolicy),
		model.WithEventHooks(hooks),
		model.WithToolRegistry(toolReg),
		model.WithLogger(spec.Logger),
		model.WithLifecycleHooks(lifecycle),
		model.WithStoreDir(dispatcherStoreDir),
		model.WithSecretGuard(guard),
		model.WithCompressOverride(spec.Compress),
		model.WithAutoMemoryOverride(spec.AutoMemory),
		// The auto-memory mirror persists through the SAME store as the
		// `memory:` block, so a cloud runner's Mongo store is what carries
		// MEMORY.md past the pod's ephemeral disk. nil keeps the local
		// filesystem default.
		model.WithAutoMemoryStore(spec.MemoryStore),
		model.WithRewriteChain(rewriteChainFromPlugins(spec.Logger)),
		model.WithPermissionOverride(spec.Permission),
		model.WithPermissionRules(spec.PermissionAllow, spec.PermissionAsk, spec.PermissionDeny),
	}
	usageGuard, err := resolveUsageGuard(spec)
	if err != nil {
		return nil, err
	}
	if usageGuard != nil {
		opts = append(opts, model.WithUsageGuard(usageGuard))
	}
	if sid := resolveSourceIssueID(spec); sid != "" {
		opts = append(opts, model.WithSourceIssueID(sid))
	}
	if spec.BoardRegister != nil {
		opts = append(opts, model.WithBoardRegister(spec.BoardRegister))
	}
	if spec.Inbox != nil {
		opts = append(opts, model.WithExecutorInbox(spec.Inbox))
	}
	if spec.AsyncAsk != nil {
		opts = append(opts, model.WithExecutorAsyncAsk(spec.AsyncAsk))
	}
	if spec.Backend != "" {
		opts = append(opts, model.WithDefaultBackend(spec.Backend))
	}
	if !spec.ModelOverrides.Empty() {
		opts = append(opts, model.WithModelOverrides(spec.ModelOverrides))
	}

	if spec.BotID != "" {
		opts = append(opts, model.WithBotID(spec.BotID))
	}
	// Export the run's artifact_files scratch area to HOST tool nodes as
	// ITERION_ARTIFACT_FILES_DIR. Sandboxed runs get the variable from the
	// container env (pkg/runtime/sandbox.go bind-mounts the same dir); this
	// closes the host/sandbox parity gap where a bot writing its outputs
	// there only worked sandboxed. Best-effort: stores without the files
	// area (or a mkdir failure) just leave the variable unset, as before.
	if fs, ok := spec.Store.(store.RunFilesStore); ok {
		if dir, derr := fs.EnsureRunFilesDir(ctx, spec.RunID); derr == nil && dir != "" {
			opts = append(opts, model.WithArtifactFilesDir(dir))
		}
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

	clawDefaults := tool.ClawDefaults{
		Workspace:        workspace,
		IncludeWebSearch: tool.ResolveWebSearchEnabled(),
	}
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

	// Register the native-board MCP tools so claw nodes that declare
	// board capabilities can actually call mcp.iterion_board.* (and
	// the claude_code-FQN alias mcp__iterion_board__*). Without this
	// the registry is empty for board tools and Resolve correctly
	// returns "unknown tool" for any board call. We pass all known
	// board capabilities so every tool registers; per-node access is
	// gated downstream by the workflow's checkNodeToolAccess (which
	// reads the node's `capabilities:` list).
	if dispatcherStoreDir != "" {
		ns, err := native.NewStore(dispatcherStoreDir)
		if err != nil {
			spec.Logger.Warn("runview: open native board store at %s: %v — board MCP tools disabled", dispatcherStoreDir, err)
		} else {
			// The store owns an fsnotify watcher goroutine + inotify fd; hand
			// it to the executor so Close() releases it. Otherwise every
			// BuildExecutor call leaks one — acute under parallel subbot
			// fan-out (one executor per child).
			opts = append(opts, model.WithExtraClosers(ns))
			boardCfg := &tool.BoardConfig{
				Store:        ns,
				Capabilities: boardops.AllCapabilities(),
				// The owning ticket travels with the tools so board.create
				// auto-stamps parent_id / spawned_from on spawned children.
				SourceIssueID: resolveSourceIssueID(spec),
			}
			if err := tool.RegisterClawBoardTools(toolReg, boardCfg); err != nil {
				spec.Logger.Warn("runview: RegisterClawBoardTools: %v", err)
			}
		}
	}

	// Register the native-board WATCH tools so claw nodes that declare
	// watch.subscribe / watch.unsubscribe can opt their run into the
	// runtime watch fan-out (MVP3b). All known watch caps are registered;
	// per-node access is gated downstream by checkNodeToolAccess via the
	// node's `capabilities:` list, mirroring the board tools. The claw
	// executor is per-run, so spec.RunID binds every subscription to the
	// correct run. spec.Store is the RunStore (it carries the
	// AddWatchedIssues/RemoveWatchedIssues mutators); the type-assertion
	// degrades to a no-op for stores that don't implement watch (e.g. a
	// bare event emitter).
	if ws, ok := spec.Store.(tool.WatchStore); ok {
		watchCfg := &tool.WatchConfig{
			Store:        ws,
			RunID:        spec.RunID,
			Capabilities: []string{ir.CapWatchSubscribe, ir.CapWatchUnsubscribe},
		}
		if err := tool.RegisterClawWatchTools(toolReg, watchCfg); err != nil {
			spec.Logger.Warn("runview: RegisterClawWatchTools: %v", err)
		}
	}

	executor := model.NewClawExecutor(reg, spec.Workflow, opts...)

	if len(spec.Vars) > 0 {
		v := make(map[string]any, len(spec.Vars))
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
			// Env is already fully resolved at catalog-build time (plugin
			// {{config.*}} placeholders expanded by loadPluginServers) — copy
			// verbatim, no os.ExpandEnv (a secret value may legitimately
			// contain a `$`).
			Env:  server.Env,
			Auth: mcp.FromIRAuth(server.Auth),
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

// resolveSourceIssueID returns the ticket that owns this run: explicit
// ExecutorSpec.SourceIssueID first, else LoadRun(Source.IssueID) when the
// store can load the record. Empty for ad-hoc launches.
func resolveSourceIssueID(spec ExecutorSpec) string {
	if id := strings.TrimSpace(spec.SourceIssueID); id != "" {
		return id
	}
	if spec.RunID == "" {
		return ""
	}
	type runLoader interface {
		LoadRun(ctx context.Context, id string) (*store.Run, error)
	}
	loader, ok := spec.Store.(runLoader)
	if !ok {
		return ""
	}
	ctx := spec.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := loader.LoadRun(ctx, spec.RunID)
	if err != nil || r == nil || r.Source == nil {
		return ""
	}
	return strings.TrimSpace(r.Source.IssueID)
}
