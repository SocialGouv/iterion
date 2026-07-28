package retrypolicy

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables. The ITERION_RETRY_* set is the machine-wide
// DEFAULT layer (lowest priority, overridable by a bot or a schedule); the
// ITERION_CLOUD_RETRY_* set is the platform CEILING (applied last, can only
// lower). Keeping them as two distinct prefixes is deliberate: an operator
// raising a default and a platform capping a tenant are opposite intents,
// and one variable cannot serve both.
const (
	EnvUsageWindow = "ITERION_RETRY_USAGE_WINDOW"
	EnvMaxAttempts = "ITERION_RETRY_MAX_ATTEMPTS"
	EnvMaxWait     = "ITERION_RETRY_MAX_WAIT"
	EnvJitter      = "ITERION_RETRY_JITTER"

	EnvCeilingMaxAttempts = "ITERION_CLOUD_RETRY_MAX_ATTEMPTS"
	EnvCeilingMaxWait     = "ITERION_CLOUD_RETRY_MAX_WAIT"
)

// FromEnv builds the machine-wide default layer. Unset or unparseable
// values are left empty so the package defaults apply — a typo in an env
// var must not silently disable retries, and Validate is not reachable from
// here (there is no request to reject).
func FromEnv() Policy {
	return Policy{
		UsageWindow: strings.TrimSpace(os.Getenv(EnvUsageWindow)),
		MaxAttempts: envPositiveInt(EnvMaxAttempts),
		MaxWait:     envDuration(EnvMaxWait),
		Jitter:      envDuration(EnvJitter),
	}
}

// CeilingFromEnv builds the platform ceiling. A zero field means "no
// ceiling on this dimension".
func CeilingFromEnv() Ceiling {
	c := Ceiling{MaxAttempts: envPositiveInt(EnvCeilingMaxAttempts)}
	if raw := envDuration(EnvCeilingMaxWait); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			c.MaxWait = d
		}
	}
	return c
}

func envPositiveInt(name string) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// envDuration returns the raw value only when it parses as a positive
// duration, so an unparseable env var falls through to the default instead
// of failing a Validate call far from its source.
func envDuration(name string) string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return ""
	}
	if d, err := time.ParseDuration(raw); err != nil || d < 0 {
		return ""
	}
	return raw
}
