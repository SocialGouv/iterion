// Package config loads iterion runtime configuration from environment
// variables and an optional YAML file. Precedence: env > yaml > compiled
// defaults. The loader is safe to call from server, runner, or CLI
// entry points; it never reads ITERION_* vars not listed in the schema
// (see plan §E), so existing wiring (ITERION_DEFAULT_BACKEND, etc.) is
// untouched.
package config

import (
	"fmt"
	"time"
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
	S3      S3Config      `yaml:"s3"`
	Runner  RunnerConfig  `yaml:"runner"`
	Server  ServerConfig  `yaml:"server"`
	Metrics MetricsConfig `yaml:"metrics"`
	Log     LogConfig     `yaml:"log"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	Auth    AuthConfig    `yaml:"auth"`
}

// AuthConfig groups multitenant auth settings: JWT signing, secrets
// master key, bootstrap admin, signup policy, and configured OIDC
// providers (Google, GitHub, generic).
//
// Required in cloud mode; ignored in local mode (the editor process
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

	OIDC OIDCConfig `yaml:"oidc"`
}

// OIDCConfig holds the three supported SSO providers. Each is opt-in
// via the Enabled flag; client_id/secret default to env override.
type OIDCConfig struct {
	Google  OIDCProviderConfig `yaml:"google"`
	GitHub  OIDCProviderConfig `yaml:"github"`
	Generic OIDCProviderConfig `yaml:"generic"`
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
}

// NATSConfig holds the NATS JetStream connection + stream/bucket names.
type NATSConfig struct {
	URL       string `yaml:"url"`
	Stream    string `yaml:"stream"`
	KVBucket  string `yaml:"kv_bucket"`
	DLQStream string `yaml:"dlq_stream"`
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
}

// ServerConfig holds server-specific settings (HTTP API + healthz port).
type ServerConfig struct {
	HealthzPort int `yaml:"healthz_port"`
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
			WorkDir:     "/tmp/iterion",
			Concurrency: 1,
			Heartbeat:   20 * time.Second,
			LockTTL:     60 * time.Second,
		},
		Server: ServerConfig{
			HealthzPort: 4891,
		},
		Metrics: MetricsConfig{
			Port: 9090,
		},
		Log: LogConfig{
			Format: LogFormatHuman,
			Level:  "info",
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

	if c.Server.HealthzPort < 1 || c.Server.HealthzPort > 65535 {
		return fmt.Errorf("ITERION_HEALTHZ_PORT %d invalid (want 1-65535)", c.Server.HealthzPort)
	}
	if c.Metrics.Port < 1 || c.Metrics.Port > 65535 {
		return fmt.Errorf("ITERION_METRICS_PORT %d invalid (want 1-65535)", c.Metrics.Port)
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

	switch c.Sandbox.Default {
	case "", "none", "auto":
	default:
		return fmt.Errorf("ITERION_SANDBOX_DEFAULT %q invalid (want \"\", \"none\", or \"auto\")", c.Sandbox.Default)
	}

	return nil
}
