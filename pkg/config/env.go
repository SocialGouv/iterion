package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// loadEnv overlays ITERION_* env vars onto cfg. An empty / unset env
// var is a no-op; only set, non-empty values override. This means
// running iterion without any new env vars set leaves Defaults() and
// the YAML file untouched.
func loadEnv(cfg *Config) error {
	if v, ok := lookup("ITERION_MODE"); ok {
		cfg.Mode = Mode(v)
	}

	if v, ok := lookup("ITERION_NATS_URL"); ok {
		cfg.NATS.URL = v
	}
	if v, ok := lookup("ITERION_NATS_STREAM"); ok {
		cfg.NATS.Stream = v
	}
	if v, ok := lookup("ITERION_NATS_KV_BUCKET"); ok {
		cfg.NATS.KVBucket = v
	}
	if v, ok := lookup("ITERION_NATS_DLQ_STREAM"); ok {
		cfg.NATS.DLQStream = v
	}

	if v, ok := lookup("ITERION_MONGO_URI"); ok {
		cfg.Mongo.URI = v
	}
	if v, ok := lookup("ITERION_MONGO_DB"); ok {
		cfg.Mongo.DB = v
	}
	if v, ok := lookup("ITERION_MONGO_EVENTS_TTL_DAYS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("ITERION_MONGO_EVENTS_TTL_DAYS: %w", err)
		}
		cfg.Mongo.EventsTTLDays = n
	}

	if v, ok := lookup("ITERION_S3_ENDPOINT"); ok {
		cfg.S3.Endpoint = v
	}
	if v, ok := lookup("ITERION_S3_REGION"); ok {
		cfg.S3.Region = v
	}
	if v, ok := lookup("ITERION_S3_BUCKET"); ok {
		cfg.S3.Bucket = v
	}
	if v, ok := lookup("ITERION_S3_ACCESS_KEY_ID"); ok {
		cfg.S3.AccessKeyID = v
	}
	if v, ok := lookup("ITERION_S3_SECRET_ACCESS_KEY"); ok {
		cfg.S3.SecretAccessKey = v
	}
	if v, ok := lookup("ITERION_S3_USE_PATH_STYLE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ITERION_S3_USE_PATH_STYLE: %w", err)
		}
		cfg.S3.UsePathStyle = b
	}

	if v, ok := lookup("ITERION_RUNNER_WORKDIR"); ok {
		cfg.Runner.WorkDir = v
	}
	if v, ok := lookup("ITERION_RUNNER_CONCURRENCY"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("ITERION_RUNNER_CONCURRENCY: %w", err)
		}
		cfg.Runner.Concurrency = n
	}
	if v, ok := lookup("ITERION_HEARTBEAT_INTERVAL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("ITERION_HEARTBEAT_INTERVAL: %w", err)
		}
		cfg.Runner.Heartbeat = d
	}
	if v, ok := lookup("ITERION_LOCK_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("ITERION_LOCK_TTL: %w", err)
		}
		cfg.Runner.LockTTL = d
	}

	if v, ok := lookup("ITERION_HEALTHZ_PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("ITERION_HEALTHZ_PORT: %w", err)
		}
		cfg.Server.HealthzPort = n
	}
	if v, ok := lookup("ITERION_METRICS_PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("ITERION_METRICS_PORT: %w", err)
		}
		cfg.Metrics.Port = n
	}
	if v, ok := lookup("ITERION_LOG_FORMAT"); ok {
		cfg.Log.Format = LogFormat(strings.ToLower(v))
	}
	if v, ok := lookup("ITERION_LOG_LEVEL"); ok {
		cfg.Log.Level = strings.ToLower(v)
	}

	if v, ok := lookup("ITERION_SANDBOX_DEFAULT"); ok {
		cfg.Sandbox.Default = strings.ToLower(v)
	}

	if v, ok := lookup("ITERION_JWT_SECRET"); ok {
		cfg.Auth.JWTSecret = v
	}
	if v, ok := lookup("ITERION_SECRETS_KEY"); ok {
		cfg.Auth.SecretsKey = v
	}
	if v, ok := lookup("ITERION_ACCESS_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("ITERION_ACCESS_TTL: %w", err)
		}
		cfg.Auth.AccessTTL = d
	}
	if v, ok := lookup("ITERION_REFRESH_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("ITERION_REFRESH_TTL: %w", err)
		}
		cfg.Auth.RefreshTTL = d
	}
	if v, ok := lookup("ITERION_BOOTSTRAP_ADMIN_EMAIL"); ok {
		cfg.Auth.BootstrapAdminEmail = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := lookup("ITERION_SIGNUP_MODE"); ok {
		cfg.Auth.SignupMode = strings.ToLower(v)
	}
	if v, ok := lookup("ITERION_PUBLIC_URL"); ok {
		cfg.Auth.PublicURL = strings.TrimRight(v, "/")
	}
	if v, ok := lookup("ITERION_COOKIE_DOMAIN"); ok {
		cfg.Auth.CookieDomain = v
	}
	if v, ok := lookup("ITERION_COOKIE_SECURE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ITERION_COOKIE_SECURE: %w", err)
		}
		cfg.Auth.CookieSecure = b
	}

	if v, ok := lookup("ITERION_OIDC_GOOGLE_ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ITERION_OIDC_GOOGLE_ENABLED: %w", err)
		}
		cfg.Auth.OIDC.Google.Enabled = b
	}
	if v, ok := lookup("ITERION_OIDC_GOOGLE_CLIENT_ID"); ok {
		cfg.Auth.OIDC.Google.ClientID = v
	}
	if v, ok := lookup("ITERION_OIDC_GOOGLE_CLIENT_SECRET"); ok {
		cfg.Auth.OIDC.Google.ClientSecret = v
	}

	if v, ok := lookup("ITERION_OIDC_GITHUB_ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ITERION_OIDC_GITHUB_ENABLED: %w", err)
		}
		cfg.Auth.OIDC.GitHub.Enabled = b
	}
	if v, ok := lookup("ITERION_OIDC_GITHUB_CLIENT_ID"); ok {
		cfg.Auth.OIDC.GitHub.ClientID = v
	}
	if v, ok := lookup("ITERION_OIDC_GITHUB_CLIENT_SECRET"); ok {
		cfg.Auth.OIDC.GitHub.ClientSecret = v
	}

	if v, ok := lookup("ITERION_OIDC_GENERIC_ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ITERION_OIDC_GENERIC_ENABLED: %w", err)
		}
		cfg.Auth.OIDC.Generic.Enabled = b
	}
	if v, ok := lookup("ITERION_OIDC_GENERIC_ISSUER_URL"); ok {
		cfg.Auth.OIDC.Generic.IssuerURL = strings.TrimRight(v, "/")
	}
	if v, ok := lookup("ITERION_OIDC_GENERIC_CLIENT_ID"); ok {
		cfg.Auth.OIDC.Generic.ClientID = v
	}
	if v, ok := lookup("ITERION_OIDC_GENERIC_CLIENT_SECRET"); ok {
		cfg.Auth.OIDC.Generic.ClientSecret = v
	}
	if v, ok := lookup("ITERION_OIDC_GENERIC_DISPLAY_NAME"); ok {
		cfg.Auth.OIDC.Generic.DisplayName = v
	}
	if v, ok := lookup("ITERION_OIDC_GENERIC_SCOPES"); ok {
		cfg.Auth.OIDC.Generic.Scopes = splitCSV(v)
	}

	if v, ok := lookup("ITERION_OAUTH_FORFAIT_ANTHROPIC_CLIENT_ID"); ok {
		cfg.Auth.OAuthForfait.AnthropicClientID = v
	}
	if v, ok := lookup("ITERION_OAUTH_FORFAIT_CODEX_CLIENT_ID"); ok {
		cfg.Auth.OAuthForfait.CodexClientID = v
	}

	return nil
}

// splitCSV trims and splits a comma-separated env var value, dropping
// empty entries. Used by env vars like ITERION_OIDC_GENERIC_SCOPES.
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// lookup returns (value, true) only when the env var is both set and
// non-empty. An explicit empty string is treated as "not set" so that
// `unset FOO` and `FOO=` behave identically; this matches the precedence
// rule in the plan ("env > yaml > defaults" means a present-but-empty
// env var is a no-op, not an override-to-empty).
func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	if v == "" {
		return "", false
	}
	return v, true
}
