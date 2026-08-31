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

	lookupString("ITERION_NATS_URL", &cfg.NATS.URL)
	lookupString("ITERION_NATS_STREAM", &cfg.NATS.Stream)
	lookupString("ITERION_NATS_KV_BUCKET", &cfg.NATS.KVBucket)
	lookupString("ITERION_NATS_DLQ_STREAM", &cfg.NATS.DLQStream)
	if err := lookupInt("ITERION_NATS_STREAM_REPLICAS", &cfg.NATS.StreamReplicas); err != nil {
		return err
	}
	if err := lookupInt("ITERION_NATS_MAX_ACK_PENDING", &cfg.NATS.MaxAckPending); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_NATS_ACK_WAIT", &cfg.NATS.AckWait); err != nil {
		return err
	}
	if err := lookupInt("ITERION_NATS_MAX_DELIVER", &cfg.NATS.MaxDeliver); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_NATS_MAX_AGE", &cfg.NATS.MaxAge); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_NATS_DLQ_MAX_AGE", &cfg.NATS.DLQMaxAge); err != nil {
		return err
	}
	if err := lookupInt("ITERION_NATS_MAX_PAYLOAD", &cfg.NATS.MaxPayload); err != nil {
		return err
	}

	lookupString("ITERION_MONGO_URI", &cfg.Mongo.URI)
	lookupString("ITERION_MONGO_DB", &cfg.Mongo.DB)
	if err := lookupInt("ITERION_MONGO_EVENTS_TTL_DAYS", &cfg.Mongo.EventsTTLDays); err != nil {
		return err
	}

	// Redis/Valkey — distributed ephemeral state (Sentinel HA or single node).
	lookupString("ITERION_REDIS_URL", &cfg.Redis.URL)
	if v, ok := lookup("ITERION_REDIS_SENTINEL_ADDRS"); ok {
		cfg.Redis.SentinelAddrs = splitCSV(v)
	}
	lookupString("ITERION_REDIS_MASTER_NAME", &cfg.Redis.MasterName)
	lookupString("ITERION_REDIS_PASSWORD", &cfg.Redis.Password)
	lookupString("ITERION_REDIS_SENTINEL_PASSWORD", &cfg.Redis.SentinelPassword)

	lookupString("ITERION_S3_ENDPOINT", &cfg.S3.Endpoint)
	lookupString("ITERION_S3_REGION", &cfg.S3.Region)
	lookupString("ITERION_S3_BUCKET", &cfg.S3.Bucket)
	lookupString("ITERION_S3_ACCESS_KEY_ID", &cfg.S3.AccessKeyID)
	lookupString("ITERION_S3_SECRET_ACCESS_KEY", &cfg.S3.SecretAccessKey)
	if err := lookupBool("ITERION_S3_USE_PATH_STYLE", &cfg.S3.UsePathStyle); err != nil {
		return err
	}

	lookupString("ITERION_RUNNER_WORKDIR", &cfg.Runner.WorkDir)
	if err := lookupInt("ITERION_RUNNER_CONCURRENCY", &cfg.Runner.Concurrency); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_HEARTBEAT_INTERVAL", &cfg.Runner.Heartbeat); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_LOCK_TTL", &cfg.Runner.LockTTL); err != nil {
		return err
	}
	lookupString("ITERION_RUNNER_DRAIN_MODE", &cfg.Runner.DrainMode)
	if err := lookupDuration("ITERION_RUNNER_DRAIN_TIMEOUT", &cfg.Runner.DrainTimeout); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_RUNNER_SCHEMA_MISMATCH_DELAY", &cfg.Runner.SchemaMismatchDelay); err != nil {
		return err
	}

	delay, err := ShutdownDelayFromEnv(cfg.Server.ShutdownDelay)
	if err != nil {
		return err
	}
	cfg.Server.ShutdownDelay = delay
	teardown, err := ShutdownTeardownFromEnv(cfg.Server.ShutdownTeardown)
	if err != nil {
		return err
	}
	cfg.Server.ShutdownTeardown = teardown
	if err := lookupInt("ITERION_METRICS_PORT", &cfg.Metrics.Port); err != nil {
		return err
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
	if v, ok := lookup("ITERION_SANDBOX_HOST_STATE"); ok {
		cfg.Sandbox.HostState = strings.ToLower(v)
	}
	if v, ok := lookup("ITERION_SANDBOX_OVERRIDE"); ok {
		cfg.Sandbox.Override = strings.ToLower(v)
	}

	lookupString("ITERION_JWT_SECRET", &cfg.Auth.JWTSecret)
	lookupString("ITERION_SECRETS_KEY", &cfg.Auth.SecretsKey)
	if err := lookupDuration("ITERION_ACCESS_TTL", &cfg.Auth.AccessTTL); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_REFRESH_TTL", &cfg.Auth.RefreshTTL); err != nil {
		return err
	}
	if v, ok := lookup("ITERION_BOOTSTRAP_ADMIN_EMAIL"); ok {
		cfg.Auth.BootstrapAdminEmail = strings.ToLower(strings.TrimSpace(v))
	}
	lookupString("ITERION_BOOTSTRAP_ADMIN_PASSWORD", &cfg.Auth.BootstrapAdminPassword)
	if v, ok := lookup("ITERION_SIGNUP_MODE"); ok {
		cfg.Auth.SignupMode = strings.ToLower(v)
	}
	if v, ok := lookup("ITERION_PUBLIC_URL"); ok {
		cfg.Auth.PublicURL = strings.TrimRight(v, "/")
	}
	lookupString("ITERION_COOKIE_DOMAIN", &cfg.Auth.CookieDomain)
	if err := lookupBool("ITERION_COOKIE_SECURE", &cfg.Auth.CookieSecure); err != nil {
		return err
	}
	if v, ok := lookup("ITERION_AUTH_TRUSTED_AUTO_LINK_PROVIDERS"); ok {
		cfg.Auth.TrustedAutoLinkProviders = splitCSV(v)
	}

	if err := lookupBool("ITERION_OIDC_GOOGLE_ENABLED", &cfg.Auth.OIDC.Google.Enabled); err != nil {
		return err
	}
	lookupString("ITERION_OIDC_GOOGLE_CLIENT_ID", &cfg.Auth.OIDC.Google.ClientID)
	lookupString("ITERION_OIDC_GOOGLE_CLIENT_SECRET", &cfg.Auth.OIDC.Google.ClientSecret)

	if err := lookupBool("ITERION_OIDC_GITHUB_ENABLED", &cfg.Auth.OIDC.GitHub.Enabled); err != nil {
		return err
	}
	lookupString("ITERION_OIDC_GITHUB_CLIENT_ID", &cfg.Auth.OIDC.GitHub.ClientID)
	lookupString("ITERION_OIDC_GITHUB_CLIENT_SECRET", &cfg.Auth.OIDC.GitHub.ClientSecret)
	lookupString("ITERION_OIDC_GITHUB_UNGRANTED_POLICY", &cfg.Auth.OIDC.GitHubUngrantedPolicy)

	if err := lookupBool("ITERION_OIDC_GENERIC_ENABLED", &cfg.Auth.OIDC.Generic.Enabled); err != nil {
		return err
	}
	if v, ok := lookup("ITERION_OIDC_GENERIC_ISSUER_URL"); ok {
		cfg.Auth.OIDC.Generic.IssuerURL = strings.TrimRight(v, "/")
	}
	lookupString("ITERION_OIDC_GENERIC_CLIENT_ID", &cfg.Auth.OIDC.Generic.ClientID)
	lookupString("ITERION_OIDC_GENERIC_CLIENT_SECRET", &cfg.Auth.OIDC.Generic.ClientSecret)
	lookupString("ITERION_OIDC_GENERIC_DISPLAY_NAME", &cfg.Auth.OIDC.Generic.DisplayName)
	if v, ok := lookup("ITERION_OIDC_GENERIC_SCOPES"); ok {
		cfg.Auth.OIDC.Generic.Scopes = splitCSV(v)
	}

	lookupString("ITERION_OAUTH_FORFAIT_ANTHROPIC_CLIENT_ID", &cfg.Auth.OAuthForfait.AnthropicClientID)
	lookupString("ITERION_OAUTH_FORFAIT_CODEX_CLIENT_ID", &cfg.Auth.OAuthForfait.CodexClientID)

	lookupString("ITERION_WEBPUSH_VAPID_PUBLIC_KEY", &cfg.WebPush.VAPIDPublicKey)
	lookupString("ITERION_WEBPUSH_VAPID_PRIVATE_KEY", &cfg.WebPush.VAPIDPrivateKey)
	lookupString("ITERION_WEBPUSH_SUBSCRIBER", &cfg.WebPush.Subscriber)

	lookupString("ITERION_ALERTS_WEBHOOK_URL", &cfg.Alerts.Webhook.URL)
	if err := lookupBool("ITERION_ALERTS_DESKTOP_ENABLED", &cfg.Alerts.Desktop.Enabled); err != nil {
		return err
	}
	if err := lookupDuration("ITERION_ALERTS_STALL_TIMEOUT", &cfg.Alerts.StallTimeout); err != nil {
		return err
	}

	return nil
}

// lookupInt overlays the env var named by key onto *dst when set and
// parses cleanly. An unset / empty env var is a no-op; a malformed one
// returns an error wrapped with the key name (callers in loadEnv
// propagate that as the function's error).
func lookupInt(key string, dst *int) error {
	v, ok := lookup(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = n
	return nil
}

// lookupDuration overlays the env var named by key onto *dst when set
// and parses cleanly. Semantics mirror lookupInt; the parser is
// time.ParseDuration.
func lookupDuration(key string, dst *time.Duration) error {
	v, ok := lookup(key)
	if !ok {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = d
	return nil
}

// Names of the two halves of a server's exit budget. They live here, with
// every other ITERION_* name, so there is exactly one parser per variable:
// Load() overlays them onto Config, and surfaces that do NOT run the full
// loader (the local studio) read the same values through
// ShutdownDelayFromEnv / ShutdownTeardownFromEnv rather than re-deriving
// the name and the accepted format.
const (
	EnvShutdownDelay    = "ITERION_SHUTDOWN_DELAY"
	EnvShutdownTeardown = "ITERION_SHUTDOWN_TEARDOWN"
)

// ShutdownDelayFromEnv resolves the lame-duck window, falling back to def
// when the variable is unset. It applies the SAME rule as Validate() —
// see checkShutdownDelay.
func ShutdownDelayFromEnv(def time.Duration) (time.Duration, error) {
	d, err := durationFromEnv(EnvShutdownDelay, def)
	if err != nil {
		return 0, err
	}
	if err := checkShutdownDelay(d); err != nil {
		return 0, err
	}
	return d, nil
}

// ShutdownTeardownFromEnv resolves the post-lame-duck teardown budget,
// falling back to def when the variable is unset. It applies the SAME
// rule as Validate() — see checkShutdownTeardown.
func ShutdownTeardownFromEnv(def time.Duration) (time.Duration, error) {
	d, err := durationFromEnv(EnvShutdownTeardown, def)
	if err != nil {
		return 0, err
	}
	if err := checkShutdownTeardown(d); err != nil {
		return 0, err
	}
	return d, nil
}

// durationFromEnv returns the parsed variable or def when it is unset. A
// malformed value is an error, never a silent fallback: an operator who
// wrote "5" instead of "5s" must see it now rather than discover months
// later that the window was never in effect.
func durationFromEnv(key string, def time.Duration) (time.Duration, error) {
	v, ok := lookup(key)
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q invalid (want a duration like 5s): %w", key, v, err)
	}
	return d, nil
}

// checkShutdownDelay and checkShutdownTeardown are each budget's SINGLE
// rule, shared by Validate() (the loader path, used by `iterion server`)
// and by the FromEnv helpers (surfaces that skip the loader — `iterion
// studio`, and `iterion server --mode local`, which routes to RunStudio
// and therefore never reaches Validate). Without that sharing the same
// chart would accept in `mode: local` what it rejects in `mode: cloud`.
func checkShutdownDelay(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("%s %s invalid (want >= 0)", EnvShutdownDelay, d)
	}
	return nil
}

func checkShutdownTeardown(d time.Duration) error {
	// Zero hands runs.Drain and http.Server.Shutdown an ALREADY-EXPIRED
	// context: in-flight runs never flip to failed_resumable and every
	// in-flight request is cut at once — the opposite of a graceful
	// shutdown, and silent.
	if d <= 0 {
		return fmt.Errorf("%s %s invalid (want > 0)", EnvShutdownTeardown, d)
	}
	return nil
}

// lookupBool overlays the env var named by key onto *dst when set and
// parses cleanly. Semantics mirror lookupInt; the parser is
// strconv.ParseBool (accepts 1/0/t/f/T/F/true/false/TRUE/FALSE/...).
func lookupBool(key string, dst *bool) error {
	v, ok := lookup(key)
	if !ok {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = b
	return nil
}

// lookupString overlays the env var named by key onto *dst when set
// (and non-empty, per lookup's contract). Unlike lookupInt/Bool/Duration
// there is no parser to fail, so this returns no error — callers in
// loadEnv invoke it for its side effect only.
func lookupString(key string, dst *string) {
	if v, ok := lookup(key); ok {
		*dst = v
	}
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
