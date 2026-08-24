// Package config loads iterion runtime configuration from environment
// variables and an optional YAML file. Precedence: env > yaml > compiled
// defaults. The loader is safe to call from server, runner, or CLI
// entry points; it never reads ITERION_* vars not listed in the schema
// (see plan §E), so existing wiring (ITERION_DEFAULT_BACKEND, etc.) is
// untouched.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Mode selects the persistence/dispatch backend.
type Mode string

const (
	ModeLocal Mode = "local"
	ModeCloud Mode = "cloud"
)

// LogFormat selects between the human-readable console format and
// structured JSON.
type LogFormat string

const (
	LogFormatHuman LogFormat = "human"
	LogFormatJSON  LogFormat = "json"
)

// Config is the parsed, validated runtime configuration.
//
// Field grouping mirrors the YAML sections so a 1:1 yaml.Marshal of a
// loaded Config is a usable config file. Zero values are treated as
// "use the default"; explicit empties (e.g. an env var set to "") win
// over defaults via the per-field defaultedXxx flags carried by the
// loader.
type Config struct {
	Mode Mode `yaml:"mode"`

	NATS    NATSConfig    `yaml:"nats"`
	Mongo   MongoConfig   `yaml:"mongo"`
	Redis   RedisConfig   `yaml:"redis"`
	S3      S3Config      `yaml:"s3"`
	Runner  RunnerConfig  `yaml:"runner"`
	Server  ServerConfig  `yaml:"server"`
	Metrics MetricsConfig `yaml:"metrics"`
	Log     LogConfig     `yaml:"log"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	Auth    AuthConfig    `yaml:"auth"`
	Alerts  AlertsConfig  `yaml:"alerts"`
	WebPush WebPushConfig `yaml:"webpush"`
}

// WebPushConfig holds the VAPID identity for browser push notifications
// (a run pausing on a human form, run outcomes). The keypair is the sender
// identity browsers pin at subscribe time, so every server replica must
// share ONE pair; rotating it invalidates all stored subscriptions.
// Generate with `iterion server webpush-keys`. The feature is enabled iff
// both keys are set.
type WebPushConfig struct {
	// VAPIDPublicKey is the base64url-encoded P-256 public key, exposed to
	// browsers via server_info (public by design).
	VAPIDPublicKey string `yaml:"vapid_public_key"`
	// VAPIDPrivateKey is the matching private key (a secret).
	VAPIDPrivateKey string `yaml:"vapid_private_key"`
	// Subscriber is the VAPID contact (mailto: or https: URL) push
	// services may use to reach the operator.
	Subscriber string `yaml:"subscriber"`
}

// Enabled reports whether web push can be served (both keys present).
func (w WebPushConfig) Enabled() bool {
	return w.VAPIDPublicKey != "" && w.VAPIDPrivateKey != ""
}

// RedisConfig points the server at a Valkey/Redis used to share ephemeral
// state across replicas (forge OAuth/CSRF state, board-MCP run tokens, auth
// rate-limit buckets). Two connection modes:
//   - Sentinel HA: SentinelAddrs + MasterName (the cloud/prod posture).
//   - Single node: URL (redis://host:port), for dev/local.
//
// When NEITHER SentinelAddrs nor URL is set, the server falls back to the
// in-process in-memory stores (single-replica / `iterion studio`).
type RedisConfig struct {
	// URL is a single-node connection string (redis://[:pass@]host:port[/db]).
	URL string `yaml:"url"`
	// SentinelAddrs is a comma-joined list of host:port sentinel endpoints;
	// when set (with MasterName) the client runs in HA failover mode.
	SentinelAddrs []string `yaml:"sentinel_addrs"`
	// MasterName is the Sentinel monitored master name (required with
	// SentinelAddrs).
	MasterName string `yaml:"master_name"`
	// Password authenticates to the Valkey data nodes.
	Password string `yaml:"password"`
	// SentinelPassword authenticates to the sentinels (optional; defaults to
	// Password when empty).
	SentinelPassword string `yaml:"sentinel_password"`
}

// Enabled reports whether a distributed Redis/Valkey backend is configured.
func (r RedisConfig) Enabled() bool {
	return len(r.SentinelAddrs) > 0 || strings.TrimSpace(r.URL) != ""
}

// AlertsConfig configures run-health alerting (stall, budget, failure).
// Delivery is fan-out: a generic incoming webhook (Slack/Discord-shaped)
// plus, in the desktop app, an in-window Wails notification. Browser
// studio sessions always receive alerts as toasts + a notification dot
// regardless of these settings.
type AlertsConfig struct {
	Webhook WebhookAlertConfig `yaml:"webhook"`
	Desktop DesktopAlertConfig `yaml:"desktop"`
	// StallTimeout is the no-activity window after which a running run
	// is flagged as stalled. Default 5m; "0" disables stall alerts.
	StallTimeout time.Duration `yaml:"stall_timeout"`
}

// WebhookAlertConfig points at a generic incoming webhook. Empty URL
// disables webhook delivery. The URL is a secret and is never logged.
type WebhookAlertConfig struct {
	URL string `yaml:"url"`
}

// DesktopAlertConfig toggles in-window desktop notifications (desktop
// app only; no effect on the headless server).
type DesktopAlertConfig struct {
	Enabled bool `yaml:"enabled"`
}

// AuthConfig groups multitenant auth settings: JWT signing, secrets
// master key, bootstrap admin, signup policy, and configured OIDC
// providers (Google, GitHub, generic).
//
// Required in cloud mode; ignored in local mode (the studio process
// is implicitly trusted to its TTY user).
type AuthConfig struct {
	// JWTSecret is a base64-encoded HS256 signing key (>=32 bytes).
	// Required in cloud mode.
	JWTSecret string `yaml:"jwt_secret"`

	// SecretsKey is a base64-encoded AES-256-GCM master key (32
	// bytes). Used by pkg/secrets to seal API keys / OAuth blobs at
	// rest. Required in cloud mode.
	SecretsKey string `yaml:"secrets_key"`

	// AccessTTL is the lifetime of an access JWT. Default 15m.
	AccessTTL time.Duration `yaml:"access_ttl"`

	// RefreshTTL is the lifetime of a refresh token. Default 720h
	// (30 days). Refresh tokens rotate on every use.
	RefreshTTL time.Duration `yaml:"refresh_ttl"`

	// BootstrapAdminEmail, when set on first boot of an empty users
	// collection, creates a super-admin account with that email and
	// a randomly generated one-time password printed to the server
	// log. The user is required to change it on first login.
	BootstrapAdminEmail string `yaml:"bootstrap_admin_email"`

	// BootstrapAdminPassword, when set (typically from a k8s secret via
	// ITERION_BOOTSTRAP_ADMIN_PASSWORD), makes the bootstrap super-admin
	// DECLARATIVE: the account is created active with this password and, on
	// every boot, reconciled to it (active super-admin, password reset only on
	// drift). The secret is authoritative — rotate by updating it + restart;
	// UI password changes to this account revert on restart by design. Never
	// logged. When empty, the legacy one-time temp-password flow applies.
	BootstrapAdminPassword string `yaml:"bootstrap_admin_password"`

	// SignupMode controls who may create new users without an
	// invitation. "invite_only" (default) — registration requires a
	// matching invitation token. "open" — anyone can register; first
	// login lands them in their own personal team.
	SignupMode string `yaml:"signup_mode"`

	// PublicURL is the externally-reachable origin of the server,
	// used to build OIDC redirect URIs (e.g. https://iterion.example).
	// Required when any OIDC provider is enabled.
	PublicURL string `yaml:"public_url"`

	// CookieDomain narrows the auth cookie's Domain attribute when
	// the SPA is served from a different host than the API (rare).
	// Empty means host-only cookie (recommended).
	CookieDomain string `yaml:"cookie_domain"`

	// CookieSecure forces the Secure flag on auth cookies. Defaults
	// to true; only set false for HTTP local dev.
	CookieSecure bool `yaml:"cookie_secure"`

	// TrustedAutoLinkProviders is the operator allowlist of OIDC provider
	// slugs whose verified email may auto-link a fresh SSO identity onto an
	// existing password-account user (no explicit "link" step). Empty
	// (default) = no auto-link: such a login returns a 409 so the SPA prompts
	// the user to link from settings. Only affects GLOBAL providers (google,
	// github, the deployment "sso"); per-org Keycloak rows have their own
	// (Phase-3) per-row opt-in. Set via ITERION_AUTH_TRUSTED_AUTO_LINK_PROVIDERS
	// (CSV).
	TrustedAutoLinkProviders []string `yaml:"trusted_auto_link_providers"`

	OIDC OIDCConfig `yaml:"oidc"`

	// OAuthForfait carries the per-deployment OAuth client ids used
	// to refresh user-provided Claude Code / Codex subscription
	// tokens. Empty disables refresh of the corresponding kind.
	OAuthForfait OAuthForfaitConfig `yaml:"oauth_forfait"`
}

// OAuthForfaitConfig carries OAuth client ids the server uses when
// refreshing user-supplied subscription tokens. Per memory
// `feedback_no_anthropic_oauth_in_third_party.md`, this code path is
// for the official CLI surface only — the claw backend never reaches
// here.
type OAuthForfaitConfig struct {
	AnthropicClientID string `yaml:"anthropic_client_id"`
	CodexClientID     string `yaml:"codex_client_id"`
}

// OIDCConfig holds the three supported SSO providers. Each is opt-in
// via the Enabled flag; client_id/secret default to env override.
type OIDCConfig struct {
	Google  OIDCProviderConfig `yaml:"google"`
	GitHub  OIDCProviderConfig `yaml:"github"`
	Generic OIDCProviderConfig `yaml:"generic"`
	// GitHubUngrantedPolicy controls a GitHub login that matches no
	// allow-listed team while team-gating is active: "refuse" (default,
	// reject) or "submitter" (admit teamless → marketplace-submit only).
	// Maps to ITERION_OIDC_GITHUB_UNGRANTED_POLICY.
	GitHubUngrantedPolicy string `yaml:"github_ungranted_policy"`
}

// OIDCProviderConfig is the per-provider config block. For Google the
// IssuerURL defaults to https://accounts.google.com; for GitHub the
// IssuerURL is unused (GitHub is OAuth2 not OIDC). For Generic the
// operator must provide IssuerURL pointing to a discovery doc.
type OIDCProviderConfig struct {
	Enabled      bool     `yaml:"enabled"`
	IssuerURL    string   `yaml:"issuer_url"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
	// DisplayName is shown on the SPA login page button.
	DisplayName string `yaml:"display_name"`
}

// SandboxConfig is the global sandbox default. The empty string means
// "no sandbox" — the factory will pick the noop driver. Workflows
// that declare their own `sandbox:` block override this value; this
// is the lowest-precedence fallback per the design plan.
//
// See pkg/sandbox/precedence.go for resolution rules and
// .plans/on-va-tudier-la-snappy-lemon.md §0 for the user-facing
// activation model.
type SandboxConfig struct {
	// Default is one of "" (no sandbox), "none" (explicit opt-out
	// across all workflows — useful when you want every workflow to
	// have to opt back in explicitly), or "auto" (every workflow
	// reads .devcontainer/devcontainer.json by default). Phase 0
	// accepts these three; "inline" requires a block body which the
	// CLI cannot express.
	Default string `yaml:"default"`

	// HostState selects whether the host's `~/.iterion` (run store)
	// and `~/.claude` (Claude Code OAuth + sessions) are
	// auto-mounted into the sandbox. "" (default → "auto") | "auto" |
	// "none". The "none" value is the recommended posture for
	// cloud / multi-tenant deploys (it avoids leaking host OAuth
	// into shared runners).
	HostState string `yaml:"host_state"`

	// Override is the CLI-strength sandbox mode ("" | "none" | "auto")
	// applied on top of everything a workflow declares — same precedence
	// tier as `iterion run --sandbox`, where "none" is the
	// non-overridable opt-out. Meant for the cloud runner
	// (ITERION_SANDBOX_OVERRIDE=none): the runner pod already IS the
	// isolation boundary and ships the toolchain (devbox), so a bot's
	// inline `sandbox:` block — written for local runs — must not spawn
	// a sibling sandbox pod there. Default (lower-precedence) knobs
	// cannot express this because a workflow block outranks them.
	Override string `yaml:"override"`
}

// NATSConfig holds the NATS JetStream connection + stream/bucket names,
// plus the work-queue tuning knobs. The tuning fields default to zero =
// inherit the natsq defaults (MaxAckPending 256, AckWait 10m, MaxDeliver 8,
// MaxAge 24h, DLQMaxAge 7d, MaxPayload server-negotiated). Server and runner
// deployments must be fed the same values — whichever connects last
// re-pins the shared stream/consumer.
type NATSConfig struct {
	URL       string `yaml:"url"`
	Stream    string `yaml:"stream"`
	KVBucket  string `yaml:"kv_bucket"`
	DLQStream string `yaml:"dlq_stream"`

	MaxAckPending int           `yaml:"max_ack_pending"` // fleet-wide in-flight (delivered-unacked) cap on the shared consumer
	AckWait       time.Duration `yaml:"ack_wait"`        // per-delivery ack window before redelivery
	MaxDeliver    int           `yaml:"max_deliver"`     // delivery attempts before the message parks in the DLQ
	MaxAge        time.Duration `yaml:"max_age"`         // run-stream retention
	DLQMaxAge     time.Duration `yaml:"dlq_max_age"`     // DLQ retention
	MaxPayload    int           `yaml:"max_payload"`     // publish-size guard override (bytes)
}

// MongoConfig holds the Mongo connection + DB + events TTL.
type MongoConfig struct {
	URI           string `yaml:"uri"`
	DB            string `yaml:"db"`
	EventsTTLDays int    `yaml:"events_ttl_days"`
}

// S3Config holds the S3/MinIO connection settings.
type S3Config struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	UsePathStyle    bool   `yaml:"use_path_style"`
}

// RunnerConfig holds runner-specific tuning.
type RunnerConfig struct {
	WorkDir     string        `yaml:"workdir"`
	Concurrency int           `yaml:"concurrency"`
	Heartbeat   time.Duration `yaml:"heartbeat"`
	LockTTL     time.Duration `yaml:"lock_ttl"`
	// DrainMode governs SIGTERM handling on a deploy/node-drain:
	// "complete" (default, lame-duck — finish the in-flight run before
	// exiting) or "interrupt" (cancel + checkpoint for auto-resume).
	DrainMode string `yaml:"drain_mode"`
	// DrainTimeout is the lame-duck ceiling — the longest the pod waits for
	// its in-flight run before capping it for a checkpoint-resume. The k8s
	// terminationGracePeriodSeconds must be >= this + margin.
	DrainTimeout time.Duration `yaml:"drain_timeout"`
	// SchemaMismatchDelay is the redelivery delay applied when this runner
	// rejects a queue message whose schema version it does not recognise
	// (mixed fleet during a rolling schema bump). It must be long enough
	// that the MaxDeliver budget stretches over a rolling restart of the
	// runner Deployment — an immediate Nak burns it in seconds and parks
	// the message on the DLQ for manual replay (issue #481). Fleets with a
	// slow lame-duck turnover (long DrainTimeout) should raise it.
	SchemaMismatchDelay time.Duration `yaml:"schema_mismatch_delay"`
}

// ServerConfig holds server-specific settings.
type ServerConfig struct {
	// ShutdownDelay is the lame-duck window on SIGTERM: /readyz answers
	// 503 for this long while the listener still accepts, giving the
	// endpoints controller time to remove the pod from the Service before
	// its socket closes. Set it to 0 to shut down immediately.
	ShutdownDelay time.Duration `yaml:"shutdown_delay"`

	// ShutdownTeardown is what the process spends AFTER the lame-duck
	// window: draining in-flight runs, then letting in-flight HTTP
	// requests finish. It is the ceiling on an operator's own work — a
	// long upload or a streaming endpoint is cut when it expires — so it
	// is a knob, not a constant.
	//
	// k8s terminationGracePeriodSeconds must cover
	// preStop + ShutdownDelay + ShutdownTeardown.
	ShutdownTeardown time.Duration `yaml:"shutdown_teardown"`
}

// MetricsConfig holds the Prometheus metrics endpoint port.
type MetricsConfig struct {
	Port int `yaml:"port"`
}

// LogConfig holds log format + level. format default differs between
// CLI ("human") and server/runner ("json"); the loader does not
// auto-detect — callers pass DefaultLogFormat in LoadOptions.
type LogConfig struct {
	Format LogFormat `yaml:"format"`
	Level  string    `yaml:"level"`
}

// Defaults returns the compiled-in default Config (independent of env/yaml).
// CLI default for log format = human; server/runner override via
// LoadOptions.DefaultLogFormat.
func Defaults() Config {
	return Config{
		Mode: ModeLocal,
		NATS: NATSConfig{
			Stream:    "ITERION_RUNS",
			KVBucket:  "iterion-run-locks",
			DLQStream: "ITERION_RUNS_DLQ",
		},
		Mongo: MongoConfig{
			DB:            "iterion",
			EventsTTLDays: 90,
		},
		S3: S3Config{
			Region:       "us-east-1",
			Bucket:       "iterion-artifacts",
			UsePathStyle: true,
		},
		Runner: RunnerConfig{
			WorkDir:      "/tmp/iterion",
			Concurrency:  1,
			Heartbeat:    20 * time.Second,
			LockTTL:      60 * time.Second,
			DrainMode:    "complete",
			DrainTimeout: 8 * time.Hour,
			// Mirrors natsq.SchemaMismatchNakDelay (kept literal here so
			// pkg/config stays free of the NATS client dependency).
			SchemaMismatchDelay: 30 * time.Second,
		},
		Server: ServerConfig{
			ShutdownDelay:    5 * time.Second,
			ShutdownTeardown: 30 * time.Second,
		},
		Metrics: MetricsConfig{
			Port: 9090,
		},
		Log: LogConfig{
			Format: LogFormatHuman,
			Level:  "info",
		},
		Alerts: AlertsConfig{
			StallTimeout: 5 * time.Minute,
		},
		Auth: AuthConfig{
			AccessTTL:    15 * time.Minute,
			RefreshTTL:   30 * 24 * time.Hour,
			SignupMode:   "invite_only",
			CookieSecure: true,
			OIDC: OIDCConfig{
				Google: OIDCProviderConfig{
					IssuerURL:   "https://accounts.google.com",
					Scopes:      []string{"openid", "email", "profile"},
					DisplayName: "Google",
				},
				GitHub: OIDCProviderConfig{
					Scopes:      []string{"read:user", "user:email"},
					DisplayName: "GitHub",
				},
				Generic: OIDCProviderConfig{
					Scopes:      []string{"openid", "email", "profile"},
					DisplayName: "SSO",
				},
			},
			// The Anthropic forfait client id defaults to the public
			// Claude Code OAuth client (a public PKCE client, not a
			// secret) so the browser connect flow works without any
			// per-deployment config; env/yaml still override it.
			OAuthForfait: OAuthForfaitConfig{
				AnthropicClientID: secrets.DefaultAnthropicOAuthClientID,
			},
		},
	}
}

// LoadOptions tunes loader behaviour.
type LoadOptions struct {
	// YAMLPath, if non-empty, is read and merged before env vars (env
	// wins). A missing file is an error; a malformed file is an error.
	YAMLPath string
	// DefaultLogFormat overrides the compiled default ("human") to
	// "json" for server/runner entry points. Env still wins.
	DefaultLogFormat LogFormat
}

// Load builds a Config from defaults <- yaml <- env. Validation is
// strict: cloud mode requires NATS+Mongo+S3 to be reachable-by-config,
// not just by env-set; a missing field returns an error before any IO.
func Load(opts LoadOptions) (Config, error) {
	cfg := Defaults()
	if opts.DefaultLogFormat != "" {
		cfg.Log.Format = opts.DefaultLogFormat
	}

	if opts.YAMLPath != "" {
		if err := loadYAML(opts.YAMLPath, &cfg); err != nil {
			return cfg, fmt.Errorf("config: yaml: %w", err)
		}
	}
	if err := loadEnv(&cfg); err != nil {
		return cfg, fmt.Errorf("config: env: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config: validate: %w", err)
	}
	return cfg, nil
}

// Validate enforces invariants (mode-specific required fields, enum
// membership). Returns the first failure as an error suitable for
// surfacing on CLI startup.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeCloud:
	default:
		return fmt.Errorf("ITERION_MODE %q invalid (want local|cloud)", c.Mode)
	}

	if c.Log.Format != LogFormatHuman && c.Log.Format != LogFormatJSON {
		return fmt.Errorf("ITERION_LOG_FORMAT %q invalid (want human|json)", c.Log.Format)
	}
	switch c.Log.Level {
	case "error", "warn", "info", "debug", "trace":
	default:
		return fmt.Errorf("ITERION_LOG_LEVEL %q invalid (want error|warn|info|debug|trace)", c.Log.Level)
	}

	if err := checkShutdownDelay(c.Server.ShutdownDelay); err != nil {
		return err
	}
	if err := checkShutdownTeardown(c.Server.ShutdownTeardown); err != nil {
		return err
	}
	if c.Metrics.Port < 1 || c.Metrics.Port > 65535 {
		return fmt.Errorf("ITERION_METRICS_PORT %d invalid (want 1-65535)", c.Metrics.Port)
	}

	if c.Alerts.StallTimeout < 0 {
		return fmt.Errorf("ITERION_ALERTS_STALL_TIMEOUT %s invalid (want >= 0)", c.Alerts.StallTimeout)
	}

	if c.Mode == ModeCloud {
		if c.NATS.URL == "" {
			return fmt.Errorf("ITERION_NATS_URL required when mode=cloud")
		}
		if c.NATS.Stream == "" {
			return fmt.Errorf("ITERION_NATS_STREAM must not be empty")
		}
		if c.NATS.KVBucket == "" {
			return fmt.Errorf("ITERION_NATS_KV_BUCKET must not be empty")
		}
		if c.NATS.DLQStream == "" {
			return fmt.Errorf("ITERION_NATS_DLQ_STREAM must not be empty")
		}
		if c.NATS.MaxAckPending < 0 {
			return fmt.Errorf("ITERION_NATS_MAX_ACK_PENDING %d invalid (want >= 0; 0 inherits the default)", c.NATS.MaxAckPending)
		}
		if c.NATS.MaxDeliver < 0 {
			return fmt.Errorf("ITERION_NATS_MAX_DELIVER %d invalid (want >= 0; 0 inherits the default)", c.NATS.MaxDeliver)
		}
		if c.NATS.AckWait < 0 {
			return fmt.Errorf("ITERION_NATS_ACK_WAIT %s invalid (want >= 0; 0 inherits the default)", c.NATS.AckWait)
		}
		if c.NATS.MaxAge < 0 {
			return fmt.Errorf("ITERION_NATS_MAX_AGE %s invalid (want >= 0; 0 inherits the default)", c.NATS.MaxAge)
		}
		if c.NATS.DLQMaxAge < 0 {
			return fmt.Errorf("ITERION_NATS_DLQ_MAX_AGE %s invalid (want >= 0; 0 inherits the default)", c.NATS.DLQMaxAge)
		}
		if c.NATS.MaxPayload < 0 {
			return fmt.Errorf("ITERION_NATS_MAX_PAYLOAD %d invalid (want >= 0; 0 inherits the default)", c.NATS.MaxPayload)
		}
		if c.Mongo.URI == "" {
			return fmt.Errorf("ITERION_MONGO_URI required when mode=cloud")
		}
		if c.Mongo.DB == "" {
			return fmt.Errorf("ITERION_MONGO_DB must not be empty")
		}
		if c.Mongo.EventsTTLDays < 0 {
			return fmt.Errorf("ITERION_MONGO_EVENTS_TTL_DAYS %d invalid (want >= 0)", c.Mongo.EventsTTLDays)
		}
		if c.S3.Endpoint == "" {
			return fmt.Errorf("ITERION_S3_ENDPOINT required when mode=cloud")
		}
		if c.S3.Bucket == "" {
			return fmt.Errorf("ITERION_S3_BUCKET must not be empty")
		}
		// Access key + secret are conditionally required (IRSA can fill
		// them from the pod environment); we don't enforce them here.
		if c.Auth.JWTSecret == "" {
			return fmt.Errorf("ITERION_JWT_SECRET required when mode=cloud (base64 of >=32 random bytes)")
		}
		if c.Auth.SecretsKey == "" {
			return fmt.Errorf("ITERION_SECRETS_KEY required when mode=cloud (base64 of 32 random bytes)")
		}
		switch c.Auth.SignupMode {
		case "invite_only", "open":
		default:
			return fmt.Errorf("ITERION_SIGNUP_MODE %q invalid (want invite_only|open)", c.Auth.SignupMode)
		}
		if c.Auth.AccessTTL <= 0 {
			return fmt.Errorf("ITERION_ACCESS_TTL %s invalid (want > 0)", c.Auth.AccessTTL)
		}
		if c.Auth.RefreshTTL <= 0 {
			return fmt.Errorf("ITERION_REFRESH_TTL %s invalid (want > 0)", c.Auth.RefreshTTL)
		}
		// Public URL is required only when at least one OIDC provider
		// is enabled — without it we can't form a redirect URI.
		if (c.Auth.OIDC.Google.Enabled || c.Auth.OIDC.GitHub.Enabled || c.Auth.OIDC.Generic.Enabled) && c.Auth.PublicURL == "" {
			return fmt.Errorf("ITERION_PUBLIC_URL required when an OIDC provider is enabled")
		}
		if c.Auth.OIDC.Generic.Enabled && c.Auth.OIDC.Generic.IssuerURL == "" {
			return fmt.Errorf("ITERION_OIDC_GENERIC_ISSUER_URL required when generic OIDC is enabled")
		}
	}

	if c.Runner.Concurrency < 1 {
		return fmt.Errorf("ITERION_RUNNER_CONCURRENCY %d invalid (want >= 1)", c.Runner.Concurrency)
	}
	if c.Runner.Heartbeat <= 0 {
		return fmt.Errorf("ITERION_HEARTBEAT_INTERVAL %s invalid (want > 0)", c.Runner.Heartbeat)
	}
	if c.Runner.LockTTL <= 0 {
		return fmt.Errorf("ITERION_LOCK_TTL %s invalid (want > 0)", c.Runner.LockTTL)
	}
	switch c.Runner.DrainMode {
	case "", "complete", "interrupt":
	default:
		return fmt.Errorf("ITERION_RUNNER_DRAIN_MODE %q invalid (want \"complete\" or \"interrupt\")", c.Runner.DrainMode)
	}
	if c.Runner.DrainTimeout < 0 {
		return fmt.Errorf("ITERION_RUNNER_DRAIN_TIMEOUT %s invalid (want >= 0)", c.Runner.DrainTimeout)
	}
	if c.Runner.SchemaMismatchDelay < 0 {
		return fmt.Errorf("ITERION_RUNNER_SCHEMA_MISMATCH_DELAY %s invalid (want >= 0)", c.Runner.SchemaMismatchDelay)
	}

	switch c.Sandbox.Default {
	case "", "none", "auto":
	default:
		return fmt.Errorf("ITERION_SANDBOX_DEFAULT %q invalid (want \"\", \"none\", or \"auto\")", c.Sandbox.Default)
	}

	switch c.Sandbox.HostState {
	case "", "auto", "none":
	default:
		return fmt.Errorf("ITERION_SANDBOX_HOST_STATE %q invalid (want \"\", \"auto\", or \"none\")", c.Sandbox.HostState)
	}

	switch c.Sandbox.Override {
	case "", "none", "auto":
	default:
		return fmt.Errorf("ITERION_SANDBOX_OVERRIDE %q invalid (want \"\", \"none\", or \"auto\")", c.Sandbox.Override)
	}

	return nil
}
