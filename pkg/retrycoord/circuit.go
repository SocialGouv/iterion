// Package retrycoord contains the policy shared by runner replicas when
// deciding whether a retry wave should be admitted. Durable state lives in an
// optional store capability; the package keeps the key/config rules in one
// place so launch surfaces cannot silently diverge.
package retrycoord

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	EnvThreshold     = "ITERION_RETRY_CIRCUIT_THRESHOLD"
	EnvCooldown      = "ITERION_RETRY_CIRCUIT_COOLDOWN"
	DefaultThreshold = 3
	DefaultCooldown  = 15 * time.Minute
)

// Config bounds a shared retry circuit. Threshold is the number of failures
// across runs before opening; cooldown is the minimum quiet period.
type Config struct {
	Threshold int
	Cooldown  time.Duration
}

func FromEnv() Config {
	c := Config{Threshold: DefaultThreshold, Cooldown: DefaultCooldown}
	if raw := strings.TrimSpace(os.Getenv(EnvThreshold)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			c.Threshold = n
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvCooldown)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			c.Cooldown = d
		}
	}
	return c
}

// Key derives a stable, non-secret breaker identity. WorkflowHash is
// preferred because changing the workflow starts a fresh contract; legacy
// runs without a hash use the workflow name.
func Key(run *store.Run) string {
	if run == nil {
		return ""
	}
	if h := strings.TrimSpace(run.WorkflowHash); h != "" {
		return "workflow:" + h
	}
	if n := strings.TrimSpace(run.WorkflowName); n != "" {
		return "workflow-name:" + n
	}
	return ""
}

// RecordFailure updates the durable breaker when the store supports it. A
// local/filesystem store simply returns nil so existing local retry semantics
// remain unchanged.
func RecordFailure(ctx context.Context, s store.RunStore, key, runID string, now time.Time, cfg Config) (*store.RetryCircuitState, error) {
	c := store.AsRetryCircuitStore(s)
	if c == nil || key == "" {
		return nil, nil
	}
	return c.RecordRetryFailure(ctx, key, runID, now, cfg.Threshold, cfg.Cooldown)
}

func Open(ctx context.Context, s store.RunStore, key string, now time.Time) (*store.RetryCircuitState, error) {
	c := store.AsRetryCircuitStore(s)
	if c == nil || key == "" {
		return nil, nil
	}
	return c.RetryCircuitOpen(ctx, key, now)
}

func RecordSuccess(ctx context.Context, s store.RunStore, key string, now time.Time) error {
	c := store.AsRetryCircuitStore(s)
	if c == nil || key == "" {
		return nil
	}
	return c.RecordRetrySuccess(ctx, key, now)
}
