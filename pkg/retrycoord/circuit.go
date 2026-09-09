// Package retrycoord contains the policy shared by runner replicas when
// deciding whether a retry wave should be admitted. Durable state lives in an
// optional store capability; the package keeps the key/config rules in one
// place so launch surfaces cannot silently diverge.
package retrycoord

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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

// FromEnv resolves the circuit bounds, falling back to the package defaults.
//
// A value it cannot use keeps the default and SAYS SO on stderr, once per
// process. Silence was the operability hole: `ITERION_RETRY_CIRCUIT_COOLDOWN=15`
// (no unit) or a typo'd threshold reads as configured, deploys clean, and
// runs the default forever with nothing in the logs — on a knob whose whole
// purpose is to be tuned against a live provider. The repo already fails this
// way loudly for ITERION_BUDGET_EXIT_GRACE; this matches it.
func FromEnv() Config {
	c, problems := configFromEnv(os.Getenv)
	if len(problems) > 0 {
		envWarnOnce.Do(func() {
			for _, p := range problems {
				fmt.Fprintf(os.Stderr, "iterion: %s\n", p)
			}
		})
	}
	return c
}

var envWarnOnce sync.Once

// configFromEnv is FromEnv's pure half: it returns the resolved config plus
// one human-readable line per value it had to reject, so the rejection is
// testable without capturing stderr or fighting a sync.Once.
func configFromEnv(getenv func(string) string) (Config, []string) {
	c := Config{Threshold: DefaultThreshold, Cooldown: DefaultCooldown}
	var problems []string
	if raw := strings.TrimSpace(getenv(EnvThreshold)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			c.Threshold = n
		} else {
			problems = append(problems, fmt.Sprintf(
				"%s=%q is not a positive integer — keeping the default threshold of %d",
				EnvThreshold, raw, DefaultThreshold))
		}
	}
	if raw := strings.TrimSpace(getenv(EnvCooldown)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			c.Cooldown = d
		} else {
			problems = append(problems, fmt.Sprintf(
				"%s=%q is not a positive Go duration (e.g. 15m) — keeping the default cooldown of %s",
				EnvCooldown, raw, DefaultCooldown))
		}
	}
	return c, problems
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

// RecordSuccess clears the streak once a run of this workflow revision has
// completed, so a recovered provider does not keep a stale breaker armed.
// Same nil-store degradation as RecordFailure.
//
// There is deliberately no Open() wrapper here: nothing in the runner GATES
// on the breaker, it only lets the cooldown push a wake-up later (see
// armUsageWindowRetry). Reading the breaker to refuse admission would be a
// different contract, and the wrapper for it belongs to the change that
// needs it — store.RetryCircuitStore already exposes RetryCircuitOpen.
func RecordSuccess(ctx context.Context, s store.RunStore, key string, now time.Time) error {
	c := store.AsRetryCircuitStore(s)
	if c == nil || key == "" {
		return nil
	}
	return c.RecordRetrySuccess(ctx, key, now)
}
