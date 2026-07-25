// Package server provides an HTTP API for the iterion studio.
// It wraps the parser, compiler, and unparser to provide endpoints
// for parsing workflow files, validating workflows, and generating DSL text.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	"github.com/SocialGouv/iterion/pkg/usernotify/webpush"
	"github.com/SocialGouv/iterion/pkg/valkey"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// Server is the studio HTTP server.
type Server struct {
	// stateMu guards the hot-swappable fields used by ProjectSwitcher
	// (cfg.WorkDir, cfg.StoreDir, runs, watcher, localSecrets, statsCache).
	// Acquired write-side
	// only during a project switch — handlers that read these fields
	// during a swap may briefly see one half of the swap, which is
	// acceptable: the SPA reset on `project_switched` invalidates any
	// inflight request data before it's surfaced.
	stateMu sync.RWMutex
	// currentProjectID is the id of the registry entry matching
	// cfg.WorkDir. Surfaced by /api/server/info (polled by the SPA);
	// caching it here avoids a disk read on every poll.
	currentProjectID  string
	cfg               Config
	logger            *iterlog.Logger
	mux               *recordingMux // records routes → GET /api/openapi.json
	handler           http.Handler  // mux wrapped with auth middleware
	server            *http.Server
	hub               *Hub
	watcher           *Watcher
	runs              *runview.Service         // run console service; nil disables /api/runs endpoints
	watchCoord        *watchCoordinator        // MVP3b issue-state fan-out; nil when no native tracker or events tail unavailable
	triggerCoord      *TriggerCoordinator      // event-driven trigger spine; nil when no TriggerStore/native tracker
	cloudTriggerCoord *CloudTriggerCoordinator // cloud (mongo board) trigger spine; nil outside cloud mode
	// userNotify + pushSink are the user-notification stack (web push on
	// human-input pauses and run outcomes); nil when the feature is off
	// (no subscription store / no VAPID keys). userNotifyCancel detaches
	// the bus subscription on Close.
	userNotify       *usernotify.Dispatcher
	pushSink         *webpush.Sink
	userNotifyCancel func()
	// statsCache memoizes the per-run events.jsonl cost scan behind
	// /api/v1/runs/stats (terminal runs only — see runs_stats_cache.go).
	// Cleared on project switch. Non-nil after New.
	statsCache *runStatsCache
	// locCache memoizes the per-run three-dot LOC diff behind the run
	// header (see runs_loc.go). Non-nil after New.
	locCache *runLOCCache

	// admissionSkipWarned dedupes the pipeline-admission "unresolvable
	// bot" warning per (ticket, bot) so a stranded Ready ticket logs
	// once instead of every 2s tick. Guarded by admissionSkipMu.
	admissionSkipMu     sync.Mutex
	admissionSkipWarned map[string]string

	// finalOutputMemo caches finished runs' resolved board output. A finished
	// run is terminal, so its final_answer/latest-artifact output never
	// changes — computing it once per run (instead of on every 3s poll, each
	// up to pipelineArtifactProbeCap artifact loads per DONE card) keeps the
	// pipeline-board poll cheap on a full 500-card board (PR #193 M1).
	finalOutputMemo finalOutputCache

	authSvc        *auth.Service
	authLimiter    authRateLimiterBackend
	signer         *auth.JWTSigner
	oidcRegistry   *oidc.Registry
	oidcStates     oidc.StateStore
	desktopTickets desktopsso.Store
	wsTickets      wsticket.Store
	apiKeys        secrets.ApiKeyStore
	genericSecrets secrets.GenericSecretStore
	// localSecrets is the concrete layered file store when running in local
	// mode (set only when genericSecrets is a *LayeredGenericSecretStore). The
	// /api/local/secrets handlers need its scope-aware ops (ForScope, Project,
	// Global, ListScoped), so keeping the concrete type here avoids a
	// type-assertion in every handler. Nil in cloud mode.
	localSecrets      *secrets.LayeredGenericSecretStore
	runSecrets        secrets.RunSecretsStore
	sealer            secrets.Sealer
	oauthStore        secrets.OAuthStore
	oauthPending      secrets.OAuthPendingStore
	webhookConfigs    webhooks.ConfigStore
	webhookDeliveries webhooks.DeliveryStore
	webhookCounter    webhooks.Counter
	orgUsage          orgusage.Counter
	orgDefaults       OrgLimitDefaults
	auditStore        audit.Store
	pats              pat.Store
	queue             *natsq.Conn
	botBindings       secrets.BotSecretBindingStore
	// pluginSources holds team-scoped, git-hosted org-private plugins. Durable
	// (unlike a plugin installed into this pod's ephemeral iterion home), so a
	// restart re-derives instead of silently dropping the plugin from runs.
	pluginSources pluginsource.Store
	// botSources holds team-authored bot bundles (pkg/botsource) — the writable,
	// tenant-scoped counterpart to the read-only baked catalog. Non-nil enables
	// cloud bot editing (/api/teams/:id/bot-sources + bot_editing_enabled).
	botSources     botsource.Store
	configShares   configshare.Store
	configShareSvc *configshare.Service
	// configShareFC overrides forge-client resolution in tests (nil in prod →
	// shareFileClient resolves the team forge_token + builds a GitHub client).
	configShareFC     func(context.Context, *configshare.Share) (forge.FileClient, error)
	forgeConnections  forge.ConnectionStore
	forgeIntegrations forge.RepoIntegrationStore
	// authorTrustG is the lazily-built TTL cache behind the issue
	// author-trust gate (webhooks + forge→board sync); use authorTrustGate().
	authorTrustG      *authorTrust
	authorTrustOnce   sync.Once
	forgeOrchestrator *forge.Orchestrator
	forgeStates       forgeStateBackend
	forgeOAuthApps    forge.OAuthAppStore
	forgeGitHubApp    ForgeGitHubAppConfig
	orgSSO            orgsso.Store
	orgDomains        orgsso.DomainStore
	orgDomainTXT      orgsso.TXTLookupFunc
	memStore          knowledge.MemoryStore
	// webhookLaunchBot overrides the inbound-webhook launch path (test
	// seam). nil → realWebhookLaunchBot (resolve bot source + s.runs.Launch).
	webhookLaunchBot func(ctx context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, keyOverrides, secretOverrides map[string]string) (string, error)
	// scheduleClock overrides the wall clock the schedules CRUD stamps on
	// CreatedAt / UpdatedAt / NextFireAt (test seam — tests need a
	// deterministic instant to assert NextFire jumps to the expected slot).
	// nil → time.Now().UTC().
	scheduleClock func() time.Time
	// webhookNoteGate overrides the conversational replier gate (forge
	// token + loop-guard + reply-in-thread detection + allowlist/role authz
	// — test seam, the real gate calls the GitLab API). nil →
	// realWebhookNoteGate. Returns (authorized, replyInThread, threadContext,
	// reason, err): replyInThread marks a plain reply in a Revi thread (no
	// /revi command); threadContext is the discussion transcript the converse
	// bot receives as {{vars.thread_context}} ("" when not fetched).
	webhookNoteGate func(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, botID string) (authorized, replyInThread bool, threadContext, reason string, err error)
	// webhookCommandGate overrides the generic slash-command replier gate
	// (forge token + loop-guard + allowlist/role authz, honouring the route's
	// per-command MinReplierRole — test seam). nil → realWebhookCommandGate.
	// Distinct from webhookNoteGate: no reply-in-thread/thread-context logic
	// (that is the Revi-converse specialisation); a generic command authorises
	// the replier and launches.
	webhookCommandGate func(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, route webhooks.CommandRoute) (authorized bool, reason string, err error)
	// webhookPRForgeCommandGate overrides the GitHub/Forgejo issue_comment
	// command replier gate (forge token + loop-guard + allowlist/role authz —
	// test seam). nil → realWebhookPRForgeCommandGate.
	webhookPRForgeCommandGate func(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, route webhooks.CommandRoute) (authorized bool, reason string, err error)
	// webhookPRForgePRResolver overrides the PR head/base resolution for a
	// PR-surface command comment (the issue_comment payload carries no head
	// branch — test seam). nil → realWebhookPRForgePRResolver.
	webhookPRForgePRResolver func(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, route webhooks.CommandRoute) (forge.PullRef, error)
	// webhookIterionBotAuthor overrides the "is this PR/MR authored by iterion's
	// own forge bot" check that keeps the PR-open auto-review lane from launching
	// Revi on another iterion bot's PR (test seam — the real impl resolves the
	// provisioned forge Connection). nil → realIterionBotAuthor.
	webhookIterionBotAuthor func(ctx context.Context, cfg webhooks.Config, login string) bool
	// webhookPriorReview overrides the lookup of the most recent review-pr (Revi)
	// run for a PR, whose findings seed a `/billy` invocation (test seam). nil →
	// realWebhookPriorReview. Returns "" when no prior review is found (best-effort).
	webhookPriorReview func(ctx context.Context, cfg webhooks.Config, prURL, projectPath string, prNumber int) string
	httpClient         *http.Client

	// forgeHTTP is the SSRF-guarded client for outbound forge calls, built
	// once (its strict flag is startup-fixed) so connection pooling is
	// preserved across forge operations. Use forgeHTTPClient().
	forgeHTTP     *http.Client
	forgeHTTPOnce sync.Once

	// detector is the cached LLM credential detector backing
	// /api/backends/detect. Lazily constructed on first request.
	detector     *detect.CachedDetector
	detectorOnce sync.Once
	// OnForceRefresh runs (if non-nil) before the cache is invalidated on
	// a `?force=1` call. The iterion-desktop binary registers a hook here
	// that re-sources ~/.iterion/env so commenting out a key in that file
	// and clicking Refresh actually clears the value — without it the
	// dotenv-applied keys would stick for the life of the process.
	OnForceRefresh func()

	// listener is captured at ListenAndServe time so callers (notably the
	// desktop host, which passes Port=0 for an OS-assigned port) can read
	// the actual bind address. Read via Addr(). Mutated only inside
	// ListenAndServe and read after addrReady is closed.
	listener  net.Listener
	addrReady chan struct{}

	// shutdown is closed by Shutdown so background goroutines (upload
	// reaper, future periodic tasks) can stop without polling the HTTP
	// server's state.
	shutdown chan struct{}

	// browserSessions is the per-run Chromium CDP session registry,
	// shared with the runtime. Nil disables the Browser pane's live
	// mode (the iframe + screenshot scrubber paths still work).
	browserSessions mcp.BrowserRegistry

	// boardMCPTokens authorizes sandboxed bots that hit the board MCP
	// HTTP endpoint. Tokens are minted per node by the closure
	// boardMCPServiceOption hands to the runview Service, which calls
	// Register at mint time; a Register failure degrades that node to
	// board-disabled (empty token). Non-nil iff cfg.NativeTrackerStore
	// is non-nil (handler is only mounted when the board exists).
	boardMCPTokens BoardMCPTokenStore

	// forgePublishTokens authorizes runs that POST their review findings
	// to /api/v1/forge/publish-review (the deterministic, tokenless-in-
	// workspace forge publish seam). Tokens are minted per launch by
	// injectForgePublishVars; grants pin (team, connection, repo). Non-nil
	// iff forgeConnections is wired.
	forgePublishTokens ForgePublishTokenStore

	// forgeReviewClientFor is a test seam overriding how the publish-review
	// handler resolves a connection's forge.ReviewClient. Nil → real admin
	// client via forgeAdminFor.
	forgeReviewClientFor func(ctx context.Context, conn forge.Connection) (forge.ReviewClient, error)

	// forgeGateClientFor is a test seam overriding how the publish-review
	// handler resolves a connection's merge-gate client (head-SHA lookup +
	// commit-status write). Nil → real admin client via forgeAdminFor.
	forgeGateClientFor func(ctx context.Context, conn forge.Connection) (forgeGateClient, error)

	// marketplace is the hosted bot registry store. Mirrors
	// Config.Marketplace; nil disables every /api/v1/marketplace/*
	// endpoint (and the studio's Marketplace view via
	// MarketplaceEnabled).
	marketplace marketplace.Store

	// redis is the optional Valkey client backing the distributed state
	// stores (see Config.Redis).
	redis *valkey.Client
}

// BoardMCPTokens returns the per-run token registry the runtime uses
// to authorize sandboxed bots talking to the board MCP HTTP endpoint.
// Returns nil when the server was built without a NativeTrackerStore.
func (s *Server) BoardMCPTokens() BoardMCPTokenStore {
	return s.boardMCPTokens
}

// boardMCPServiceOption builds the runview option that wires the sandboxed
// board MCP transport (C082). It hands the runview Service a DEDICATED mux
// serving ONLY the board MCP routes — safe to expose on the per-run
// gateway-reachable listener (it is token-gated), unlike s.mux which also
// carries authenticated routes — plus a per-node token minter against this
// server's registry. Returns (nil, false) when the native board store
// isn't configured (board-emit then stays disabled, as before).
func (s *Server) boardMCPServiceOption(logger *iterlog.Logger) (runview.ServiceOption, bool) {
	if s.cfg.NativeTrackerStore == nil || s.boardMCPTokens == nil {
		return nil, false
	}
	mux := http.NewServeMux()
	RegisterBoardMCPRoutes(mux, "/api/v1/mcp/board", s.cfg.NativeTrackerStore, s.boardMCPTokens)
	reg := s.boardMCPTokens
	return runview.WithBoardMCP(mux, func(caps []string, sourceIssueID string) string {
		token := newBoardMCPToken()
		if token == "" {
			if logger != nil {
				logger.Warn("board MCP: token generation failed; sandboxed board-emit disabled for a node")
			}
			return ""
		}
		if err := reg.Register(token, caps, sourceIssueID); err != nil {
			if logger != nil {
				logger.Error("board MCP: %v; sandboxed board-emit disabled for a node", err)
			}
			return ""
		}
		return token
	}), true
}

// New creates a new studio server.
//
// Port semantics: cfg.Port == 0 means "let the OS pick a free port"
// (the desktop host depends on this). If you want the legacy default of
// 4891, set it explicitly — pkg/cli.RunStudio does so when the caller
// passes Port=0. Tests that construct Config{} directly previously got
// 4891 by default; they now get a random port, which is what we want
// to avoid cross-test bind conflicts.
func New(cfg Config, logger *iterlog.Logger) *Server {
	// Default to loopback. The previous behaviour was to leave Addr as ":<port>"
	// which binds 0.0.0.0 — exposing the studio (which has unauthenticated
	// /api/files/save and /api/files/open endpoints) to anyone on the LAN.
	// The startup log used to print "http://localhost:<port>" regardless,
	// which actively misled operators about the bind surface. Operators who
	// genuinely want LAN access must now opt in via --bind 0.0.0.0.
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}
	cfg = applyUploadDefaults(cfg)
	if cfg.AccessTTL <= 0 && cfg.AuthSigner != nil {
		cfg.AccessTTL = cfg.AuthSigner.AccessTTL()
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.OIDCStates == nil {
		cfg.OIDCStates = oidc.NewMemoryStateStore(10 * time.Minute)
	}
	if cfg.DesktopTickets == nil {
		cfg.DesktopTickets = desktopsso.NewMemoryStore(desktopTicketTTL)
	}
	if cfg.WSTickets == nil {
		cfg.WSTickets = wsticket.NewMemoryStore(wsTicketTTL)
	}
	s := &Server{
		cfg:               cfg,
		logger:            logger,
		mux:               newRecordingMux(),
		addrReady:         make(chan struct{}),
		shutdown:          make(chan struct{}),
		authSvc:           cfg.AuthService,
		signer:            cfg.AuthSigner,
		oidcRegistry:      cfg.OIDCRegistry,
		oidcStates:        cfg.OIDCStates,
		desktopTickets:    cfg.DesktopTickets,
		wsTickets:         cfg.WSTickets,
		apiKeys:           cfg.ApiKeys,
		genericSecrets:    cfg.GenericSecrets,
		runSecrets:        cfg.RunSecrets,
		sealer:            cfg.Sealer,
		orgSSO:            cfg.OrgSSO,
		orgDomains:        cfg.OrgDomains,
		orgDomainTXT:      orgsso.DefaultTXTLookup(),
		oauthStore:        cfg.OAuthForfait,
		oauthPending:      cfg.OAuthPending,
		webhookConfigs:    cfg.WebhookConfigs,
		webhookDeliveries: cfg.WebhookDeliveries,
		webhookCounter:    cfg.WebhookCounter,
		orgUsage:          cfg.OrgUsage,
		orgDefaults:       cfg.OrgDefaults,
		auditStore:        cfg.Audit,
		pats:              cfg.PATs,
		queue:             cfg.Queue,
		botBindings:       cfg.BotBindings,
		forgeConnections:  cfg.ForgeConnections,
		pluginSources:     cfg.PluginSources,
		botSources:        cfg.BotSources,
		forgeIntegrations: cfg.ForgeIntegrations,
		forgeOAuthApps:    cfg.ForgeOAuthApps,
		forgeGitHubApp:    cfg.ForgeGitHubApp,
		memStore:          cfg.MemoryStore,
		httpClient:        &http.Client{Timeout: 15 * time.Second},
		browserSessions:   cfg.BrowserRegistry,
		statsCache:        newRunStatsCache(),
		locCache:          newRunLOCCache(),
		marketplace:       cfg.Marketplace,
		redis:             cfg.Redis,
	}
	// Local mode wires a *LayeredGenericSecretStore; keep the concrete type so
	// the /api/local/secrets handlers use its scope-aware ops directly. Cloud
	// mode's Mongo store leaves this nil.
	if ls, ok := cfg.GenericSecrets.(*secrets.LayeredGenericSecretStore); ok {
		s.localSecrets = ls
	}
	// Config-share editor store: default to in-memory (local/desktop) when
	// cloud didn't wire a persistent one, so the scoped-editor works out of
	// the box; the Service is stateless over it.
	s.configShares = cfg.ConfigShares
	if s.configShares == nil {
		s.configShares = configshare.NewMemoryStore()
	}
	s.configShareSvc = configshare.NewService(s.configShares)
	if cfg.NativeTrackerStore != nil {
		// Valkey-backed token registry when a distributed backend is wired,
		// else the in-memory one (replaced transparently — same interface).
		if s.redis != nil {
			s.boardMCPTokens = newValkeyBoardMCPTokenStore(s.redis.Redis(), s.logger)
		} else {
			s.boardMCPTokens = NewBoardMCPTokenRegistry()
		}
	}
	if s.forgeConnections != nil {
		// Same replica story as the board MCP tokens: a run's publish POST
		// may land on any server pod, so a distributed backend shares the
		// grants; single-replica keeps the in-memory registry.
		if s.redis != nil {
			s.forgePublishTokens = newValkeyForgePublishTokenStore(s.redis.Redis(), s.logger)
		} else {
			s.forgePublishTokens = NewForgePublishTokenRegistry()
		}
	}
	// Auth rate limiter — eagerly built so the lazy `if s.authLimiter == nil`
	// init sites become no-ops. Valkey-backed (exact across replicas) when a
	// distributed backend is wired, else per-pod in-memory.
	if s.redis != nil {
		s.authLimiter = newValkeyAuthRateLimiter(s.redis.Redis())
	} else {
		s.authLimiter = newAuthRateLimiter()
	}
	// Outbound forge integrations: build the orchestrator + OAuth state
	// store when the full dependency set is present. The orchestrator reuses
	// the existing webhook config + generic secret stores (the managed
	// forge_token rides the unchanged binding/run path).
	if s.forgeConnections != nil && s.forgeIntegrations != nil && s.webhookConfigs != nil && s.genericSecrets != nil && s.sealer != nil {
		if s.redis != nil {
			s.forgeStates = newValkeyForgeStateStore(s.redis.Redis(), 10*time.Minute)
		} else {
			s.forgeStates = newForgeStateStore(10 * time.Minute)
		}
		s.forgeOrchestrator = &forge.Orchestrator{
			Connections:     s.forgeConnections,
			Integrations:    s.forgeIntegrations,
			Webhooks:        s.webhookConfigs,
			Secrets:         s.genericSecrets,
			Sealer:          s.sealer,
			Bindings:        s.botBindings,
			Bots:            s.forgeBotForge,
			Invocations:     s.forgeBotInvocations,
			Schedules:       cfg.ScheduledBots,
			AdminFor:        s.forgeAdminFor,
			GitHubAppMinter: s.forgeAppMinter,
			PublicURL:       cfg.PublicURL,
		}
	}
	s.hub = NewHub(logger)
	go s.hub.Run()
	// File watcher is only meaningful in local mode where the studio
	// SPA is editing files on disk that the server should hot-reload.
	// In cloud mode the server pod has no local source tree (workflows
	// arrive inline on the wire) and starting the watcher there would
	// generate noise events on whatever transient WorkDir was passed.
	if cfg.WorkDir != "" && cfg.Mode != "cloud" {
		var err error
		s.watcher, err = NewWatcher(cfg.WorkDir, s.hub, logger)
		if err != nil {
			logger.Warn("file watcher disabled: %v", err)
		} else {
			go s.watcher.Start()
		}
	}
	// Wire the run console service. A failure here is non-fatal: we log a
	// warning and leave s.runs == nil, which disables /api/runs but keeps
	// the studio usable. The guard preserves the prior behaviour of
	// disabling runs entirely when neither StoreDir nor WorkDir are set
	// (e.g. tests that build a Config{} directly).
	var storeDir string
	if cfg.StoreDir != "" || cfg.WorkDir != "" {
		storeDir = store.ResolveStoreDir(cfg.WorkDir, cfg.StoreDir)
	}
	// When a caller-supplied Store is wired (cloud mode), bypass the
	// filesystem .iterion discovery and inject the store directly so
	// runview.NewService talks to Mongo+S3.
	switch {
	case cfg.Store != nil:
		opts := []runview.ServiceOption{
			runview.WithLogger(logger),
			runview.WithStore(cfg.Store),
		}
		if cfg.LaunchPublisher != nil {
			opts = append(opts, runview.WithLaunchPublisher(cfg.LaunchPublisher))
		}
		if cfg.StreamSource != nil {
			opts = append(opts, runview.WithStreamSource(cfg.StreamSource))
		}
		if cfg.Alerts != nil {
			opts = append(opts, runview.WithAlerts(*cfg.Alerts))
		}
		svc, svcErr := runview.NewService("", opts...)
		if svcErr != nil {
			logger.Warn("run console disabled: %v", svcErr)
		} else {
			s.runs = svc
		}
	case storeDir != "":
		svcOpts := []runview.ServiceOption{
			runview.WithLogger(logger),
			runview.WithMaxConcurrentPipelines(cfg.MaxConcurrentPipelines),
			// Sandbox-by-default for studio/server-launched in-process runs
			// (same resolution as `iterion run`).
			runview.WithSandboxDefault(runtime.ResolveGlobalSandboxDefault()),
		}
		if cfg.WorkDir != "" {
			svcOpts = append(svcOpts, runview.WithWorkDir(cfg.WorkDir))
		}
		if cfg.Alerts != nil {
			svcOpts = append(svcOpts, runview.WithAlerts(*cfg.Alerts))
		}
		if opt, ok := s.boardMCPServiceOption(logger); ok {
			svcOpts = append(svcOpts, opt)
		}
		if s.cfg.Mode != "cloud" && s.localSecrets != nil && s.sealer != nil {
			svcOpts = append(svcOpts, runview.WithLocalSecrets(s.localSecrets, s.sealer))
		}
		svc, svcErr := runview.NewService(storeDir, svcOpts...)
		if svcErr != nil {
			logger.Warn("run console disabled: %v", svcErr)
		} else {
			s.runs = svc
		}
	}
	// Wire the same Origin allowlist used for HTTP CORS into the WebSocket
	// upgrader so cross-origin browser tabs can't subscribe to file events.
	SetWebSocketOriginCheck(s.isAllowedOrigin)
	s.routes()
	// Always wrap in authMiddleware. Public paths (health probes,
	// auth endpoints, static SPA) bypass the JWT check internally;
	// every /api/* call requires a valid bearer / cookie. DEV
	// override: cfg.DisableAuth synthesizes a super-admin Identity
	// instead of rejecting unauthenticated requests.
	s.handler = s.authMiddleware(s.mux)
	s.server = &http.Server{
		Addr:              net.JoinHostPort(cfg.Bind, fmt.Sprintf("%d", cfg.Port)),
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Auto-register the boot workdir in the shared project registry so
	// it shows up in the SPA's ProjectSwitcher — mirrors the desktop
	// app's AddProjectSilently call on first launch. Best-effort: a
	// registry I/O failure is logged but never blocks startup.
	s.registerInitialProject()
	return s
}
