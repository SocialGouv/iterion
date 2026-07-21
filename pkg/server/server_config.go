package server

import (
	"context"
	"embed"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/cloud/orgsweep"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	"github.com/SocialGouv/iterion/pkg/usernotify/webpush"
	"github.com/SocialGouv/iterion/pkg/valkey"
	"github.com/SocialGouv/iterion/pkg/webhooks"
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

	// DesktopTickets persists single-use desktop SSO exchange tickets between
	// the OIDC callback (mint) and /api/auth/desktop/exchange (redeem), which
	// can hit different replicas. Defaults to an in-memory store when nil;
	// the cloud control plane wires a Mongo-backed store.
	DesktopTickets desktopsso.Store

	// WSTickets persists single-use WS tickets between POST /api/ws/ticket
	// (mint) and the WS upgrade (redeem), so a client authenticates the WS
	// with an opaque ?ticket= instead of a long-lived JWT in the URL.
	// Defaults to in-memory; the cloud control plane wires Mongo.
	WSTickets wsticket.Store

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

	// ConfigShares backs the scoped self-service config-share editor. When
	// nil, the server defaults to an in-memory store (local/desktop); cloud
	// wires a persistent (Mongo) store. The routes additionally require
	// GenericSecrets + Sealer (to resolve the repo's forge_token).
	ConfigShares configshare.Store

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

	// PluginSources holds team-scoped, git-hosted org-private plugins
	// (pkg/pluginsource). Non-nil registers /api/teams/:id/plugin-sources;
	// nil answers 501 there. The durable counterpart to a plugin installed
	// into a pod's iterion home, which a restart silently loses.
	PluginSources pluginsource.Store

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

	// StreamSource, when non-nil, replaces the in-process EventBroker /
	// RunLogBuffer machinery for live + historical WS delivery. Cloud
	// mode wires a Mongo change-stream source so the WS handler sees
	// runner-pod writes. ADR-053.
	StreamSource runstream.Source

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

	// PushSubscriptions, NotificationPrefs and NotificationSent wire the
	// user-notification stack (pkg/usernotify): browser Web Push for a run
	// pausing on a human form and for run outcomes. All three non-nil +
	// the VAPID keypair set ⇒ the dispatcher starts, the reconciliation
	// sweep runs (when NotifiableRuns is wired), and the
	// /api/v1/notifications/* routes activate. nil ⇒ feature off.
	PushSubscriptions webpush.SubscriptionStore
	NotificationPrefs usernotify.PrefsStore
	NotificationSent  usernotify.SentStore
	// WebPushVAPIDPublicKey / WebPushVAPIDPrivateKey are the shared VAPID
	// sender identity (public key is exposed via server_info by design);
	// WebPushSubscriber is the VAPID contact (mailto:).
	WebPushVAPIDPublicKey  string
	WebPushVAPIDPrivateKey string
	WebPushSubscriber      string
	// EventsBus, when non-nil, is the shared trigger-event spine the
	// usernotify dispatcher subscribes on (the cloud NATSBus — queue-group
	// delivery ⇒ exactly one replica handles each event). nil → the
	// dispatcher falls back to the local trigger coordinator's in-proc bus.
	EventsBus eventbus.Bus
	// NotifiableRuns is the reconciliation sweep's scan seam (the Mongo
	// store's ListNotifiableRuns). nil → no sweep (bus-only delivery).
	NotifiableRuns usernotify.ListNotifiableRuns

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

	// OrgPurgeSweeper, when set (cloud mode), runs a nightly sweep that
	// hard-purges orgs whose soft-delete grace has elapsed. nil disables it.
	OrgPurgeSweeper *orgsweep.Sweeper

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

	// MaxConcurrentPipelines caps how many ROOT pipelines the run
	// console service runs at once (the cross-run limit
	// `max_parallel_branches` never provided). 0 leaves the
	// ITERION_MAX_CONCURRENT_PIPELINES env default (unlimited if unset).
	// Threaded into runview.WithMaxConcurrentPipelines. Inert in cloud
	// mode (the publisher path bypasses the local gate).
	MaxConcurrentPipelines int

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

	// Redis is the Valkey/Redis client for distributing ephemeral state
	// (forge CSRF state, board-MCP run tokens, auth rate-limit) across
	// replicas. Nil → the in-memory stores are used (local/desktop /
	// single-replica).
	Redis *valkey.Client
}

// ReadinessCheck is the contract /readyz invokes on each external
// dependency. It MUST be cheap (HEAD/ping) and MUST respect the
// supplied context's deadline.
type ReadinessCheck func(ctx context.Context) error
