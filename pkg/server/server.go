// Package server provides an HTTP API for the iterion studio.
// It wraps the parser, compiler, and unparser to provide endpoints
// for parsing workflow files, validating workflows, and generating DSL text.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/pat"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// StaticFS embeds the built studio so any importer (the server
// itself, and the desktop GUI's asset proxy) can serve it. Exported
// because cmd/iterion-desktop relies on it to ship the SPA inside the
// GUI binary — that way UI updates don't require a daemon restart.
//
//go:embed all:static
var StaticFS embed.FS

// Config holds the server configuration.
type Config struct {
	Port        int    // HTTP port (default 4891). Pass 0 for an OS-assigned random port.
	Bind        string // bind address (default "127.0.0.1"; use "0.0.0.0" only with explicit user opt-in)
	ExamplesDir string // path to examples directory
	WorkDir     string // root directory for file operations
	StoreDir    string // run store directory (default: <WorkDir>/.iterion)
	OpenBrowser bool   // open browser on start

	// SkipProjectRegistration disables the boot-time call to the
	// shared project registry (~/.config/Iterion/config.json's
	// recent_projects). Tests set this to true so the user's real
	// recents list isn't polluted with /tmp paths from every
	// `newTestServer(t)`. Production (desktop launcher + CLI studio
	// mode) leaves it false so the launched WorkDir surfaces in the
	// switcher.
	SkipProjectRegistration bool

	// AuthService is the multitenant authentication service. When
	// non-nil, every /api/* request is gated by authMiddleware. CLI
	// local mode leaves this nil and DisableAuth=true so the studio
	// process trusts its TTY user (legacy behaviour).
	AuthService *auth.Service

	// AuthSigner is the JWT verifier used by middleware to validate
	// access tokens. Required when AuthService is set.
	AuthSigner *auth.JWTSigner

	// OIDCRegistry maps provider slugs ("google", "github", "sso")
	// to their connectors. nil disables every OIDC route.
	OIDCRegistry *oidc.Registry

	// OIDCStates persists per-flow PendingAuth records between
	// /start and /callback. Defaults to an in-memory store when nil.
	OIDCStates oidc.StateStore

	// CookieDomain narrows the auth cookies' Domain attribute. Empty
	// means host-only cookie (recommended).
	CookieDomain string

	// CookieSecure forces the Secure flag on auth cookies. Should
	// always be true in production. Defaults to false in test/dev.
	CookieSecure bool

	// AccessTTL is the access JWT lifetime; the auth cookie's
	// MaxAge mirrors it. Defaults to AuthSigner.AccessTTL().
	AccessTTL time.Duration

	// RefreshTTL is the refresh cookie / session lifetime.
	// Defaults to 30d.
	RefreshTTL time.Duration

	// PublicURL is the externally-reachable origin (e.g.
	// https://iterion.example) used to build OIDC redirect URIs.
	PublicURL string

	// SignupMode is "open" or "invite_only"; surfaced to the SPA.
	SignupMode string

	// DisableAuth bypasses every auth check — DEV ONLY.
	DisableAuth bool

	// ApiKeys is the BYOK store. When non-nil, the server registers
	// /api/teams/:id/api-keys + /api/me/api-keys and the cloud
	// publisher resolves keys at launch time.
	ApiKeys secrets.ApiKeyStore

	// GenericSecrets stores workflow/user secrets addressable by name
	// from the DSL `secrets:` block. Plaintexts are sealed at rest and
	// are only resolved into per-run sealed bundles by the cloud publisher.
	GenericSecrets secrets.GenericSecretStore

	// Webhook* wire the inbound webhook spine. When WebhookConfigs is
	// non-nil (and the auth stack is present), the server registers the
	// per-org webhook CRUD under /api/teams/:id/webhooks and the inbound
	// /api/webhooks/{provider}/{id} routes.
	WebhookConfigs    webhooks.ConfigStore
	WebhookDeliveries webhooks.DeliveryStore
	WebhookCounter    webhooks.Counter

	// OrgUsage is the per-org monthly run/cost metering counter. When
	// non-nil, every launch (REST, resume, webhook) passes the
	// gateLaunch quota checks and increments the month's run counter;
	// the usage REST views read it back. nil → no metering (local mode).
	OrgUsage orgusage.Counter

	// Audit, when non-nil, persists control-plane mutations (org
	// status, secrets/bindings/webhooks CRUD, member changes…) and
	// enables GET /api/teams/{id}/audit + /api/admin/audit.
	Audit audit.Store
	// OrgDefaults are the platform-wide launch limits applied when a
	// team has no per-org override. Zero values mean "no limit".
	OrgDefaults OrgLimitDefaults

	// PATs, when non-nil, enables personal access tokens: the
	// /api/me/tokens CRUD plus `iap_` bearer authentication in
	// requireAuth. PATMaxTTL (0 = none) caps every token's lifetime
	// regardless of what the caller requests.
	PATs      pat.Store
	PATMaxTTL time.Duration

	// Queue is the cloud-mode NATS connection. When non-nil the
	// server registers the super-admin DLQ endpoints and starts the
	// orphan-run sweeper (paired with a Mongo store).
	Queue *natsq.Conn

	// BotBindings is the policy wrapper over GenericSecrets: it maps a
	// stored org/user secret to a bot under the workflow's declared
	// name. When non-nil, the server registers the bot-binding CRUD and
	// the cloud publisher consults it during secret resolution.
	BotBindings secrets.BotSecretBindingStore

	// ForgeConnections + ForgeIntegrations wire the OUTBOUND forge
	// integration layer (pkg/forge): connect a GitLab/GitHub/Forgejo repo
	// via OAuth/PAT and auto-provision the inbound webhook + token binding
	// when a bot is enabled on it. Both (plus WebhookConfigs + GenericSecrets
	// + Sealer + the auth stack) must be present for /api/teams/:id/forge/*
	// and the OAuth callback to register.
	ForgeConnections  forge.ConnectionStore
	ForgeIntegrations forge.RepoIntegrationStore
	// ForgeOAuthApps holds per-tenant, per-instance forge OAuth-app
	// credentials (sealed client_secret). The connect flow resolves an app
	// from this store for a (tenant, provider, base URL); an instance with no
	// registered app only accepts the PAT fallback. Replaces the legacy
	// process-global ITERION_FORGE_*_OAUTH_* env map.
	ForgeOAuthApps forge.OAuthAppStore
	// ForgeGitHubApp is the global GitHub-App identity for the
	// installation-token connect mode. Empty → that mode is unavailable.
	ForgeGitHubApp ForgeGitHubAppConfig

	// MemoryStore backs the shared-knowledge REST surface
	// (/api/memory/*). nil → the local filesystem store. Cloud mode
	// passes the Mongo store so the studio reads the tenant's memory.
	MemoryStore knowledge.MemoryStore

	// RunSecrets is the per-run sealed bundle store. Required when
	// ApiKeys is set.
	RunSecrets secrets.RunSecretsStore

	// Sealer is the AES-GCM sealer used to encrypt API keys at rest
	// and run-scoped bundles in flight. Required when ApiKeys is set.
	Sealer secrets.Sealer

	// OrgSSO is the per-tenant SSO provider store (per-org Keycloak +
	// GitHub team-gating). When non-nil, the server registers
	// /api/teams/{id}/sso/* CRUD, resolves per-org "oidc-org-<id>"
	// connectors, and surfaces a tenant's providers on
	// /api/auth/providers?org=<slug>. Requires Sealer + AuthService.
	OrgSSO orgsso.Store

	// OrgDomains is the per-tenant verified email-domain store gating per-org
	// SSO auto-link. When set, /api/teams/{id}/sso/domains CRUD is registered.
	OrgDomains orgsso.DomainStore

	// OAuthForfait is the per-user OAuth credential store. When
	// non-nil, the server registers /api/me/oauth/* endpoints and
	// the cloud publisher injects sealed credentials.json /
	// auth.json blobs into the run bundle for runs that don't have
	// a BYOK API key for the relevant provider.
	OAuthForfait secrets.OAuthStore

	// OAuthPending backs the browser OAuth (authorization-code + PKCE)
	// flow — the short-lived per-(owner,kind) PKCE state held between
	// /authorize/start and /authorize/complete. When non-nil (and a
	// client id is configured) the studio can connect a forfait without
	// `claude login` or pasting a credentials.json file.
	OAuthPending secrets.OAuthPendingStore

	// AnthropicOAuthClientID is the OAuth client id used to refresh
	// Claude Code subscription tokens. Empty disables refresh of
	// the claude_code kind (the user must re-upload on expiry).
	AnthropicOAuthClientID string
	// CodexOAuthClientID is the equivalent for Codex.
	CodexOAuthClientID string

	// Store overrides the default filesystem store with a caller-
	// supplied implementation (typically the cloud Mongo+S3 store).
	// When non-nil, StoreDir + the .iterion auto-discovery are
	// ignored and the supplied store is wired into runview.NewService
	// directly. Plan §F (T-30).
	Store store.RunStore

	// LaunchPublisher, when non-nil, routes the run console's Launch /
	// Resume / Cancel through the cloud queue instead of spawning the
	// runtime in-process. Used by `iterion server` in cloud mode
	// (T-31, T-32, T-33).
	LaunchPublisher runview.LaunchPublisher

	// EventSource, when non-nil, replaces the in-process EventBroker
	// for live + historical WS event delivery. Cloud mode wires a
	// Mongo change-stream source so the WS handler sees runner-pod
	// writes. Plan §F (T-21).
	EventSource runview.EventStreamSource

	// ReadinessChecks, when non-nil, are invoked by /readyz to verify
	// every external dependency (Mongo, NATS, S3) is reachable. Each
	// entry is run with a sub-context bounded by ReadinessTimeout (1s
	// by default) so a slow dependency cannot stall the probe past
	// the kubelet's readiness window. Empty in local mode.
	ReadinessChecks map[string]ReadinessCheck

	// ReadinessTimeout caps each ReadinessCheck individually. Defaults
	// to 1s when zero.
	ReadinessTimeout time.Duration

	// Mode advertises the deployment mode in the health response.
	// Defaults to "local" when empty for backward compat with callers
	// that don't set it.
	Mode string

	// TrustedProxyCIDRs is the allowlist of CIDR ranges whose
	// X-Forwarded-For headers we believe. Empty (the default) means
	// we never trust forwarded headers — audit IPs come from
	// r.RemoteAddr only, defeating spoofing by an unprivileged
	// client that sends its own X-Forwarded-For. Set this only when
	// the server sits behind a known L7 proxy/ingress and the proxy
	// rewrites the header on every request.
	TrustedProxyCIDRs []string

	// Metrics, when non-nil, lets the server publish iterion_ws_connections
	// gauge updates as run-console clients connect / disconnect. Other
	// cloud metrics live on the runner / publisher side.
	Metrics *metrics.Registry

	// BrowserRegistry tracks active Chromium CDP sessions for the
	// studio's Browser pane (PR 3 of the browser-simulation feature).
	// When non-nil, the server registers GET
	// /api/runs/{id}/browser/cdp and proxies CDP frames to the
	// matching session. Local + cloud builds wire an in-memory
	// registry shared with the runtime; tests can pass a hand-rolled
	// mock to validate the WS proxy independently of Chromium.
	BrowserRegistry mcp.BrowserRegistry

	// NativeTrackerStore, when non-nil, exposes the dispatcher's native
	// kanban tracker under /api/v1/native/* (issues CRUD + board) so
	// the studio SPA can render the Board view.
	NativeTrackerStore *native.Store

	// TriggerStore, when non-nil alongside NativeTrackerStore, activates the
	// event-driven trigger spine: Serve starts a trigger coordinator that
	// tails native-board transitions, matches them against the stored
	// trigger.Subscriptions, and promotes matching cards (stamping their bot)
	// so the dispatcher picks them up immediately instead of at the next poll.
	// It also backs the /api/v1/triggers subscription CRUD. nil = spine off.
	TriggerStore trigger.SubscriptionStore

	// CloudBoardFor returns a tenant-scoped board store for cloud mode (a
	// boardmongo.Store). When set, a board-mode slash-command materialises a
	// tracked kanban card on that tenant's board (in addition to launching the
	// run). nil in self-hosted/local mode — board-mode then just launches.
	CloudBoardFor func(tenantID string) native.BoardStore

	// CloudBoardCoordinator, when set, activates the cloud board DISPATCHER:
	// Serve starts a CAS-based loop (no leader election) that claims eligible
	// cards across all tenants and runs each via the publisher. With it active,
	// a board-mode command creates the card in the eligible state and does NOT
	// launch directly (the dispatcher owns execution + state transitions);
	// without it, board-mode creates a tracking card + launches directly.
	CloudBoardCoordinator *boardmongo.Coordinator

	// ScheduledBots, when set (cloud mode), backs the recurring-bot scheduler:
	// Serve starts a cloudsched.Ticker that fires each due schedule exactly
	// once (CAS, multi-replica-safe) via the run publisher. nil disables it.
	ScheduledBots cloudsched.Store

	// Bots configures the /api/v1/bots endpoints used by the studio
	// Board ticket form's bot picker. Empty Paths falls back to the
	// project-relative conventions (see BotsConfig.Paths comment).
	Bots BotsConfig

	// Dispatcher, when non-nil, exposes the long-running dispatcher
	// lifecycle + operational endpoints under /api/v1/dispatcher/*.
	// The Manager owns the full surface (config GET/PUT, start/stop/
	// pause/resume, state, refresh, issue cancel, WS) so the studio
	// SPA can configure and pilot the dispatcher without a separate
	// `iterion dispatch` process. When nil the SPA hides the
	// Dispatcher + Board controls beyond plain CRUD.
	Dispatcher *dispatcher.Manager

	// MaxUploadSize bounds the bytes the upload endpoint will accept
	// per attachment. Zero is replaced with a mode-specific default
	// (1 GB desktop, 50 MB web/cloud) at registration time.
	MaxUploadSize int64
	// MaxTotalUploadSize bounds the cumulative bytes per run across
	// every attachment. Zero defaults to 5x MaxUploadSize.
	MaxTotalUploadSize int64
	// MaxUploadsPerRun caps how many distinct attachments may
	// reference a single run. Zero defaults to 20.
	MaxUploadsPerRun int
	// AllowedUploadMIMEs is the server-side allowlist applied to
	// every upload's sniffed MIME. Each entry is a `type/subtype`
	// pattern with optional `*` wildcards (e.g. `image/*`). Empty
	// means "use the built-in safe defaults" (image/png, image/jpeg,
	// image/gif, image/webp, application/pdf, application/json,
	// text/plain, text/markdown, text/csv, application/yaml,
	// application/zip, application/gzip, application/x-tar).
	AllowedUploadMIMEs []string

	// Alerts, when non-nil, enables run-health alerting (stall / budget /
	// failure) on the run console service. The settings carry the webhook
	// URL, stall window, deep-link base URL and an optional desktop sink;
	// the always-on browser sink publishes EventAlert to each run's broker
	// so the SPA can toast. nil disables alerting entirely.
	Alerts *runview.AlertSettings

	// Marketplace, when non-nil, enables the hosted bot registry under
	// /api/v1/marketplace/* (browse + submit + install). The studio
	// surfaces the Marketplace view only when this is wired; nil hides
	// the feature entirely. Self-host / local mode typically passes the
	// JSON-file store (marketplace.NewJSONStore); cloud mode passes the
	// Mongo store (marketplace.NewMongoStore).
	Marketplace marketplace.Store
}

// ReadinessCheck is the contract /readyz invokes on each external
// dependency. It MUST be cheap (HEAD/ping) and MUST respect the
// supplied context's deadline.
type ReadinessCheck func(ctx context.Context) error

// Server is the studio HTTP server.
type Server struct {
	// stateMu guards the hot-swappable fields used by ProjectSwitcher
	// (cfg.WorkDir, cfg.StoreDir, runs, watcher). Acquired write-side
	// only during a project switch — handlers that read these fields
	// during a swap may briefly see one half of the swap, which is
	// acceptable: the SPA reset on `project_switched` invalidates any
	// inflight request data before it's surfaced.
	stateMu sync.RWMutex
	// currentProjectID is the id of the registry entry matching
	// cfg.WorkDir. Surfaced by /api/server/info (polled by the SPA);
	// caching it here avoids a disk read on every poll.
	currentProjectID string
	cfg              Config
	logger           *iterlog.Logger
	mux              *http.ServeMux
	handler          http.Handler // mux wrapped with auth middleware
	server           *http.Server
	hub              *Hub
	watcher          *Watcher
	runs             *runview.Service    // run console service; nil disables /api/runs endpoints
	watchCoord       *watchCoordinator   // MVP3b issue-state fan-out; nil when no native tracker or events tail unavailable
	triggerCoord     *TriggerCoordinator // event-driven trigger spine; nil when no TriggerStore/native tracker
	// statsCache memoizes the per-run events.jsonl cost scan behind
	// /api/v1/runs/stats (terminal runs only — see runs_stats_cache.go).
	// Cleared on project switch. Non-nil after New.
	statsCache *runStatsCache

	authSvc           *auth.Service
	authLimiter       *authRateLimiter
	signer            *auth.JWTSigner
	oidcRegistry      *oidc.Registry
	oidcStates        oidc.StateStore
	apiKeys           secrets.ApiKeyStore
	genericSecrets    secrets.GenericSecretStore
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
	forgeConnections  forge.ConnectionStore
	forgeIntegrations forge.RepoIntegrationStore
	forgeOrchestrator *forge.Orchestrator
	forgeStates       *forgeStateStore
	forgeOAuthApps    forge.OAuthAppStore
	forgeGitHubApp    ForgeGitHubAppConfig
	orgSSO            orgsso.Store
	orgDomains        orgsso.DomainStore
	orgDomainTXT      orgsso.TXTLookupFunc
	memStore          knowledge.MemoryStore
	// webhookLaunchBot overrides the inbound-webhook launch path (test
	// seam). nil → realWebhookLaunchBot (resolve bot source + s.runs.Launch).
	webhookLaunchBot func(ctx context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, keyOverrides, secretOverrides map[string]string) (string, error)
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
	httpClient                *http.Client

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
	// HTTP endpoint. The runtime obtains the handle via [Server.BoardMCPTokens]
	// and calls Register/Revoke around each run. Non-nil iff
	// cfg.NativeTrackerStore is non-nil (handler is only mounted when
	// the board exists).
	boardMCPTokens *BoardMCPTokenRegistry

	// marketplace is the hosted bot registry store. Mirrors
	// Config.Marketplace; nil disables every /api/v1/marketplace/*
	// endpoint (and the studio's Marketplace view via
	// MarketplaceEnabled).
	marketplace marketplace.Store
}

// BoardMCPTokens returns the per-run token registry the runtime uses
// to authorize sandboxed bots talking to the board MCP HTTP endpoint.
// Returns nil when the server was built without a NativeTrackerStore.
func (s *Server) BoardMCPTokens() *BoardMCPTokenRegistry {
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
	return runview.WithBoardMCP(mux, func(caps []string) string {
		token := newBoardMCPToken()
		if token == "" {
			if logger != nil {
				logger.Warn("board MCP: token generation failed; sandboxed board-emit disabled for a node")
			}
			return ""
		}
		reg.Register(token, caps)
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
	s := &Server{
		cfg:               cfg,
		logger:            logger,
		mux:               http.NewServeMux(),
		addrReady:         make(chan struct{}),
		shutdown:          make(chan struct{}),
		authSvc:           cfg.AuthService,
		signer:            cfg.AuthSigner,
		oidcRegistry:      cfg.OIDCRegistry,
		oidcStates:        cfg.OIDCStates,
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
		forgeIntegrations: cfg.ForgeIntegrations,
		forgeOAuthApps:    cfg.ForgeOAuthApps,
		forgeGitHubApp:    cfg.ForgeGitHubApp,
		memStore:          cfg.MemoryStore,
		httpClient:        &http.Client{Timeout: 15 * time.Second},
		browserSessions:   cfg.BrowserRegistry,
		statsCache:        newRunStatsCache(),
		marketplace:       cfg.Marketplace,
	}
	if cfg.NativeTrackerStore != nil {
		s.boardMCPTokens = NewBoardMCPTokenRegistry()
	}
	// Outbound forge integrations: build the orchestrator + OAuth state
	// store when the full dependency set is present. The orchestrator reuses
	// the existing webhook config + generic secret stores (the managed
	// forge_token rides the unchanged binding/run path).
	if s.forgeConnections != nil && s.forgeIntegrations != nil && s.webhookConfigs != nil && s.genericSecrets != nil && s.sealer != nil {
		s.forgeStates = newForgeStateStore(10 * time.Minute)
		s.forgeOrchestrator = &forge.Orchestrator{
			Connections:  s.forgeConnections,
			Integrations: s.forgeIntegrations,
			Webhooks:     s.webhookConfigs,
			Secrets:      s.genericSecrets,
			Sealer:       s.sealer,
			Bots:         s.forgeBotForge,
			Invocations:  s.forgeBotInvocations,
			Schedules:    cfg.ScheduledBots,
			AdminFor:     s.forgeAdminFor,
			PublicURL:    cfg.PublicURL,
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
		if cfg.EventSource != nil {
			opts = append(opts, runview.WithEventSource(cfg.EventSource))
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
		svcOpts := []runview.ServiceOption{runview.WithLogger(logger)}
		if cfg.WorkDir != "" {
			svcOpts = append(svcOpts, runview.WithWorkDir(cfg.WorkDir))
		}
		if cfg.Alerts != nil {
			svcOpts = append(svcOpts, runview.WithAlerts(*cfg.Alerts))
		}
		if opt, ok := s.boardMCPServiceOption(logger); ok {
			svcOpts = append(svcOpts, opt)
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

// Addr returns the actual bound address (host:port) once ListenAndServe has
// successfully created its listener. It blocks until the listener is ready or
// the context is cancelled. Used by the desktop host when Port=0 was passed
// and the OS picks the port.
func (s *Server) Addr() string {
	<-s.addrReady
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		// Even on error, signal Addr() so callers don't block forever.
		close(s.addrReady)
		return err
	}
	s.listener = ln
	// Reflect the OS-chosen port back into the config so logging and the
	// origin allowlist see the real port (Port=0 mode).
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.cfg.Port = tcpAddr.Port
	}
	close(s.addrReady)
	// Sweep abandoned upload staging dirs in the background. Without
	// this, attachments uploaded for runs that never launched (operator
	// closed the modal, browser crashed mid-upload, etc.) accumulate
	// under <store>/uploads/ until the disk fills. The reaper itself
	// is best-effort — it walks the staging root, deletes dirs older
	// than uploadStagingTTL, and stops when s.shutdown closes.
	if s.runs != nil {
		go s.runStagedUploadReaper()
	}
	// MVP3b: fan native-board issue-state transitions out to runs that
	// subscribed (Run.WatchedIssueIDs). No-op when no native tracker is
	// wired or the events tail can't start.
	if s.runs != nil && s.cfg.NativeTrackerStore != nil {
		s.watchCoord = startWatchCoordinator(s.runs, s.cfg.NativeTrackerStore, s.logger)
	}
	// Event-driven trigger spine: tail board transitions → match stored
	// subscriptions → promote matching cards (the dispatcher then claims
	// them). No-op when no TriggerStore is wired. The dispatcher Manager
	// doubles as the nudger so a promoted card dispatches now, not at poll.
	if s.cfg.NativeTrackerStore != nil && s.cfg.TriggerStore != nil {
		var nudger trigger.Nudger
		if s.cfg.Dispatcher != nil {
			nudger = s.cfg.Dispatcher
		}
		var launcher trigger.Launcher
		if s.runs != nil {
			launcher = newServiceLauncher(s.runs, s.effectivePaths(), s.logger)
		}
		s.triggerCoord = StartTriggerCoordinator(s.cfg.NativeTrackerStore, s.cfg.TriggerStore, nudger, launcher, s.logger)
		// Wire the run-completion source onto the same bus so a finished /
		// failed run can fire downstream trigger subscriptions.
		if s.triggerCoord != nil && s.runs != nil {
			s.runs.SetEventPublisher(s.triggerCoord.Bus())
		}
	}
	// Sweep abandoned OIDC PendingAuth entries — a user who clicks
	// "Sign in with Google" then closes the tab never returns to
	// trigger the lazy eviction inside Take, so without this the
	// in-memory store grows unbounded under brute-force attempts
	// or distracted users.
	if mss, ok := s.oidcStates.(*oidc.MemoryStateStore); ok {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			mss.StartSweeper(ctx, 0) // 0 = use store TTL as interval
		}()
	}
	// Forge OAuth token refresh: keep oauth_app connection tokens (and their
	// managed forge_token secrets) fresh so bot runs never read an expired
	// credential. PAT connections are skipped by the worker. No-op when the
	// forge orchestrator isn't wired (local mode).
	if s.forgeOrchestrator != nil {
		worker := &forge.RefreshWorker{
			Connections:  s.forgeConnections,
			Secrets:      s.genericSecrets,
			Sealer:       s.sealer,
			RefresherFor: s.forgeRefresherFor,
			Lead:         5 * time.Minute,
		}
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := worker.RunOnce(ctx); err != nil && s.logger != nil {
						s.logger.Warn("forge token refresh: %v", err)
					}
				}
			}
		}()
	}
	// OAuth-forfait token refresh: proactively rotate Claude Code (and
	// Codex) subscription access tokens before they expire so neither an
	// interactive run nor an automated (webhook/dispatcher/cron) run ever
	// reads a stale credential. Covers personal AND org-scoped records.
	// No-op without a store/sealer or any configured client id.
	if s.oauthStore != nil && s.sealer != nil && (s.cfg.AnthropicOAuthClientID != "" || s.cfg.CodexOAuthClientID != "") {
		worker := &secrets.OAuthRefreshWorker{
			Store:             s.oauthStore,
			Sealer:            s.sealer,
			HTTP:              s.httpClient,
			AnthropicClientID: s.cfg.AnthropicOAuthClientID,
			CodexClientID:     s.cfg.CodexOAuthClientID,
			Lead:              30 * time.Minute,
		}
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := worker.RunOnce(ctx); err != nil && s.logger != nil {
						s.logger.Warn("oauth-forfait refresh: %v", err)
					} else if n > 0 && s.logger != nil {
						s.logger.Info("oauth-forfait refresh: rotated %d token(s)", n)
					}
				}
			}
		}()
	}
	// Orphan-run sweeper (cloud only): flips queued/running rows whose
	// runner died without a terminal write to failed_resumable. Needs
	// both the Mongo store (stale scan capability) and the queue (KV
	// lease check) — silently absent otherwise (local mode).
	if lister, ok := s.cfg.Store.(staleRunLister); ok && s.queue != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.runQueueSweeper(ctx, lister, s.queue)
		}()
	}
	// Cloud scheduler: fire due cron-scheduled bots. Multi-replica-safe via the
	// store CAS (no leader election). Absent in local mode (ScheduledBots nil).
	if s.cfg.ScheduledBots != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			(&cloudsched.Ticker{
				Store:  s.cfg.ScheduledBots,
				Launch: s.launchScheduledBot,
				Logger: s.logger,
			}).Run(ctx)
		}()
	}
	// Cloud board dispatcher: claim + run eligible cards across all tenants.
	// Multi-replica-safe via the per-card Claim CAS (no leader election).
	if s.cfg.CloudBoardCoordinator != nil {
		marker := "board-dispatcher:" + uuid.NewString()
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			newBoardDispatcher(s.cfg.CloudBoardCoordinator, s.processBoardCard, marker, 4, s.logger).run(ctx)
		}()
	}
	// Truthful URL in the log: if the operator chose a non-loopback bind we
	// print the actual address so they know the studio is exposed beyond the
	// local machine. Previously we always printed http://localhost:<port>
	// regardless of the bind interface.
	displayHost := s.cfg.Bind
	if displayHost == "127.0.0.1" || displayHost == "::1" || displayHost == "" {
		displayHost = "localhost"
	}
	s.logger.Info("Editor server listening on http://%s:%d", displayHost, s.cfg.Port)
	return s.server.Serve(ln)
}

// Shutdown gracefully shuts down the server.
//
// Order matters: HTTP-level shutdown (Server.Shutdown) drains in-flight
// requests, while the run console service drains in-process workflow
// goroutines. We do the workflow drain first so any cancel events
// reach the on-disk store before the file watcher stops broadcasting
// and clients drop. The drain ctx is the caller-supplied shutdown
// deadline.
//
// Drain (rather than Stop) is intentional: it flips each in-flight
// run to failed_resumable and emits EventRunInterrupted so the next
// boot can offer one-click resume and clients can distinguish
// shutdown-induced termination from user-initiated cancel.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.runs != nil {
		s.runs.Drain(ctx)
	}
	// Stop the dispatcher before the HTTP server tears down so its
	// shutdown() path can release in-flight claims to a clean state.
	// Without this the daemon's SIGTERM (watchexec restart, operator
	// Ctrl+C) would orphan claimed-by-self tickets on disk and the
	// next dispatcher start would skip them (ListCandidates filters
	// Claimed=true) until the operator manually edited the JSON.
	// See ticket 012cb3a2 / 7221c7be.
	if s.cfg.Dispatcher != nil {
		// Shutdown (not Stop) so a server SIGTERM / Ctrl-C / watchexec
		// rebuild preserves the operator's last-known intent in
		// runtime.json. Stop persists `desired=stopped` — that's
		// operator-driven only.
		s.cfg.Dispatcher.Shutdown()
	}
	if s.watchCoord != nil {
		s.watchCoord.Close()
	}
	if s.triggerCoord != nil {
		s.triggerCoord.Close()
	}
	if s.watcher != nil {
		s.watcher.Stop()
	}
	s.hub.Stop()
	// Close shutdown last so background goroutines (upload reaper)
	// observe it after the drain has settled and HTTP handlers have
	// stopped accepting new requests.
	select {
	case <-s.shutdown:
		// already closed (idempotent shutdown)
	default:
		close(s.shutdown)
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) routes() {
	// CORS preflight handler — only echoes ACAO when the Origin is an
	// allowed loopback origin. The wildcard ACAO previously emitted here
	// (combined with POST /api/files/save accepting JSON bodies) allowed
	// any browser tab the user visited to write attacker-controlled workflow
	// files into WorkDir, which iterion would then execute under `sh -c`
	// the next time the user ran the workflow — drive-by RCE on the dev
	// machine. The 'local-only server' framing didn't address this because
	// the threat is browser-side, not network-side.
	s.mux.HandleFunc("OPTIONS /api/", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !s.isAllowedOriginReq(r) {
			// No ACAO header → browser blocks the cross-origin request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})

	s.mux.HandleFunc("POST /api/parse", s.handleParse)
	s.mux.HandleFunc("POST /api/unparse", s.handleUnparse)
	s.mux.HandleFunc("POST /api/validate", s.handleValidate)
	s.mux.HandleFunc("GET /api/examples", s.handleListExamples)
	s.mux.HandleFunc("GET /api/examples/{name...}", s.handleLoadExample)
	s.mux.HandleFunc("GET /api/effort-capabilities", s.handleEffortCapabilities)
	s.mux.HandleFunc("GET /api/resolve-effort", s.handleResolveEffort)
	s.mux.HandleFunc("GET /api/backends/detect", s.handleBackendsDetect)

	// Bot registry — exposes the bots discoverable on the host (single
	// .bot files, .botz bundles) with their declared workflow vars +
	// presets so the studio's Board ticket form can render a typed
	// args form per bot. Read-only, gated by the same auth middleware
	// as the rest of /api/* (the wrapper at line ~427 wraps the mux).
	s.mux.HandleFunc("GET /api/v1/bots", s.handleBotsList)
	s.mux.HandleFunc("POST /api/v1/bots/install", s.handleBotInstall)
	s.mux.HandleFunc("POST /api/v1/bots/upload", s.handleBotUpload)
	s.mux.HandleFunc("GET /api/v1/bots/{name}", s.handleBotsGet)
	s.mux.HandleFunc("PUT /api/v1/bots/{name}", s.handleBotsPut)
	s.mux.HandleFunc("PUT /api/v1/bots/{name}/overlay", s.handleBotOverlay)

	// Hosted marketplace — curated registry of bot bundles published by
	// repos. Each endpoint short-circuits to 404 when s.marketplace is
	// nil so wiring is single-config (the studio gates its view on
	// MarketplaceEnabled in /api/server/info).
	s.mux.HandleFunc("GET /api/v1/marketplace/bots", s.handleMarketplaceList)
	s.mux.HandleFunc("POST /api/v1/marketplace/submit", s.handleMarketplaceSubmit)
	s.mux.HandleFunc("GET /api/v1/marketplace/config", s.handleMarketplaceConfig)
	s.mux.HandleFunc("GET /api/v1/marketplace/bots/{slug}", s.handleMarketplaceGet)
	// Public .botz download (see isPublicMarketplaceRead) — packs the
	// bundle on demand from its source coordinates.
	s.mux.HandleFunc("GET /api/v1/marketplace/bots/{slug}/download", s.handleMarketplaceDownload)
	s.mux.HandleFunc("POST /api/v1/marketplace/bots/{slug}/install", s.handleMarketplaceInstall)
	s.mux.HandleFunc("DELETE /api/v1/marketplace/bots/{slug}/install", s.handleMarketplaceUninstall)
	// Moderation (cloud-only; handlers 404 in local mode).
	s.mux.HandleFunc("GET /api/v1/marketplace/moderation", s.handleMarketplaceModerationList)
	s.mux.HandleFunc("POST /api/v1/marketplace/moderation/{slug}/approve", s.handleMarketplaceApprove)
	s.mux.HandleFunc("POST /api/v1/marketplace/moderation/{slug}/reject", s.handleMarketplaceReject)

	// Health endpoints — liveness (always 200 if the mux is alive)
	// and readiness (cloud-mode dependency pings come via T-26 when
	// Mongo/NATS/S3 ping handles are threaded into the server). Used
	// by the Helm chart probes (plan §F T-36, T-37).
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// WebSocket endpoint for file watching
	s.mux.HandleFunc("GET /api/ws", s.hub.HandleWebSocket)

	// File management endpoints
	s.mux.HandleFunc("GET /api/files", s.handleListFiles)
	s.mux.HandleFunc("POST /api/files/open", s.handleOpenFile)
	s.mux.HandleFunc("POST /api/files/save", s.handleSaveFile)

	// Project registry — lets the SPA list MRU projects, switch
	// between them, and add/remove entries. The same on-disk file
	// (<UserConfigDir>/Iterion/config.json) is shared with the desktop
	// (Wails) app via pkg/server/projects.
	s.mux.HandleFunc("GET /api/projects", s.handleListProjects)
	s.mux.HandleFunc("GET /api/projects/current", s.handleCurrentProject)
	s.mux.HandleFunc("POST /api/projects", s.handleAddProject)
	s.mux.HandleFunc("POST /api/projects/switch", s.handleSwitchProject)
	s.mux.HandleFunc("DELETE /api/projects/{id}", s.handleRemoveProject)
	// Server-side directory browser, gated by the ITERION_BROWSE_ROOT
	// env var. Used by the web-mode AddProject dialog so the user can
	// pick a folder without typing its absolute path. Disabled (403)
	// when the env var is unset, so a default deployment never exposes
	// the filesystem.
	s.mux.HandleFunc("GET /api/filesystem/list", s.handleFilesystemList)

	// Run console endpoints (registered only when s.runs is wired).
	s.registerRunRoutes()
	s.registerRunLogRoutes()

	// Auth + identity endpoints (login, logout, refresh, OIDC,
	// teams, invitations). Registered only when authSvc is wired —
	// local mode without an auth service skips them.
	if s.authSvc != nil {
		s.registerAuthRoutes()
	}

	// BYOK endpoints. Requires the auth+identity stack already in
	// place — caller must wire AuthService + ApiKeys + Sealer.
	if s.apiKeys != nil && s.sealer != nil && s.authSvc != nil {
		s.registerBYOKRoutes()
	}
	if s.genericSecrets != nil && s.sealer != nil && s.authSvc != nil {
		s.registerGenericSecretRoutes()
	}

	// Inbound webhook spine: per-org webhook token CRUD. The inbound
	// /api/webhooks/{provider}/{id} delivery routes are registered by
	// each provider (see registerGitLabWebhookRoute).
	if s.webhookConfigs != nil && s.authSvc != nil {
		s.registerWebhookRoutes()
	}
	// Inbound delivery routes (self-authenticating via webhookAuth) —
	// one per supported provider, registered whenever the config store
	// is present. Each handler is independent: a tenant who only wires
	// up GitLab can ignore the GitHub/Forgejo/generic URLs.
	if s.webhookConfigs != nil {
		s.registerGitLabWebhookRoute()
		s.registerGitHubWebhookRoute()
		s.registerForgejoWebhookRoute()
		s.registerGenericWebhookRoute()
	}

	// Bot-secret bindings (policy wrapper over generic secrets).
	if s.botBindings != nil && s.authSvc != nil {
		s.registerBotBindingRoutes()
	}

	// Outbound forge integrations (connect a repo + auto-provision). Gated
	// on the orchestrator being built (full dependency set) + the auth stack.
	if s.forgeOrchestrator != nil && s.authSvc != nil {
		s.registerForgeRoutes()
		s.registerForgeProvisioningRoutes()
		if s.forgeOAuthApps != nil {
			s.registerForgeOAuthAppRoutes()
		}
	}

	// Per-tenant SSO providers (a tenant's own Keycloak + GitHub team-gating).
	// Needs the auth stack + a sealer for the OIDC client secret.
	if s.orgSSO != nil && s.authSvc != nil && s.sealer != nil {
		s.registerOrgSSORoutes()
	}
	if s.orgDomains != nil && s.authSvc != nil {
		s.registerOrgSSODomainRoutes()
	}

	// Audit log read surface (writes happen inline in the mutation
	// handlers via auditTenant/auditPlatform). No-op when no store.
	if s.authSvc != nil {
		s.registerAuditRoutes()
	}

	// Personal access tokens (programmatic API access).
	if s.pats != nil && s.authSvc != nil {
		s.registerPATRoutes()
	}

	// Super-admin DLQ inspection/replay (cloud only — needs the queue).
	if s.authSvc != nil {
		s.registerQueueAdminRoutes()
	}

	// Shared-knowledge memory REST (FS fallback when no store wired).
	s.registerMemoryRoutes()

	// OAuth-forfait endpoints. Same gating as BYOK plus the per-
	// user OAuthForfait store.
	if s.oauthStore != nil && s.sealer != nil && s.authSvc != nil {
		s.registerOAuthForfaitRoutes()
		// Team/org-scoped mirror (admin-gated). Only meaningful when the
		// auth store can resolve team membership.
		if s.authStore() != nil {
			s.registerOAuthTeamRoutes()
		}
	}

	// Dispatcher + native tracker — both optional. Each handler is
	// registered through requireAuth so a server bound to a non-loopback
	// address (devcontainer / LAN / SSH tunnel) can't have its kanban
	// or dispatcher state mutated by an unauthenticated peer. The
	// RegisterRoutesWithMiddleware variants preserve method-specific
	// patterns so they don't conflict with the server's OPTIONS /api/
	// catch-all.
	if s.cfg.NativeTrackerStore != nil {
		s.cfg.NativeTrackerStore.RegisterRoutesWithMiddleware(s.mux, "/api/v1/native", s.requireAuth)
		// The Board MCP HTTP endpoint authenticates via its own
		// per-run X-Iterion-Run token (issued by the runtime at
		// run-start), so it intentionally bypasses requireAuth.
		RegisterBoardMCPRoutes(s.mux, "/api/v1/mcp/board", s.cfg.NativeTrackerStore, s.boardMCPTokens)
		// Resolve a "/command" in a board-issue comment into a bot launch (the
		// native/local twin of the forge issue-comment trigger). requireAuth on
		// the comment route already gates who may post; the resolver only adds
		// command→bot routing + the open_mr stamp.
		s.wireNativeBoardCommands()
	}
	if s.cfg.Dispatcher != nil {
		s.cfg.Dispatcher.RegisterRoutesWithMiddleware(s.mux, "/api/v1/dispatcher", s.requireAuth)
	}
	// Event-driven trigger subscription CRUD backing the Triggers /
	// Automations view. No-op without a TriggerStore.
	if s.cfg.TriggerStore != nil {
		s.registerTriggerRoutes()
	}
	// Cross-run stats aggregation backing /insights. No-op when the
	// server runs without a run-store handle (cloud control plane).
	s.registerRunsStatsRoutes()

	// Distinct repositories (project_path) backing the run-list "by repo"
	// filter chips. No-op without a run-store handle.
	s.registerRunsReposRoutes()

	// Daily spend-cap status + one-click override. No-op without a run
	// store. Gated by the global /api/* auth middleware.
	s.registerLimitsRoutes()

	// Serve static frontend files with SPA fallback so client-side routes
	// (e.g. /runs/abc) render index.html instead of 404.
	staticSub, err := fs.Sub(StaticFS, "static")
	if err != nil {
		log.Fatalf("failed to create sub filesystem: %v", err)
	}
	s.mux.Handle("GET /", SPAHandler(staticSub))
}

// --- Request/Response types ---

type parseRequest struct {
	Source string `json:"source"`
}

type parseResponse struct {
	Document    json.RawMessage `json:"document"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
}

// DiagnosticDTO is the wire-safe shape of an ir.Diagnostic. It carries the
// structured fields (code, severity, attribution, hint) so the studio can
// render inline badges without resorting to string-matching the message.
type DiagnosticDTO struct {
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	NodeID   string `json:"node_id,omitempty"`
	EdgeID   string `json:"edge_id,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

func irDiagToDTO(d ir.Diagnostic) DiagnosticDTO {
	sev := "error"
	if d.Severity == ir.SeverityWarning {
		sev = "warning"
	}
	return DiagnosticDTO{
		Code:     string(d.Code),
		Severity: sev,
		Message:  d.Message,
		NodeID:   d.NodeID,
		EdgeID:   d.EdgeID,
		Hint:     d.Hint,
	}
}

type unparseRequest struct {
	Document json.RawMessage `json:"document"`
}

type unparseResponse struct {
	Source string `json:"source"`
}

type validateRequest struct {
	Document json.RawMessage `json:"document"`
}

type validateResponse struct {
	// Legacy string shape — preserved for any external consumer that already
	// reads it. New consumers should prefer Issues, which carries structured
	// attribution and hints.
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
	Valid       bool            `json:"valid"`
	NodeCount   int             `json:"node_count,omitempty"`
	EdgeCount   int             `json:"edge_count,omitempty"`
}

// --- File management types ---

type listFilesResponse struct {
	Files []fileEntry `json:"files"`
}

type fileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type openFileRequest struct {
	Path string `json:"path"`
}

type saveFileRequest struct {
	Path     string          `json:"path"`
	Document json.RawMessage `json:"document"`
}

type saveFileResponse struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// --- Handlers ---

func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) {
	var req parseRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	pr := parser.Parse("studio.bot", req.Source)

	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}

	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}

	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}

	writeJSON(w, parseResponse{
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
	})
}

func (s *Server) handleUnparse(w http.ResponseWriter, r *http.Request) {
	var req unparseRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	source := unparse.Unparse(f)
	writeJSON(w, unparseResponse{Source: source})
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	resp := validateResponse{Valid: true}

	// Parse diagnostics (re-validate via compiler).
	cr := ir.Compile(f)
	for _, d := range cr.Diagnostics {
		msg := d.Error()
		resp.Issues = append(resp.Issues, irDiagToDTO(d))
		if d.Severity == ir.SeverityError {
			resp.Diagnostics = append(resp.Diagnostics, msg)
			resp.Valid = false
		} else {
			resp.Warnings = append(resp.Warnings, msg)
		}
	}

	if cr.Workflow != nil {
		resp.NodeCount = len(cr.Workflow.Nodes)
		resp.EdgeCount = len(cr.Workflow.Edges)
	}

	writeJSON(w, resp)
}

func (s *Server) handleListExamples(w http.ResponseWriter, _ *http.Request) {
	// Two sources, merged + de-duplicated, surfaced as the studio Home's
	// "Bots" quick-open panel:
	//   1. <ExamplesDir>/<bot>/main.bot — first-class bots shipped on
	//      disk (e.g. <repo>/bots/ when the user opens an iterion repo),
	//      filtered to bundles whose manifest declares a display_name
	//      persona (see isFirstClassBot).
	//   2. The bot recipes embedded in the binary (bots/embed.go), so a
	//      fresh project that ships none of its own still gets the
	//      canonical built-ins (feature_dev, whole/branch_improve_loop).
	// On-disk wins on name collision: a project that overrides an
	// embedded recipe by placing one with the same relative name in its
	// bots/ dir gets to override what the SPA loads.
	seen := map[string]struct{}{}
	var entries []exampleEntry

	if dir := s.cfg.ExamplesDir; dir != "" {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != dir && isSkippedDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !workflowfile.IsWorkflowFile(d.Name()) {
				return nil
			}
			// Only surface first-class bots — a bundle's main.bot whose
			// manifest.yaml declares a display_name persona. Drops loose
			// .bot files (smoke tests) and un-personified bundles so the
			// Home panel shows exactly the named team.
			persona, desc, ok := firstClassBot(path)
			if !ok {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}
			// Normalize to forward slashes so the name matches the
			// embed FS convention and the load endpoint.
			rel = filepath.ToSlash(rel)
			if _, dup := seen[rel]; dup {
				return nil
			}
			seen[rel] = struct{}{}
			entries = append(entries, exampleEntry{Name: rel, DisplayName: persona, Description: desc})
			return nil
		})
	}

	for _, p := range bots.List() {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		// Embedded recipes carry no manifest in the binary, so no persona
		// here; the on-disk walk above already surfaced (with its persona)
		// any bundle the workspace actually ships.
		entries = append(entries, exampleEntry{Name: p})
	}

	// Present the bots in the curated team order (Nexie first), not the
	// filesystem walk order.
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := botRosterRank(entries[i].Name), botRosterRank(entries[j].Name)
		if ri != rj {
			return ri < rj
		}
		return entries[i].Name < entries[j].Name
	})

	if entries == nil {
		entries = []exampleEntry{}
	}
	writeJSON(w, entries)
}

// exampleEntry is one bot surfaced by the studio Home "Bots" panel:
// its relative load name (e.g. "whats-next/main.bot") plus the manifest
// persona + a one-line description the SPA shows in place of the raw
// path. (The technical name is the first path segment, derived SPA-side.)
type exampleEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// firstClassBot returns the persona + a one-line description of the bot
// bundle whose entry point is mainBotPath, and whether it is first-class
// — a "main.bot" whose sibling manifest.yaml declares a non-empty
// display_name. The studio Home lists only first-class bots, so loose
// .bot files (smoke tests) and un-personified bundles stay out.
func firstClassBot(mainBotPath string) (persona, description string, ok bool) {
	if filepath.Base(mainBotPath) != "main.bot" {
		return "", "", false
	}
	m, err := bundle.LoadManifest(filepath.Join(filepath.Dir(mainBotPath), "manifest.yaml"))
	if err != nil || m == nil {
		return "", "", false
	}
	persona = strings.TrimSpace(m.DisplayName)
	if persona == "" {
		return "", "", false
	}
	return persona, shortDescription(m.Description), true
}

// shortDescription condenses a multi-line manifest description into a
// single tidy line for the Home: whitespace collapsed, trimmed to the
// first sentence (or ~140 chars) so the panel rows stay compact.
func shortDescription(desc string) string {
	d := strings.Join(strings.Fields(desc), " ")
	if d == "" {
		return ""
	}
	if i := strings.Index(d, ". "); i > 0 && i < 160 {
		return d[:i+1]
	}
	const max = 140
	if len(d) > max {
		return strings.TrimSpace(d[:max-1]) + "…"
	}
	return d
}

// botRosterOrder is the curated team order the studio surfaces present
// bots in — Nexie first, then the build / improve / doc / review /
// security line (matches the README "Meet the legion" table). Bots
// outside the roster sort after it, alphabetically.
var botRosterOrder = []string{
	"whats-next",          // Nexie
	"feature-dev",         // Featurly
	"branch-improve-loop", // Billy
	"whole-improve-loop",  // Willy
	"docs-refresh",        // Doki
	"review-pr",           // Revi
	"sec-audit-source",    // Seki
	"sec-audit-deps",      // Depsy
	"secured-renovacy",    // Renovacy
}

// botRosterRank ranks an example name ("<bot-id>/main.bot") by its
// position in botRosterOrder; unknown bots rank last.
func botRosterRank(name string) int {
	id := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		id = name[:i]
	}
	for idx, b := range botRosterOrder {
		if b == id {
			return idx
		}
	}
	return len(botRosterOrder)
}

func (s *Server) handleLoadExample(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httpError(w, http.StatusBadRequest, "missing example name")
		return
	}

	// Sanitize: allow forward-slash relative paths (e.g. "feature_dev/main.bot")
	// but reject backslashes, leading dots, parent traversal, and
	// absolute paths. Must end in an accepted workflow extension.
	if strings.Contains(name, "\\") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "/") || !workflowfile.IsWorkflowFile(name) {
		httpError(w, http.StatusBadRequest, "invalid example name")
		return
	}
	cleaned := path.Clean(name)
	if cleaned != name || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
		httpError(w, http.StatusBadRequest, "invalid example name")
		return
	}

	// Try on-disk first (lets a project's <ExamplesDir>/<name>
	// override an embedded recipe of the same basename), then fall
	// back to the binary-embedded recipe set (examples/embed.go).
	var data []byte
	if dir := s.cfg.ExamplesDir; dir != "" {
		if d, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
			data = d
		}
	}
	if data == nil {
		if d, ok := bots.Get(name); ok {
			data = d
		}
	}
	if data == nil {
		httpError(w, http.StatusNotFound, "example not found: %s", name)
		return
	}

	// Parse and return the document + source.
	pr := parser.Parse(name, string(data))
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}

	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}

	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}

	writeJSON(w, struct {
		Source      string          `json:"source"`
		Document    json.RawMessage `json:"document"`
		Diagnostics []string        `json:"diagnostics,omitempty"`
	}{
		Source:      string(data),
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
	})
}

// --- Helpers ---

func readJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MB max
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// decodeJSON reads+unmarshals the request body into *dst, writing a 400
// "invalid request: %v" on failure. Returns true on success, false if it
// already wrote an error response. Intended for the dominant handler-boilerplate
// pattern; handlers that emit a different status/message, use httpErrorFor, or
// do extra validation between decode and error should keep the explicit form.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	if err := readJSON(r, dst); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return false
	}
	return true
}

// IsAllowedOrigin reports whether the given Origin header value matches the
// loopback set the studio server accepts. It is exposed as a method so test
// code (and a future config flag) can extend the allowlist without rewriting
// every handler. Empty Origin (same-origin request, curl, etc.) is allowed
// because the browser CORS layer is not involved in that case.
func (s *Server) isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	for _, allowed := range s.allowedOrigins() {
		if origin == allowed {
			return true
		}
	}
	return false
}

// isAllowedOriginReq is the request-aware origin check used by the HTTP CORS
// path (requireSafeOrigin, reflectAllowedOrigin, the OPTIONS preflight). It
// accepts, in order:
//   - an empty Origin (non-browser caller: curl, server-to-server),
//   - a same-origin request — the SPA dialing the host that served it. This
//     is what makes the deployed/cloud studio work behind any proxy or on
//     any public host WITHOUT configuring its URL, and mirrors the WebSocket
//     upgrader's sameOrigin policy (see hub.go). Without it, every
//     state-changing POST from a non-loopback studio is rejected with 403,
//     while reads (no Origin) and the WS (already same-origin-aware) work —
//     the exact asymmetry that broke "Dispatch existing board items" in prod.
//   - an Origin in the static allowlist (loopback, wails, configured
//     PublicURL) — covers proxies that rewrite the Host header so the
//     same-origin check above can't fire.
func (s *Server) isAllowedOriginReq(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if sameOrigin(origin, r) {
		return true
	}
	return s.isAllowedOrigin(origin)
}

func (s *Server) allowedOrigins() []string {
	origins := []string{
		fmt.Sprintf("http://localhost:%d", s.cfg.Port),
		fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port),
		fmt.Sprintf("http://[::1]:%d", s.cfg.Port),
	}
	// Desktop mode: the studio SPA is hosted on the Wails AssetServer
	// (wails:// on Mac/Linux, http://wails.localhost on Windows) so that
	// `window.go.main.App.*` bindings + `/wails/runtime.js` injection are
	// available. HTTP API calls reach the local server via Wails' reverse
	// proxy (which rewrites Origin to the loopback target), but the
	// studio's WebSocket clients dial the local server DIRECTLY (Wails'
	// AssetServer returns 501 on WS upgrade). The dialer therefore arrives
	// with the SPA's true origin in the upgrade handshake; without these
	// entries the upgrader's CheckOrigin would reject every cross-origin
	// WS handshake from the desktop window. Token-bearing requests are
	// already authenticated by the auth middleware; origin allow-listing
	// is defense-in-depth.
	origins = append(origins,
		"wails://wails",
		"http://wails.localhost",
	)
	// Cloud / proxied deployments: the studio SPA is served from (and dials)
	// the operator's public host, not loopback. The configured PublicURL is
	// that origin; including it lets requests survive even when a reverse
	// proxy rewrites the Host header (so the same-origin check in
	// isAllowedOriginReq can't match). Normalised to scheme://host — the
	// shape a browser Origin header carries (no path, no trailing slash).
	if s.cfg.PublicURL != "" {
		if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Scheme != "" && u.Host != "" {
			origins = append(origins, u.Scheme+"://"+u.Host)
		}
	}
	return origins
}

// reflectAllowedOrigin sets ACAO to the request's Origin if (and only if) it
// is in the allowlist. Callers should always set Vary: Origin so caches don't
// poison the response across origins.
func (s *Server) reflectAllowedOrigin(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && s.isAllowedOriginReq(r) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONFor is the request-aware variant of writeJSON: it also reflects an
// allowlisted Origin header so legitimate browser callers receive ACAO.
func (s *Server) writeJSONFor(w http.ResponseWriter, r *http.Request, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	msg := fmt.Sprintf(format, args...)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// httpErrorFor is the request-aware variant: reflects allowlisted Origin so
// browser code can read the error body when same-origin or loopback.
func (s *Server) httpErrorFor(w http.ResponseWriter, r *http.Request, code int, format string, args ...interface{}) {
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	w.WriteHeader(code)
	msg := fmt.Sprintf(format, args...)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requireSafeOrigin gates state-changing endpoints. Any request whose Origin
// header is set and not in the allowlist is rejected with 403 BEFORE the
// handler runs — preventing a malicious page in another tab from POSTing
// into the local studio's filesystem-write endpoints. Same-origin and
// non-browser callers (no Origin header) pass through.
func (s *Server) requireSafeOrigin(w http.ResponseWriter, r *http.Request) bool {
	if s.isAllowedOriginReq(r) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "cross-origin request rejected: origin not allowed (must be same-origin, loopback, or the configured public URL)",
	})
	return false
}

// safePath resolves relPath against WorkDir and ensures the result stays within
// WorkDir AFTER symlink resolution. The previous implementation used only
// filepath.Abs + prefix check, which lets a symlink at any depth in the
// workdir point at /etc, /home/$USER/.ssh, etc. — combined with the
// unauthenticated /api/files/open and /api/files/save endpoints, that gave
// any caller on an allowlisted origin (or the same machine, before B5) a
// path-traversal primitive.
//
// Strategy:
//  1. Compute the workdir's canonical (symlink-resolved) absolute path once;
//     use it as the containment root.
//  2. Resolve the requested path's canonical form. If the file does not yet
//     exist (legitimate Save case for new files), resolve the longest
//     existing ancestor and append the remaining components. We refuse the
//     path if any existing ancestor is itself a symlink that escapes the
//     root, OR if the final composed path is not under the root.
//  3. As a defence-in-depth on Save, refuse if the immediate parent
//     directory or any intermediate path component is a symlink — a
//     pre-planted symlink at parent dir would otherwise let WriteFile
//     follow it through.
func (s *Server) safePath(relPath string) (string, error) {
	// Snapshot WorkDir under the read lock so a concurrent
	// /api/projects/switch can't intersect baseAbs and baseReal
	// derivations against two different roots. Without the snapshot,
	// the containment check below could be satisfied by an OLD workdir
	// while the actual write lands under the NEW one — or vice versa.
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	return safePathWithin(workDir, relPath)
}

// safePathWithin resolves relPath against base with the same strict,
// symlink-aware containment used by Save: the returned absolute path is
// guaranteed to live inside base after resolving symlinks on the longest
// existing prefix. It is the single audited path-traversal boundary shared
// by the studio-workdir Save (safePath) and the run-worktree file editor
// (handleGetRunFileContent / handleSaveRunFileContent), so a fix here covers
// both surfaces.
func safePathWithin(base, relPath string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("no working directory configured")
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("workdir abs: %w", err)
	}
	baseReal, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", fmt.Errorf("workdir resolve: %w", err)
	}

	// Compute the requested absolute path (without symlink resolution yet).
	// Idempotent on absolute inputs: handleResumeRun passes the
	// runMeta.FilePath value, which was already canonicalised at launch
	// time. Naively re-joining baseAbs with an already-absolute path
	// duplicates the workdir prefix (e.g. "/foo/bar" joined with "/foo/bar/x"
	// yields "/foo/bar/foo/bar/x"). The containment check below remains
	// the security boundary, so taking absolute inputs as-is is safe —
	// any path that escapes baseReal will still be rejected.
	var abs string
	if filepath.IsAbs(relPath) {
		abs = filepath.Clean(relPath)
	} else {
		abs = filepath.Join(baseAbs, filepath.Clean("/"+relPath))
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return "", err
	}

	// Resolve symlinks for the longest existing prefix; keep the trailing
	// not-yet-existing components verbatim. This supports legitimate Save of
	// a brand-new file inside an existing directory.
	resolved, err := evalSymlinksLongestPrefix(abs)
	if err != nil {
		return "", err
	}

	if !pathContains(baseReal, resolved) {
		return "", fmt.Errorf("path escapes working directory")
	}
	return resolved, nil
}

// pathContains reports whether target is base or a path under base, after
// canonicalisation. Both paths must be absolute.
func pathContains(base, target string) bool {
	if base == target {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(base, sep) {
		base += sep
	}
	return strings.HasPrefix(target, base)
}

// evalSymlinksLongestPrefix walks abs from the root, finding the longest
// existing prefix and resolving it via filepath.EvalSymlinks; it then
// re-attaches any remaining (not-yet-existing) trailing components. If any
// existing component on the path is a symlink, EvalSymlinks resolves it —
// callers that want to refuse all symlinks in the chain (e.g. Save) should
// gate via a separate check. Returns the canonicalised absolute path.
func evalSymlinksLongestPrefix(abs string) (string, error) {
	// If the full path exists, resolve it directly.
	if _, err := os.Lstat(abs); err == nil {
		return filepath.EvalSymlinks(abs)
	}
	// Walk up until we find an existing ancestor.
	dir, leaf := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs, nil
	}
	parent, err := evalSymlinksLongestPrefix(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, leaf), nil
}

// --- File management handlers ---

func (s *Server) handleListFiles(w http.ResponseWriter, _ *http.Request) {
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	if workDir == "" {
		writeJSON(w, listFilesResponse{Files: []fileEntry{}})
		return
	}
	var files []fileEntry
	filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSkippedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isWorkflowFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(workDir, path)
		files = append(files, fileEntry{Name: rel, Size: info.Size()})
		return nil
	})
	if files == nil {
		files = []fileEntry{}
	}
	writeJSON(w, listFilesResponse{Files: files})
}

func (s *Server) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req openFileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		// Embedded-recipe fallback: the bot picker sets currentFilePath
		// to "bots/<name>" (legacy "examples/<name>") after loading a
		// binary-embedded recipe (see studio Toolbar handlePickFile).
		// When the project has no matching on-disk file, every later
		// flow that re-reads via /files/open (LaunchView's pre-launch
		// document fetch, the file watcher, hot-reload) would 404
		// without this fallback. Strip the prefix and resolve the
		// remainder (e.g. "feature_dev/main.bot") against the embed.
		for _, prefix := range []string{"bots/", "examples/"} {
			if rest := strings.TrimPrefix(req.Path, prefix); rest != req.Path {
				if embedded, ok := bots.Get(rest); ok {
					data = embedded
					err = nil
				}
				break
			}
		}
	}
	if err != nil {
		httpError(w, http.StatusNotFound, "file not found: %s", req.Path)
		return
	}
	pr := parser.Parse(req.Path, string(data))
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}
	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}
	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	writeJSON(w, struct {
		Source      string          `json:"source"`
		Document    json.RawMessage `json:"document"`
		Diagnostics []string        `json:"diagnostics,omitempty"`
		Path        string          `json:"path"`
	}{
		Source:      string(data),
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
		Path:        req.Path,
	})
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req saveFileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !workflowfile.IsWorkflowFile(req.Path) {
		httpError(w, http.StatusBadRequest, "filename must end in .bot")
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}
	source := unparse.Unparse(f)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, "cannot create directory: %v", err)
		return
	}
	if s.watcher != nil {
		s.watcher.IgnorePath(absPath)
	}
	if err := os.WriteFile(absPath, []byte(source), 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, "write error: %v", err)
		return
	}
	writeJSON(w, saveFileResponse{Path: req.Path, Source: source})
}
